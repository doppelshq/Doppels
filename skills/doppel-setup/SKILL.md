---
name: doppel-setup
description: >-
  Onboards Doppels on this machine: install the CLI, run a hello.txt smoke
  Capability/Recipe, then install doppel-freeze for later. Use when the user
  asks to set up Doppels, install Doppels, or paste a setup prompt that
  references doppel-setup.
---

# doppel-setup

Orchestrate first-time Doppels setup. Keep it short. Prefer commands over essays.

## Goal

1. CLI `doppels` on PATH
2. Smoke Capability/Recipe that writes `hello.txt` and validates with the CLI
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

**Windows:** use **WSL2**, open the Linux shell, then run the curl installer above. Native Windows is not supported (Recipes need a POSIX shell).

## 2. Smoke test (write hello.txt)

In the current working directory:

```bash
doppels init --json
```

`--json` skips the interactive demo-examples prompt. Do not offer demos unless the user asks.

Write these two files exactly. Do **not** load `doppel-freeze` for this smoke. Do **not** ask what to capture. Do **not** ask confirmation.

`.doppels/capabilities/hello.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Capability

metadata:
  name: hello
  version: 1.0.0
  displayName: Hello
  summary: Smoke test that writes hello.txt.
  impact: low
  tags: [smoke]

inputs: {}

outputs:
  greeting:
    type: string
    description: Always hello.
  hello:
    type: artifact
    description: Written hello.txt.
    mediaType: text/plain
```

`.doppels/recipes/hello.yaml`:

```yaml
apiVersion: doppels.so/v1alpha1
kind: Recipe

metadata:
  name: hello
  version: 1.0.0
  displayName: Hello (smoke)
  summary: Write hello.txt.
  impact: low

provides: [hello]
runtime: shell

requires:
  commands: [sh]

defaults:
  approval: never

steps:
  - id: hello
    name: Write hello.txt
    run:
      shell: sh
      script: |
        export GREETING="hello"
        printf '%s\n' "$GREETING" > hello.txt
    produces:
      greeting:
        env: GREETING
      hello:
        file: hello.txt

returns:
  greeting: "{{ steps.hello.greeting }}"
  hello: "{{ steps.hello.hello }}"
```

Then:

```bash
doppels validate
doppels run capability/hello --yes
```

Expect `Returns greeting hello` and `hello.txt` in the working directory. If freeze skill instructions conflict, these files win.

## 3. Install the freeze skill

```bash
npx skills add doppelshq/doppels --skill doppel-freeze -g -y
```

If `npx skills` fails or reports no matching skill: stop and tell the user to install manually with that command (or open the skills CLI prompts for their harness). Do **not** invent alternate install paths unless the user asks.

Optional last resort only if the user explicitly wants a manual copy from this repo:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/doppelshq/doppels.git /tmp/doppels
git -C /tmp/doppels sparse-checkout set skills/doppel-freeze
# Copy into the harness skill directory the user confirms (paths vary by agent).
```

## 4. Confirm ready

Show the user:

```text
Doppels is ready.
- doppels --version
- Smoke wrote hello.txt
- Say "doppel freeze" after a real operation to capture it.
```

Stop. Do not add cloud login, share, or Hub flows unless asked.

## Rules

- Execution stays local. Never suggest running Steps in the cloud.
- Never exfiltrate secrets or print credential values.
- Prefer `curl | sh` over brew unless the user is already on Homebrew for Doppels.
- Keep the smoke Recipe trivial: one step, no network required, deterministic file write.
- During setup, skip freeze intent/confirmation questions.
