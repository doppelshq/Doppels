# Contratos de Doppels

`schemas/` es la fuente ejecutable del modelo del MVP. Los documentos usan
YAML o JSON, se validan con JSON Schema Draft 2020-12 y comparten
`apiVersion: doppels.so/v1alpha1`.

El YAML aceptado es un subset determinista compatible con el modelo de datos
JSON. Los números usan la gramática numérica de JSON; tags, timestamps,
anchors, aliases y merge keys no están permitidos. Tokens plain ambiguos entre
YAML 1.1 y 1.2 —por ejemplo `yes`, `on`, `~`, fechas o sexagesimales— deben
entrecomillarse cuando se quieran usar como texto. Los comments y block scalars
para scripts multiline sí están permitidos.

## Modelo mínimo

```text
Organization → Space
                 ↓
Capability ← Recipe
     ↓
  Request → Run → RunEvent

Share → Request
```

- `Capability` define exclusivamente el contrato público de inputs y outputs.
- `Recipe` define cómo cumplir una o varias Capabilities mediante `provides`.
- `Space` es el límite mutable de configuración y datos dentro de la
  Organization seleccionada por el Context local de la CLI.
- `Request` fija una solicitud e inputs ya validados.
- `Run` registra un intento automático o humano.
- `RunEvent` añade evidencia inmutable al intento.
- `Share` expone temporalmente una Capability; la Recipe, si existe, permanece
  en el host que comparte.

`Playbook` compondrá Capabilities en el futuro. No tiene schema ni runtime en
esta versión.

## Space

```yaml
apiVersion: doppels.so/v1alpha1
kind: Space

metadata:
  name: platform
  displayName: Platform
  labels:
    environment: production
```

Space no lleva `metadata.version`: representa estado deseado mutable, no una
definición publicable. Tampoco contiene Organization; `doppels context` aporta
el destino para que el mismo manifiesto pueda aplicarse en distintas
Organizations. Capabilities y Recipes se descubren por convención y no se
enumeran de nuevo dentro del Space.

## Capability

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: release-build
  version: 1.0.0
  displayName: Generar release

inputs:
  version:
    type: string
    required: true

outputs:
  archive:
    type: artifact
    mediaType: application/gzip
  checksum:
    type: string
```

Todos los outputs declarados forman parte del resultado obligatorio. Una
Capability puede solicitarse aunque todavía no exista Recipe: una persona
entrega esos outputs mediante un Run humano.

## Recipe

Una Recipe no repite inputs. `provides` relaciona la implementación con una o
varias Capabilities y el lockfile fijará las revisiones y digests validados
conjuntamente.

### Runtime `shell`

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: release-build-with-tar
  version: 1.0.0

provides: [release-build]
runtime: shell

requires:
  commands: [tar, sha256sum]

defaults:
  approval: never

steps:
  - id: build
    name: Empaquetar release
    env:
      VERSION: "{{ inputs.version }}"
    run:
      shell: sh
      script: |
        tar -czf "release-$VERSION.tgz" dist/
        export CHECKSUM="$(sha256sum "release-$VERSION.tgz" | cut -d ' ' -f 1)"
    produces:
      archive:
        file: "release-{{ inputs.version }}.tgz"
      checksum:
        env: CHECKSUM

returns:
  archive: "{{ steps.build.archive }}"
  checksum: "{{ steps.build.checksum }}"
```

Los Steps se ejecutan en el orden declarado y cada uno abre un proceso aislado
de `sh` o `bash`. `produces.file` captura un artefacto y `produces.env` el valor
final de una variable exportada. `returns` debe cubrir los outputs de todas las
Capabilities proporcionadas. `stdout` y `stderr` son logs, no resultados.

Una variable exportada llega al runtime como texto. Al asignarla mediante
`returns`, se convierte según el output de Capability: `string` conserva el
valor; `integer` exige decimal base 10 dentro del rango JSON portable
`[-9007199254740991, 9007199254740991]`; `number` exige un número JSON finito;
si su valor es matemáticamente integral se aplica el mismo rango portable; y
`boolean` acepta únicamente `true` o `false`. No se recortan espacios ni se
aplican conversiones tolerantes.

Todos los paths declarados por un manifiesto usan sintaxis POSIX relativa al
workspace: `/`, prefijos de unidad como `C:`, `\\` y segmentos `..` están
prohibidos. Esto conserva el mismo contrato en macOS, Linux y Windows.

Las expresiones usan `{{ ... }}` dentro de strings YAML entre comillas. Solo se
admiten `inputs.<name>` y resultados ya disponibles mediante
`steps.<step>.<result>`. En runtime `shell` se usan en `env`, paths declarativos
y `returns`, nunca dentro de `run.script`; el script consume variables de
entorno normales para evitar inyección de shell.

La aprobación nunca se infiere: cada Step debe resolverla desde
`defaults.approval` o desde su propio `approval`.

### Runtime `manual`

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: service-status-manual
  version: 1.0.0

provides: [service-status]
runtime: manual

procedure:
  readme: ./service-status-runbook.md

evidence:
  verification-notes:
    type: string
```

El procedimiento explica el trabajo. Los outputs se recogen directamente
según el contrato de Capability y `evidence` añade la prueba específica que
exige el procedimiento.

## Share y tiempo real

`ShareCreated` devuelve por única vez la URL pública y la credencial privada
de la CLI. El `Share` persistido contiene un snapshot de Capability, pero no el
script ni el token en texto plano. Su referencia a Recipe es opcional para
permitir cumplimiento humano.

`capabilityRevision` fija nombre, versión, SHA-256 del manifiesto y SHA-256 del
bundle transitivo del schema. Request y Run copian esa referencia exacta; un
digest falso o una reutilización de versión con contenido diferente es un
error.

`ShareMessage` es el envelope idempotente del Phoenix Channel. Request, Run y
RunEvent se persisten antes de emitir sus confirmaciones; el WebSocket no es la
cola ni la fuente de verdad. La CLI sube artefactos por HTTP autenticado y los
returns públicos se descargan mediante URLs temporales firmadas.

## Validación

```console
npm ci
npm run validate
node scripts/schema-bundle.mjs
```

El validador comprueba schemas, ejemplos, referencias Capability–Recipe,
expresiones, orden de Steps, approvals, tipos de `returns`, inputs de Request y
coherencia básica de Request → Run → RunEvent. También calcula los SHA-256
reales de manifiestos y bundles de schemas usados por los ejemplos. El último
comando imprime los digests transitivos de Capability y Recipe para alinear
otros runtimes sin reimplementar el cálculo a mano.

Ejemplos relevantes:

- `examples/release-preview.capability.yaml` y `release-preview.yaml`: varios
  artefactos, valor escalar, requisitos, secreto del host y aprobación local.
- `examples/service-status.manual.yaml`: Recipe humana.
- `examples/manual-run-succeeded.json`: outputs y evidencia de cumplimiento
  manual persistidos de forma separada.
- `examples/no-recipe-*.json`: cumplimiento humano ad hoc de una Capability sin
  Recipe ni Node.
- `examples/share-created.json` y `share-request-available.json`: bootstrap y
  notificación de una sesión compartida.
- `examples/share-run-*.json`: confirmaciones submitted/recorded aisladas por
  Share para Run y RunEvent.

La política de publicación inmutable está en [VERSIONING.md](VERSIONING.md).
