# Redacción de secretos

Nunca incluir valores secretos en un manifest. Solo referencias.

## Patrón correcto

```yaml
env:
  DEPLOY_TOKEN:
    from: host_env
    name: DEPLOY_TOKEN
```

Doppel resolverá `DEPLOY_TOKEN` desde el entorno local del Node al ejecutar.

## Patrones prohibidos

```yaml
# MAL — token en claro
env:
  DEPLOY_TOKEN: "ghp_xxxxxxxxxxxxxxxxxxxx"

# MAL — valor en script
run:
  script: |
    curl -H "Authorization: Bearer ghp_xxxxxxxxxxxxxxxxxxxx"

# MAL — en comentario
# DEPLOY_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx

# MAL — en description
description: "Usa el token ghp_xxxxxxxx"
```

## Cómo detectar secretos en la sesión

Inspeccionar la sesión del agente buscando:

- Cadenas tipo `ghp_*`, `sk-*`, `AKIA*`, `xox[bpars]-*`.
- URLs con `user:password@host`.
- Líneas que parezcan `KEY=value` en logs.
- Headers `Authorization:`, `X-API-Key:`, `Cookie:`.

Si aparecen, redactar antes de persistir el manifest y avisar al usuario.

## Redacción durante freeze

1. Detectar cualquier cadena sospechosa en la sesión.
2. Sustituirla por una referencia simbólica:

```text
ghp_xxxxxxxxxxxxxxxxxxxx → {{ from: host_env, name: GITHUB_TOKEN }}
```

3. Añadir la variable a `requires.hostEnv` o al `env` del Step:

```yaml
requires:
  hostEnv: [GITHUB_TOKEN]
```

4. Avisar al usuario qué secretos se referencian antes de confirmar.

## Si el usuario debe proveer un secreto

Indicarle que añada la variable a su entorno local:

```bash
export GITHUB_TOKEN="..."
```

O usar un secret manager externo (1Password CLI, `pass`, etc.) que exponga el
valor al shell antes de la ejecución.

## Qué NUNCA hacer

- Guardar valores en el YAML ni en `description`.
- Confiar en que el usuario "ya sabe" que no debe pegar el token.
- Asumir que un secret manager "ya lo tiene"; declararlo explícitamente.