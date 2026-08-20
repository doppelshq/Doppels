# Schema discovery

Cómo conocer el contrato **real** al escribir manifests. No inventar campos.

## Dónde está la verdad

Schemas y ejemplos viven en el repo OSS **`doppelshq/doppels`**, no en un repo
aparte:

| Recurso | URL |
|---|---|
| Capability schema | https://raw.githubusercontent.com/doppelshq/doppels/main/schemas/capability.schema.json |
| Recipe schema | https://raw.githubusercontent.com/doppelshq/doppels/main/schemas/recipe.schema.json |
| Common | https://raw.githubusercontent.com/doppelshq/doppels/main/schemas/common.schema.json |
| Árbol schemas | https://github.com/doppelshq/doppels/tree/main/schemas |
| Demo greet | https://github.com/doppelshq/doppels/tree/main/examples/demo/.doppels |

También en esta skill: `examples/capability-simple.yaml` +
`examples/recipe-simple.yaml` (copia del demo `greet`).

## Lo que NO existe

- No hay `doppel schema list` / `doppel schema describe`.
- El binario es **`doppels`** (con **s**).
- No existe un generador `doppels freeze`.

Si necesitas el schema: **fetch del raw JSON** arriba (o clona el repo) y
léelo. Luego valida siempre con la CLI.

## Validar (fuente operativa)

```bash
doppels validate
# o un archivo:
doppels validate -f .doppels/capabilities/<name>.yaml
doppels validate -f .doppels/recipes/<name>.yaml
```

`doppels validate` es la autoridad: si el schema en GitHub y la CLI discrepan,
gana lo que diga la CLI instalada.

## apiVersion

```yaml
apiVersion: doppels.so/v1alpha1
```

Una revisión publicada del schema no se muta in-place; breaking → nueva
versión (`v1alpha2`, …).

## Durante freeze

1. Copiar/adaptar el ejemplo `greet` de esta skill (o `examples/demo`).
2. Si dudas de un campo, abrir el JSON Schema raw.
3. `doppels validate` tras cada cambio material.
4. `doppels run capability/<name> --yes` (con `--input` si hace falta).
