# Reglas de determinismo

Una Capability o Recipe congelada debe ser reproducible. Aplicar siempre:

## Paths

- **Siempre relativos POSIX** (barra `/`, nunca `\`).
- **Sin paths absolutos** del host (`/Users/...`, `/home/...`).
- **Sin `~`**; usar paths relativos al workspace o al Recipe.
- **Sin `./` redundante** salvo al inicio del path.

Bueno:

```yaml
file: "release-{{ inputs.version }}.tgz"
file: dist/backup.sql.gz
```

Malo:

```yaml
file: /tmp/release.tgz
file: ~/Downloads/release.tgz
file: C:\Users\...\release.tgz
```

## Variables y secretos

- **Nunca capturar valores secretos** del entorno del agente.
- Usar siempre `from: host_env` con `name` simbólico:

```yaml
env:
  DEPLOY_TOKEN:
    from: host_env
    name: DEPLOY_TOKEN
```

- **Nunca** volcar `.env`, tokens, claves SSH o cookies al YAML.
- Si la sesión mostró un secreto, redactarlo en comentarios y pedir al
  usuario que confirme la redacción.

## Identificadores dinámicos

- **Quitar** timestamps, UUIDs, IDs auto-generados.
- **Quitar** resultados de `git rev-parse HEAD` salvo como `produces.env`
  explícito.
- **Quitar** outputs de `date`, `random`, etc., salvo que sean entradas
  deseadas.

## Dependencias detectadas

- Si Doppel detecta la versión de una dependencia, declararla:

```yaml
requires:
  commands:
    - name: node
      version: ">=22.0.0 <26.0.0"
```

- Si no puede detectar versión, dejar solo el nombre:

```yaml
requires:
  commands: [git]
```

## Aprobaciones

- Declarar `approval` por Step si la acción es sensible.
- `defaults.approval` se aplica a Steps sin override.
- `impact: high | critical` sugiere aprobación; el manifest debe reflejarla.

## Timeouts

- Si la sesión mostró un Step lento, declarar `timeout` por Step.
- Si no, dejar `defaults.timeout` global.

## Versionado

- Si se modifica un manifest existente, incrementar `version` semver.
- Cambios incompatibles en `inputs`/`outputs` → mayor.
- Cambios solo en defaults o descripciones → patch.

## Validación previa a freeze

Antes de pedir confirmación al usuario, verificar:

- [ ] Paths POSIX relativos.
- [ ] Sin secretos literales.
- [ ] Sin IDs dinámicos hardcodeados.
- [ ] Dependencias declaradas con versión si conocida.
- [ ] `approval` coherente con `impact`.
- [ ] `doppels validate` pasa.
- [ ] `doppels preview` pasa (si hay Context Org/Space).