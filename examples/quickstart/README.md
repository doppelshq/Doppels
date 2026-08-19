# Quickstart

Full reference Space: approval, manual capability, and a multi-step pipeline.

```console
cd examples/quickstart
doppels validate
doppels run capability/greet --input name=Ada --yes
```

`greet-with-shell` produces a scalar and an artifact. `greet-with-approval` is
identical but requires HITL approval before each step.

`release-pipeline` is a four-step Recipe (≈1–3s each) for timeline demos:

```console
doppels run capability/release-pipeline --input version=1.2.3 --yes
```

`manual-review` has no Recipe — fulfill manually with `--output` / `--evidence`.

## Space layout

```text
.doppels/
  platform.space.yaml   # Space manifest
  capabilities/         # versioned
  recipes/              # versioned
  runs/                 # gitignored (runtime state)
  .gitignore
```

## Cloud (experimental)

With a local Cloud running from the repo root (`task dev:cloud`):

```console
doppels experimental on
doppels login --server http://localhost:4000
doppels org use local
doppels space use platform
doppels preview
doppels apply
```

`apply` writes `doppels.lock` only after the Cloud confirms full reconciliation.

For `doppels share`:

```console
doppels share capability/greet \
  --server http://localhost:4000 \
  --input name=Ada \
  --yes
```
