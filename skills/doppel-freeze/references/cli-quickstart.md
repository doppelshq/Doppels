# CLI quickstart

Binario: **`doppels`** (no `doppel`).

## Instalar

```bash
curl -fsSL https://doppels.so/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
# o: brew tap doppelshq/tap && brew trust doppelshq/tap && brew install --cask doppels
```

macOS / Linux nativo. Windows: **WSL2** + el mismo `curl | sh` dentro de la
distro Linux. Sin `.exe` nativo en alpha.

Desde fuente (mise):

```bash
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install
task setup
task build:cli          # → ./bin/doppels
```

Comprueba: `doppels --version`.

## Space local

```bash
doppels init --json
```

```text
.
└── .doppels/
    ├── capabilities/      # commit these
    ├── recipes/           # commit these
    ├── <space>.space.yaml
    └── runs/              # gitignored runtime
```

El **cwd de `doppels run`** es la raíz del Space (este `.`), no
`.doppels/runs/`. Archivos de `produces.file` se crean relativos a esa raíz.

## Comandos útiles para freeze

```bash
doppels validate
doppels capabilities list
doppels describe capability/<name>
doppels run capability/<name> --input key=value --yes
doppels runs list
doppels runs show <run-id>
doppels runs logs <run-id>
doppels --help
```

`--json` en la mayoría de comandos.

Manifests sueltos:

```bash
doppels validate -f .doppels/capabilities/<name>.yaml
```

## Inputs en run

- `--input name=Ada` fija `inputs.name`.
- Si el Capability declara `default` y omites `--input`, se usa el default.
- `required: true` sin valor ni default → error.

## No existen

- `doppel` (sin s) como binario oficial.
- `doppels schema list` / `doppels schema describe`.
- Generador `doppels freeze`.

Schemas: ver `schema-discovery.md` (raw JSON en `doppelshq/doppels`).

## Cloud (fuera del freeze local)

`doppels preview` / `apply` son flujos cloud/org — **no** hace falta para
freeze + `run` local.
