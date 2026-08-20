# Anatomía de un manifest

Contrato **v1alpha1** real. Si esto choca con un schema viejo en tu cabeza,
gana `schemas/*.schema.json` + `doppels validate`.

## Flujo de datos (memorizar)

```text
inputs (Capability)
  → step.env: FOO: "{{ inputs.foo }}"
  → script: export OUT=… ; usa $FOO
  → produces: { key: { env: OUT } }   # o file: relative/path
  → returns: key: "{{ steps.<id>.key }}"
  → outputs (Capability) con el mismo nombre de clave
```

Reglas duras:

- Tipos de **input**: solo `string` | `integer` | `number` | `boolean`.
  `enum` es **propiedad** opcional del input, **no** un `type`.
- Tipos de **output**: `string` | `integer` | `number` | `boolean` | `artifact`.
  `object` / `array` **no** son válidos. Si `type: artifact` → `mediaType` **obligatorio**.
- `produces.env` debe ser nombre de variable **`^[A-Z_][A-Z0-9_]*$`** (MAYÚSCULAS).
- Interpolación útil de inputs: en **`steps[].env`**, no solo en paths.
- `requires.commands[].version` (si se declara) sigue regex estricto tipo
  `>=1.2.3` / `>1.0.0 <2.0.0` (semver con operador). No escribas prosa.

## Ejemplo mínimo completo (copiar)

Capability → `.doppels/capabilities/greet.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: greet
  version: 1.0.0
  displayName: Greet a person
  summary: Produce a greeting message.
  impact: low
  tags: [demo]

inputs:
  name:
    type: string
    description: Person to greet.
    placeholder: "e.g. Alice"
    required: true

outputs:
  message:
    type: string
    description: Generated greeting.
```

Recipe → `.doppels/recipes/greet.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: greet
  version: 1.0.0
  displayName: Greet (instant)
  summary: Instant greeting.
  impact: low

provides: [greet]
runtime: shell

requires:
  commands: [sh]

defaults:
  approval: never

steps:
  - id: greet
    name: Generate greeting
    env:
      NAME: "{{ inputs.name }}"
    run:
      shell: sh
      script: |
        export MESSAGE="Hello, $NAME"
        printf '%s\n' "$MESSAGE"
    produces:
      message:
        env: MESSAGE

returns:
  message: "{{ steps.greet.message }}"
```

Probar:

```bash
doppels validate
doppels run capability/greet --input name=Ada --yes
```

Mismos archivos en `references/examples/` y en
`examples/demo/.doppels/` del repo.

## Capability (campos)

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: <identificador>
  version: <semver>
  displayName: <título>
  summary: <una frase>
  impact: low | medium | high | critical
  tags: []

inputs:
  <nombre>:
    type: string | integer | number | boolean
    description: <texto>
    required: true | false
    default: <scalar del mismo tipo>
    enum: [<scalars>]          # opcional; no es un type
    placeholder: <texto>       # opcional (UI share)

outputs:
  <nombre>:
    type: string | integer | number | boolean | artifact
    description: <texto>
    mediaType: <MIME>          # obligatorio si type=artifact
```

Obligatorios: `apiVersion`, `kind`, `metadata.name`, `metadata.version`,
`inputs`, `outputs` (pueden ser `{}`).

## Recipe (campos)

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: <identificador>
  version: <semver>
  displayName: <título>
  summary: <frase>
  impact: low | medium | high | critical

provides: [<capability names>]
runtime: shell

requires:
  commands:
    - sh
    - name: node
      version: ">=22.0.0"
  hostEnv: [TOKEN_NAME]
  files: [relative/path]

defaults:
  approval: never | required
  timeout: 15m
  workingDirectory: .          # relativo al root del Space

steps:
  - id: <id>
    name: <legible>
    env:
      FOO: "{{ inputs.foo }}"
    run:
      shell: sh
      script: |
        export RESULT="…"
    produces:
      out:
        env: RESULT
      # o archivo:
      # report:
      #   file: report.json

returns:
  out: "{{ steps.<id>.out }}"
```

### Working directory y artefactos

- El **cwd del Run** es la **raíz del Space** (el directorio que contiene
  `.doppels/`), no el cwd del agente ni `runs/`.
- `produces.file: stories.json` crea/escribe `stories.json` **en esa raíz**
  (ensucia el working tree). Prefiere un subdir (`out/stories.json`) o limpia
  en el script si no debe quedar trackeado.
- Paths de `file:`: relativos POSIX (`/`), nunca absolutos del host.

### Inputs en `doppels run`

- `--input key=value` fija un input.
- Si el Capability declara `default` y no pasas `--input` para esa clave, la
  CLI usa el default.
- Si `required: true` y no hay valor ni default → el run falla en validación.

### Secretos

```yaml
env:
  DEPLOY_TOKEN:
    from: host_env
    name: DEPLOY_TOKEN
```

Nunca valores literales de secretos en el YAML.
