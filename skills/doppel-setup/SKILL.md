---
name: doppel-setup
description: >-
  Onboards Doppels on this machine: install the CLI, install doppel-freeze,
  run a 10-second smoke freeze (ping→pong). Use when the user asks to set up
  Doppels, install Doppels, or paste a setup prompt that references doppel-setup.
---

# doppel-setup

Orchestrate first-time Doppels setup. Keep it short. Prefer commands over essays.

## Goal

1. CLI `doppels` on PATH
2. Skill `doppel-freeze` installed for this agent harness
3. Smoke freeze that prints `pong` and validates with the CLI

## 1. Install the CLI

Check:

```bash
doppels --version
```

If missing:

**Preferred (official):**

```bash
curl -fsSL https://doppels.so/install.sh | sh
```

Ensure `~/.local/bin` is on PATH when the installer says so. Re-check `doppels --version`.

**macOS Homebrew alternative** (only if the user prefers brew):

```bash
brew tap doppelshq/tap && brew trust doppelshq/tap && brew install --cask doppels
```

Do not invent other installers. Do not download random binaries.

## 2. Install the freeze skill

```bash
npx skills add doppelshq/doppels --skill doppel-freeze
```

If the harness needs a different agent flag, follow the skills CLI prompts.
Also load `doppel-freeze` from this repo when already cloning `doppelshq/doppels`.

## 3. Smoke test (ping → pong → freeze)

In the current working directory:

```bash
doppels init
```

Decline demo examples unless the user asks for them (`[y/N]` → no is fine).

Then run an operational ping (not an LLM summary):

```bash
printf 'pong\n'
```

Capture that as a tiny Capability/Recipe using `doppel-freeze` guidance
(explicit freeze request is implied by this setup skill):

- Capability that returns a string message `pong`
- Recipe with one shell step that prints `pong`
- `doppels validate`
- `doppels run` with `--yes` if prompts appear

If freeze skill instructions conflict, prefer: write minimal YAML under
`.doppels/`, validate, run.

## 4. Confirm ready

Show the user:

```text
Doppels is ready.
- doppels --version
- Say "doppel freeze" after a real operation to capture it.
```

Stop. Do not add cloud login, share, or Hub flows unless asked.

## Rules

- Execution stays local. Never suggest running Steps in the cloud.
- Never exfiltrate secrets or print credential values.
- Prefer `curl | sh` over brew unless the user is already on Homebrew for Doppels.
- Keep the smoke Recipe trivial: one step, no network required.
