# CLI quickstart

Comandos esenciales de Doppel para usar desde la skill `doppel-freeze`.

## Verificar instalación

```bash
doppel --version
which doppel
```

Si no está instalado, instalar según el sistema operativo.

### macOS

```bash
brew install doppels/tap/doppels
```

### Linux

```bash
curl -fsSL https://doppels.so/install.sh | sh
```

### Verificar binario

```bash
doppel --help
doppel doctor
```

## Inicializar Space local

```bash
doppels spaces init
```

Crea estructura base:

```text
.
├── capabilities/
├── recipes/
├── doppels.<space>.yaml   # stub Space
└── .doppels/              # runtime only
```

`doppels spaces init` es idempotente: no sobrescribe un `doppels.<space>.yaml`
existente. No registra el Space en Cloud; usa `org use` / `space use` + `apply`.

## Comandos frecuentes

```bash
# Listar Capabilities y Recipes
doppel list

# Validar manifest contra schema
doppels validate <archivo>

# Mostrar plan de ejecución
doppels plan <archivo> --input <nombre>=<valor>

# Ejecutar (modo prueba sin efectos)
doppels run --dry-run <archivo> --input <nombre>=<valor>

# Ejecutar
doppels run <archivo> --input <nombre>=<valor>
```

## Descubrir schemas

```bash
doppel schema list
doppel schema describe capability
doppel schema describe recipe
```

## Ayuda contextual

```bash
doppel <command> --help
```