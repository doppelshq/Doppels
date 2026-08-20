# Doppels skills

Agent skills for Doppels, installable via
[`npx skills add`](https://github.com/vercel-labs/skills).

## Status

| Skill | Status | Notes |
|---|---|---|
| `doppel-setup` | Active | Onboarding: CLI + freeze skill + smoke test. |
| `doppel-freeze` | Active | Capture an agent session as a local deterministic manifest. |
| `doppel-use` | Draft | Blocked until the MCP server ships. |

## Agent prompt (recommended)

Paste into Cursor, Claude Code, Codex, or similar:

```text
Install the doppel-setup skill from github.com/doppelshq/doppels and use it to set up Doppels on this machine following best practices.
```

## Install via skills CLI

```bash
npx skills add doppelshq/doppels --skill doppel-setup
npx skills add doppelshq/doppels --skill doppel-freeze
npx skills add doppelshq/doppels -a claude-code -a cursor -a codex
```
