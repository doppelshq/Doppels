# Doppels

**Freeze Cursor, Claude, and Codex sessions into local deterministic recipes.**

You explore with an agent. You get something that works once. Next week you re-prompt and hope. Doppels turns that session into a **Capability** (the contract) and a **Recipe** (how it runs on *your* machine)—typed, validated, replayable. Execution never leaves the host. Credentials stay where they already are (`aws`, `gcloud`, `psql`, SSH, VPN).

Apache-2.0. The hosted control plane ([doppels.so](https://doppels.so)) is **not** in this repository.

## Why

Coding agents are great at discovery and terrible at memory. Scripts in chat die. Docs drift. Re-running “that migration” means starting over.

Doppels is the freeze button:

1. Agent (or you) captures the working path.
2. CLI validates the contract and runs it locally.
3. Same inputs → same Steps → auditable result.

## Fastest path: freeze from the agent

In Cursor, Claude Code, or Codex:

```console
npx skills add doppelshq/doppels --skill doppel-freeze
```

Then ask the agent something like:

> doppel freeze — turn what we just did into a Capability

The skill installs/checks the CLI, writes the manifests, and validates them. You stay in the chat where you already work.

## Install the CLI

Toolchains: [mise](https://mise.jdx.dev).

```console
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install
task setup
task build:cli          # → ./bin/doppels
```

With the mise shell hook, `bin/` is on `PATH`.

## Quickstart (offline)

```console
cd examples/quickstart
../../bin/doppels validate
../../bin/doppels run capability/greet --input name=Ada --yes
```

More examples in that folder (`release-pipeline`, manual fulfillment without a Recipe).

## Core ideas

| Term | Meaning |
| --- | --- |
| **Capability** | Public contract: inputs and outputs |
| **Recipe** | Local how: Steps, host `requires`, `returns` |
| **Request / Run** | A ask, and one attempt to fulfill it |

A Capability can have zero, one, or many Recipes. With no Recipe you can still fulfill manually. Cloud, if you use it later, coordinates shares and teams—it never executes Steps.

## What’s in this repo

```text
apps/cli/              Go runtime (validate, run, share client)
schemas/               JSON Schema contracts (source of truth)
skills/doppel-freeze/  Agent skill for Cursor / Claude / Codex
skills/doppel-use/     Draft — blocked on MCP
examples/quickstart/   Local Space you can run offline
```

`doppels share` can talk to a server (`--server`). Default hosted API is doppels.so. That server lives elsewhere.

## Docs & product

- Site: [doppels.so](https://doppels.so)
- Contribute: [CONTRIBUTING.md](CONTRIBUTING.md) (DCO + CLA)
- License: Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE)
