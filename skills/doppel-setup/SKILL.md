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
2. Smoke Capability/Recipe that prints `pong` and validates with the CLI
3. Skill `doppel-freeze` installed for this agent harness (for later)

## 1. Install the CLI

Always prefix the session PATH before checking:

```bash
export PATH="$HOME/.local/bin:$PATH"
doppels --version
```

If missing:

**Preferred (official):**

```bash
curl -fsSL https://doppels.so/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
doppels --version
```

**macOS Homebrew alternative** (only if the user prefers brew):

```bash
brew tap doppelshq/tap && brew trust doppelshq/tap && brew install --cask doppels
```

Do not invent other installers. Do not download random binaries.

## 2. Smoke test (ping → pong)

In the current working directory:

```bash
doppels init
```

Current CLI writes the Space and continues. Do not wait for a demo-examples prompt.

Write these two files exactly. Do **not** load `doppel-freeze` for this smoke. Do **not** ask what to capture. Do **not** ask confirmation.

`.doppels/capabilities/ping.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: ping
  version: 1.0.0
  displayName: Ping
  summary: Smoke test that returns pong.
  impact: low
  tags: [smoke]

inputs: {}

outputs:
  message:
    type: string
    description: Always pong.
```

`.doppels/recipes/ping.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: ping
  version: 1.0.0
  displayName: Ping (smoke)
  summary: Print pong.
  impact: low

provides: [ping]
runtime: shell

requires:
  commands: [sh]

defaults:
  approval: never

steps:
  - id: ping
    name: Print pong
    run:
      shell: sh
      script: |
        export MESSAGE="pong"
        printf '%s\n' "$MESSAGE"
    produces:
      message:
        env: MESSAGE

returns:
  message: "{{ steps.ping.message }}"
```

Then:

```bash
printf 'pong\n'
doppels validate
doppels run capability/ping --yes
```

Expect `Returns message pong`. If freeze skill instructions conflict, these files win.

## 3. Install the freeze skill

```bash
npx skills add doppelshq/doppels --skill doppel-freeze -g -y
```

If the CLI reports no matching skill, copy from this repo into the harness skill dir:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/doppelshq/doppels.git /tmp/doppels
git -C /tmp/doppels sparse-checkout set skills/doppel-freeze
cp -R /tmp/doppels/skills/doppel-freeze ~/.agents/skills/doppel-freeze
```

Also copy to `~/.cursor/skills/doppel-freeze` when the harness is Cursor.

If the harness needs a different agent flag, follow the skills CLI prompts.

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
- During setup, skip freeze intent/confirmation questions.
