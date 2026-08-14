# Doppels

**Freeze Cursor, Claude, and Codex sessions into local deterministic recipes.**

You explore with an agent. You get something that works once. Next week you re-prompt and hope. Doppels turns that session into a **Capability** (the contract) and a **Recipe** (how it runs on *your* machine)—typed, validated, replayable. Execution never leaves the host. Credentials stay where they already are (`aws`, `gcloud`, `psql`, SSH, VPN).

Apache-2.0.

## Why

Coding agents are great at discovery and terrible at memory. Scripts in chat die. Docs drift. Re-running “that migration” means starting over.

Doppels is the freeze button:

1. Agent (or you) captures the working path.
2. CLI validates the contract and runs it locally.
3. Same inputs → same Steps → auditable result.

## How it works

```text
Cursor / Claude / Codex          Your repo
        │                              │
        │  explore, fix, ship once     │
        ▼                              │
   doppel freeze (skill)               │
        │                              │
        │  writes YAML                 ▼
        └──────────────────►  capabilities/<name>.yaml
                              recipes/<name>.yaml   (optional)
                                        │
                                        ▼
                              doppels validate
                              doppels run capability/<name>
                                        │
                                        ▼
                              .doppels/runs/<id>/   (logs, artifacts)
```

- **Capability** = what it does (inputs / outputs). The public contract.
- **Recipe** = how it runs (Steps, shell, `requires`, `returns`). Written to `recipes/` in **your** project — commit it to git like any other source.
- **Run** = one local attempt with fixed inputs. History lives under `.doppels/` (runtime; usually gitignored).

## Freeze from Cursor, Claude, or Codex

**1. Install the skill** (once per machine or project):

```console
npx skills add doppelshq/doppels --skill doppel-freeze
```

**2. Do real work in the agent** — deploy, migrate, fix prod, whatever you want to repeat later.

**3. Freeze** — say something like:

> doppel freeze — turn what we just did into a Capability

The skill then:

1. Checks `doppels` is on `PATH` (or points you at install steps below).
2. Runs `doppels spaces init` if the folder has no `.doppels/` yet.
3. Asks what to capture (one Capability per distinct outcome).
4. Reads the session (commands, files, inputs, outputs) and **writes YAML by hand** — there is no `doppels freeze` generator command.
5. Loops `doppels validate` and test `doppels run …` when safe until clean.
6. Shows you the contract and asks before treating it as done. **Commit** the YAML when you are happy — that is the repeatable asset.

Skill internals: [`skills/doppel-freeze/SKILL.md`](skills/doppel-freeze/SKILL.md) and [`skills/doppel-freeze/references/`](skills/doppel-freeze/references/).

## Install the CLI

From source (mise):

```console
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install
task setup
task build:cli          # → ./bin/doppels
```

With the mise shell hook, `bin/` is on `PATH`.

Tagged builds (when a `v*` tag exists): [GitHub Releases](https://github.com/doppelshq/doppels/releases). Homebrew tap comes later.

## CLI workflow

From any project directory:

```console
doppels spaces init              # capabilities/, recipes/, .doppels/
doppels validate                 # all manifests under discovery paths
doppels capabilities list        # what you have locally
doppels describe capability/greet
doppels run capability/greet --input name=Ada --yes
doppels runs list
doppels runs show <run-id>
doppels runs logs <run-id>
```

Machine-readable output: add `--json` to most commands.

### Quickstart (copy-paste)

```console
cd examples/quickstart
../../bin/doppels validate
../../bin/doppels run capability/greet --input name=Ada --yes
../../bin/doppels runs list
```

That Space includes `greet`, a multi-step `release-pipeline`, and a Capability with no Recipe (`manual-review`).

### Minimal manifests

See [`examples/quickstart/capabilities/greet.yaml`](examples/quickstart/capabilities/greet.yaml) and [`examples/quickstart/recipes/greet.yaml`](examples/quickstart/recipes/greet.yaml). More samples in [`skills/doppel-freeze/references/examples/`](skills/doppel-freeze/references/examples/). Full schema: [`schemas/`](schemas/).

## What’s in this repo

```text
apps/cli/              Go runtime (validate, run, local history)
schemas/               JSON Schema contracts (source of truth)
skills/doppel-freeze/  Agent skill for Cursor / Claude / Codex
skills/doppel-use/     Draft — blocked on MCP
examples/quickstart/   Local Space you can run offline
```

## Links

- [doppels.so](https://doppels.so)
- [CONTRIBUTING.md](CONTRIBUTING.md) (DCO + CLA)
- [LICENSE](LICENSE) and [NOTICE](NOTICE) — Apache-2.0
