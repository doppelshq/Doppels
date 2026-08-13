# Schema discovery

Cómo encontrar y usar los schemas de Doppel al escribir manifests.

## Localización de schemas

Los schemas viven en el repo público `doppels/schemas`:

- https://github.com/doppels/schemas

Estructura:

```text
schemas/
├── capability.schema.json
├── recipe.schema.json
├── request.schema.json
├── run.schema.json
├── run-event.schema.json
├── share.schema.json
├── space.schema.json
└── common.schema.json
```

## Descubrir schemas locales

```bash
doppel schema list
```

Muestra los schemas conocidos por la CLI y su versión (`apiVersion`).

```bash
doppel schema describe capability
```

Imprime el schema en formato resumido o path local al archivo JSON Schema.

## Versionado

Los schemas se versionan por `apiVersion`:

```yaml
apiVersion: doppels.so/v1alpha1
```

Una versión publicada no se modifica; los cambios incompatibles crean
`v1alpha2`, `v1beta1`, etc.

Cada manifest declara `apiVersion` y debe coincidir con un schema conocido por
la CLI. Si la CLI no reconoce la versión, `doppels validate` falla.

## Uso durante freeze

1. Ejecutar `doppel schema describe capability` para conocer campos válidos.
2. Consultar `examples/` del repo de schemas para manifests completos.
3. Validar con `doppels validate` después de cada cambio.

## Tipos auxiliares

El schema `common.schema.json` define tipos reutilizables (`hostEnv`,
expresiones `{{ ... }}`, paths relativos POSIX).

## Referencias

- `examples/capability-simple.yaml` — Manifest mínimo.
- `examples/recipe-simple.yaml` — Recipe mínima.