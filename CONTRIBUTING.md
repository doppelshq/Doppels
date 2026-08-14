# Contributing to Doppels

This repository is the **source of truth** for the Apache-2.0 CLI, schemas,
and agent skills. Pull requests merge here.

The private control plane (`doppelshq/internal`) consumes this repo as a git
submodule. Do not send CLI patches there.

Keep changes small, tested, and on-vocabulary (Capability, Recipe, Request,
Run, RunEvent).

## Legal (required for external PRs)

1. **DCO** — every commit must include `Signed-off-by` (`git commit -s`). See `DCO.md`.
2. **CLA** — state in the PR that you agree to `CLA.md` (until a CLA bot exists).

PRs without DCO (and CLA acceptance when required) will not be merged.

## Dev

```bash
mise install
task setup
task test
```

Toolchains: **mise** only (not asdf). Conventional Commits with scope
`cli` or `schemas`.

## Boundaries

- Execution is local-only (`apps/cli`). A remote server is never a worker.
- Data shapes live in `schemas/` first.
- Do not add a hosted control plane to this repository.
