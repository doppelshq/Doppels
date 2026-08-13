# Doppels

Local-first CLI for **deterministic Capabilities and Recipes**. Agents and
humans capture a repeatable run on your machine. Execution never leaves the
host.

Apache-2.0. The hosted control plane ([doppels.so](https://doppels.so)) is
**not** in this repository.

## Install

Toolchains: [mise](https://mise.jdx.dev). Then:

```console
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install
task setup
task build:cli          # → ./bin/doppels
```

With the mise shell hook, `bin/` is on `PATH`.

## Quickstart

```console
cd examples/quickstart
../../bin/doppels validate
../../bin/doppels run capability/greet --input name=Ada --yes
```

## Agent skill

Capture a session as a local Capability + Recipe:

```console
npx skills add doppelshq/doppels --skill doppel-freeze
```

## Layout

```text
apps/cli/              Go runtime (validate, run, share client)
schemas/               JSON Schema contracts (source of truth)
skills/doppel-freeze/  Agent skill
examples/quickstart/   Local Space you can run offline
```

`doppels share` talks to a server (`--server`). Default hosted API is
doppels.so. This repo does not include that server.

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
