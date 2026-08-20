# CLI quickstart

Binario: **`doppels`** (no `doppel`).

## Instalar

```bash
curl -fsSL https://doppels.so/install.sh | sh
# o: brew tap doppelshq/tap && brew trust doppelshq/tap && brew install --cask doppels
```

Desde fuente (mise):

```bash
git clone https://github.com/doppelshq/doppels.git
cd doppels
mise install
task setup
task build:cli          # → ./bin/doppels
```

Con el hook de mise, `bin/` entra en `PATH`. Comprueba:

```bash
doppels --version
which doppels
```

Si no está en PATH: usa `./bin/doppels` desde la raíz de este repo.

Tras un tag `v*`: binarios en
[GitHub Releases](https://github.com/doppelshq/doppels/releases).

## Space local

```bash
doppels spaces init
```

```text
.
├── capabilities/
├── recipes/
├── doppels.<space>.yaml
└── .doppels/              # runtime only
```

## Comandos

```bash
doppels validate
doppels capabilities list
doppels describe capability/<name>
doppels run capability/<name> --input key=value --yes
doppels runs list
doppels runs show <run-id>
doppels runs logs <run-id>
doppels --help
doppels <command> --help
```

`--json` en la mayoría de comandos.

Manifests sueltos: `doppels validate -f ./capabilities/<name>.yaml`
