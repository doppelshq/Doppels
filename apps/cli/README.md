# Doppels CLI

The Go CLI owns discovery, validation and execution. Cloud coordinates a
shared Request but never receives or executes Recipe Steps.

## Development binary

In this repo, development always uses the local build:

```console
task build:cli          # → bin/doppels at the monorepo root
```

With **mise** activated in the shell (`mise install` + hook), `bin/` is on
`PATH`, so `doppels` resolves to that build:

```console
doppels …               # same as ./bin/doppels when cwd is inside the repo
./bin/doppels …         # always works from repo root without mise PATH
../../bin/doppels …     # from examples/quickstart
```

Do not rely on a globally installed `doppels` while developing.

```console
task build:cli
doppels spaces init platform
doppels validate
doppels describe capability/release-preview
doppels capabilities list
doppels recipes list
doppels run capability/release-preview --input version=1.2.3
doppels run capability/greet -d
doppels runs list
doppels runs list --limit 50
doppels runs list --all
doppels runs show <run-id>
doppels runs logs <run-id>
doppels runs logs <run-id> -f
```

`spaces init` creates `capabilities/`, `recipes/`, and `.doppels/` (runtime
marker only). It optionally writes a stub `doppels.<space>.yaml`. It does not
register the Space in Cloud — use `org use` / `space use` and `apply` for that.
`validate` discovers YAML recursively in `capabilities/` and `recipes/` by
default (or paths listed under `discovery` in the Space stub) and checks:

- the typed `doppels.so/v1alpha1` Capability and Recipe shapes;
- `Recipe.provides`, common input contracts and complete public `returns`;
- expression ordering, Step approvals and safe shell value binding through
  `env`;
-   declared commands and versions, host environment variables, and static
  Space files.

Use repeatable `-f` flags to validate explicit files and `--json` for stable
machine-readable output. A missing host requirement is operational (exit 1);
invalid usage or contracts use exit 2.

`run` resolves a Capability and exactly one compatible Recipe. Zero Recipes
starts manual fulfillment; more than one requires `--recipe name[@version]`.
Shell Steps run sequentially and in isolation. Values enter through declared
environment mappings. Local `run` treats the operator invocation as the grant
for Steps marked `approval: required` (no `--yes`). Shared Requests still prompt
when handled by `doppels share` or `doppels node up`, unless `--yes` grants the
required Steps. Local records
live under `.doppels/runs/<run-id>` (request/run JSON, events,
logs, artifacts). A SQLite index at `.doppels/runs.db` backs `runs list` and
an offline outbox for future metadata sync; blobs stay on disk.

`runs list` always includes local index rows with `source: local`. When you are
logged in and a Context with Organization and Space is selected, it also merges
Space Runs from the control plane as `source: cloud`. Cloud unreachable keeps
the local rows (Offline-First). After login (and after local `run` / `runs list`),
the CLI drains the outbox via `POST .../runs/ingest` when the Capability (and
Recipe, if any) is registered in the Space. Pending items stay queued on failure.

When Context is set and a Recipe with the same name@version is registered in the
Space, `doppels run` blocks if the local digest differs: sync from Git, review the
diff, then retry.

The plural resource commands are the local-first inspection API. Capability and
Recipe queries always read the current Project. Run lists may add explicit Cloud
rows when login+Context allow it; login never silently rewrites local history.

Step isolation means a fresh process and declared environment per Step; the
MVP is not an operating-system sandbox or container boundary.

Manual fulfillment accepts repeatable `--output name=value` and
`--evidence name=value`. Declared artifact outputs/evidence use
workspace-relative file paths. Without a Recipe there is no evidence contract,
so prefix an evidence file with `@`, for example
`--evidence proof=@review.txt`. Missing declared values are prompted
interactively.

To delegate one Request while keeping execution local:

```console
DOPPELS_API_TOKEN=local-development-token \
go run ./cmd/doppels share capability/release-preview \
  --server http://localhost:4000 \
  --expires 1h \
  --input release_version=1.2.3 \
  --locked \
  --yes
```

Remote servers must use HTTPS; plain HTTP is limited to loopback development.
`share` remains in the foreground, streams durably acknowledged RunEvents over
Phoenix Channels, uploads return/evidence artifacts before success and exits
after its one Request. Expiry closes a still-unused link; an already accepted
Request is allowed to finish. The runner token, Recipe, scripts, environment
and logs never enter the public response.

Durable control-plane registration is explicit:

```console
doppels init
DOPPELS_API_TOKEN=... doppels login --server https://doppels.so
doppels organizations list
doppels org use acme
doppels spaces list
doppels space use platform
doppels preview
doppels apply
```

`login` validates the token and stores it outside the Space working tree in an
owner-only credentials file. `DOPPELS_CONFIG_HOME` overrides that location. The
session response also ensures a personal Organization/Space (`<slug>`/`private`)
on the control plane and selects that Context after login. A Context is local
CLI state selecting an Organization and Space; it is not a Cloud resource.
`preview` never mutates local or remote state. `apply` discovers
the local Capability and Recipe manifests, optionally reconciles
`doppels.<space>.yaml`, registers immutable revisions, then writes the
deterministic `doppels.lock`. Reusing a pinned name/version with changed bytes
is rejected. Capability contracts are registered in full. Recipe registration
contains only its safe descriptor (metadata, `provides`, runtime and
requirements); scripts, environment mappings, procedures and return wiring
remain on the local host. Apply is additive: removing a local manifest never
unregisters it from the Space or produces an implicit delete. `prune` is the
explicit, opt-in counterpart: `doppels prune` previews unregistering
manifest-managed Space registrations (Capability/Recipe) whose name is absent
from the local Project, and `doppels prune --yes` executes it. Prune never
touches Cloud-managed registrations, never deletes the Organization-level
Capability/Recipe/Revision history, and never deletes the Space itself.

Exit codes are `0` for success, `1` for an operational or Run failure, `2` for
usage/contract errors and `130` for interruption. With `--json`, stdout is
reserved for one stable response document and human progress goes to stderr.
