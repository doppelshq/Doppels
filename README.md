<p align="center">
  <img src="docs/images/mark.svg" width="40" height="40" alt="Doppels mark">
</p>

<h1 align="center">Doppels</h1>

<p align="center"><strong>Freeze your AI operations</strong></p>

<p align="center">Turn AI processes into deterministic local Capabilities.</p>

<p align="center">
  <a href="https://doppels.so">Website</a>
  ·
  <a href="https://docs.doppels.so">Docs</a>
  ·
  <a href="LICENSE">Apache-2.0</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-Apache%202.0-2F6B45?style=flat-square" alt="Apache 2.0">
  <img src="https://img.shields.io/badge/status-pre--alpha-737373?style=flat-square" alt="pre-alpha">
</p>

<p align="center">
  <img src="docs/images/hero-freeze.svg" alt="Freeze an agent session into Capability YAML, commit it, then doppels run replays it locally for zero tokens" width="100%">
</p>

## Why

Coding agents are great at discovery and terrible at memory. Scripts in chat die. Docs drift. Re-running “that migration” means starting over — and paying tokens again.

The diagram above is the whole product:

1. **Freeze once** — the agent (or you) captures the working path as YAML. That session costs tokens once (`$0.54` in the example). There is no `doppels freeze` generator; the [skill](skills/doppel-freeze/SKILL.md) writes the files by hand.
2. **Saved on your Git** — a **Capability** is the public contract (inputs / outputs). A **Recipe** is how it runs on *your* machine (Steps, shell, `requires`, `returns`). Both are plain YAML in `capabilities/` and `recipes/`. Review them in a PR like any other source.
3. **Replay for $0** — `doppels run` executes locally on your Node. Same inputs → same Steps → auditable **Run** under `.doppels/` (gitignored). Credentials stay where they already are (`aws`, `gcloud`, `psql`, SSH, VPN). No model. No re-prompt.

> Pre-alpha. Tagged builds are prereleases (`v0.0.0-dev.*`), not a product launch.

## Install

Binary name is **`doppels`** (not `doppel`).

**curl**

```console
curl -fsSL https://doppels.so/install.sh | sh
```

Installs to `~/.local/bin/doppels`. Add that directory to `PATH` if needed. Pin a tag with `DOPPELS_VERSION=v0.0.0-dev.1`. Same script: [GitHub Releases](https://github.com/doppelshq/doppels/releases) / `install.sh`.

**Homebrew**

```console
brew tap doppelshq/tap
brew trust doppelshq/tap   # Homebrew 6+
brew install doppels
```

**From source** ([mise](https://mise.jdx.dev))

```console
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install && task setup && task build:cli   # → ./bin/doppels
```

```console
doppels --version
```

## Freeze from an agent

```console
npx skills add doppelshq/doppels --skill doppel-freeze
```

Do the real work (deploy, migrate, fix prod). Then:

> doppel freeze — turn what we just did into a Capability

Commit the YAML when it validates. That is the asset.

Details: [freeze guide](https://docs.doppels.so/guides/freeze-with-agent) · [`skills/doppel-freeze/`](skills/doppel-freeze/SKILL.md)

## Run locally

```console
doppels spaces init
doppels validate
doppels run capability/greet --input name=Ada --yes
doppels runs list
```

Most commands take `--json`. Copy-paste Space in [`examples/quickstart`](examples/quickstart) (`greet`, `release-pipeline`, and a Capability with no Recipe). Schemas: [`schemas/`](schemas/). CLI reference: [docs](https://docs.doppels.so/reference/cli).

## What’s in this repo

```text
apps/cli/              Go runtime (validate, run, local history)
schemas/               JSON Schema contracts (source of truth)
skills/doppel-freeze/  Agent skill for Cursor / Claude / Codex
skills/doppel-use/     Draft — blocked on MCP
examples/quickstart/   Local Space you can run offline
docs/images/           README diagram (Session → Capability → Run)
install.sh             curl installer
```

## Links

- [doppels.so](https://doppels.so)
- [docs.doppels.so](https://docs.doppels.so)
- [Homebrew tap](https://github.com/doppelshq/homebrew-tap)
- [CONTRIBUTING.md](CONTRIBUTING.md) (DCO + CLA)
- [LICENSE](LICENSE) and [NOTICE](NOTICE) — Apache-2.0
