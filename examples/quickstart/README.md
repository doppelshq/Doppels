# Quickstart

Este Space local ejercita la vertical completa sin tocar infraestructura externa:

```console
task build:cli
cd examples/quickstart
../../bin/doppels validate
../../bin/doppels run capability/greet --input name=Ada
```

`greet-with-shell` produces a scalar and an artifact for validate/run/share.
`release-pipeline` is a four-Step Recipe (≈1–3s each) for timeline demos:

```console
../../bin/doppels run capability/release-pipeline --input version=1.2.3
```

`manual-review` has no Recipe: fulfill manually with `--output` / `--evidence`.

Por ejemplo, tras crear `manual-report.txt` y `review-proof.txt`:

```console
../../bin/doppels run capability/manual-review \
  --input subject=ACME \
  --output decision=approved \
  --output report=manual-report.txt \
  --evidence proof=@review-proof.txt
```

Con el Cloud local ejecutándose desde la raíz (`task dev:cloud`):

```console
DOPPELS_API_TOKEN=local-development-token \
DOPPELS_IDENTITY=local-developer \
../../bin/doppels share capability/greet \
  --server http://localhost:4000 \
  --expires 1h \
  --input name=Ada \
  --locked \
  --yes
```

Abre el enlace, introduce un nombre y la CLI ejecutará la Recipe en este host.
La página mostrará `message` y permitirá descargar `receipt.txt` mediante una
URL firmada temporal; script, entorno y logs permanecen locales.

Para registrar durablemente el Space en el control plane local:

```console
# Preferido: login/registro en el Cloud (sin token)
../../bin/doppels login --server http://localhost:4000
# Alternativa bootstrap:
DOPPELS_API_TOKEN=local-development-token \
../../bin/doppels login --server http://localhost:4000
../../bin/doppels plan
../../bin/doppels apply
```

`login` deja Context en `<slug>/private` (Org del Identity + Space private).
Display de la Org: p.ej. `Ada's Org`.
Para el Space de ejemplo (`doppels.platform.yaml`), usa el Org
sembrado `local`:

```console
../../bin/doppels org use local
../../bin/doppels space use platform
../../bin/doppels plan
../../bin/doppels apply
```

Layout del Space:

```text
capabilities/           # manifests (git)
recipes/
doppels.platform.yaml
doppels.lock            # tras apply
.doppels/               # solo runtime
```

`apply` escribe `doppels.lock` solo después de que el Cloud (u org `local`)
confirme la reconciliación completa.
