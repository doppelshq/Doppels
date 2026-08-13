# Anatomía de un manifest

Estructura canónica de Capability y Recipe en Doppel.

## Capability

Define el contrato público: qué hace, qué necesita, qué devuelve.

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: <identificador estable>
  version: <semver>
  displayName: <título legible>
  summary: <una frase>
  description: <párrafo explicativo>
  impact: low | medium | high | critical
  tags: [<etiquetas libres>]
  labels:
    <clave>: <valor>

documentation:
  readme: <path relativo al README>

inputs:
  <nombre>:
    type: string | number | boolean | enum | object | array
    description: <texto>
    required: true | false
    default: <valor>
    enum: [<valores>]
    # según tipo: minimum, maximum, pattern, etc.

outputs:
  <nombre>:
    type: string | number | boolean | artifact | object
    description: <texto>
    mediaType: <MIME si artifact>
```

### Campos obligatorios

- `apiVersion`
- `kind: Capability`
- `metadata.name`
- `metadata.version`
- `inputs` (al menos uno declarado o `{}`)
- `outputs` (al menos uno declarado o `{}`)

### Campos opcionales pero recomendados

- `metadata.summary`
- `metadata.description`
- `metadata.impact`
- `documentation.readme`

## Recipe

Define cómo se ejecuta localmente una Capability.

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: <identificador estable>
  version: <semver>
  displayName: <título legible>
  summary: <una frase>
  description: <párrafo>
  impact: low | medium | high | critical

provides: [<lista de Capabilities que implementa>]

runtime: shell

requires:
  commands:
    - <binario>
    - name: <binario>
      version: "<rango semver>"
  hostEnv:
    - <nombre de variable>
  files:
    - <path relativo>

defaults:
  timeout: 15m
  approval: never | required
  workingDirectory: .

env:
  <nombre>: <valor o expresión>

steps:
  - id: <único en el Recipe>
    name: <legible>
    approval: never | required
    env:
      <nombre>: <expresión o referencia>
    run:
      shell: sh | bash
      script: |
        <script multilínea>
    produces:
      <clave>:
        file: <path relativo>
        # o
        env: <nombre de variable>

returns:
  <clave>: "{{ steps.<id>.<clave> }}"
```

### Campos obligatorios

- `apiVersion`
- `kind: Recipe`
- `metadata.name`
- `metadata.version`
- `provides: [<al menos una Capability>]`
- `runtime`
- `steps: [<al menos uno>]`

### Reglas clave

- Una Recipe implementa **una o varias Capabilities** mediante `provides`.
- Los Steps son **secuenciales** según el orden en el YAML.
- Cada Step corre en **shell nuevo**; no comparte estado entre Steps.
- Los outputs de Steps previos se referencian como
  `{{ steps.<id>.<clave> }}`.
- Secretos vía `from: host_env`; nunca valores literales.

## Ejemplos completos

Ver `examples/capability-simple.yaml` y `examples/recipe-simple.yaml`.