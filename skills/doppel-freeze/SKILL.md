---
name: doppel-freeze
description: >-
  Convierte la sesión actual del agente en una Capability + Recipe local
  determinista. Asume que el agente no conoce Doppel y lo guía desde
  instalación hasta manifest validado. Usar SOLO cuando el usuario pida
  captura de forma explícita: "doppel freeze", "congelar esto", "guardar
  este proceso", "hazlo repetible" o "cómo lo vuelvo a ejecutar". No
  activar por defecto en cada tarea operativa. No preguntar al final de
  cada operación si persistir.
requires:
  doppels: ">=0.1.0"
---

# doppel freeze

Guía al agente para capturar una sesión en Capability + Recipe locales.
No asume conocimiento previo de Doppel.

**Cuándo activar (v1):** solo petición explícita de captura, o si el usuario
pregunta cómo repetir lo que acaba de funcionar (ofrecer freeze **una vez**).
Nunca recipe-first. Nunca preguntar al cierre de cada tarea.

## Contrato mínimo (leer antes de escribir YAML)

1. Copia el ejemplo **greet** en `references/examples/` (también embebido en
   `references/manifest-anatomy.md`). Es capability + recipe válidos.
2. Flujo de datos:
   `inputs` → `step.env: VAR: "{{ inputs.x }}"` → `export OUT=…` →
   `produces: { key: { env: OUT } }` → `returns: "{{ steps.id.key }}"`.
3. Schemas reales:
   https://raw.githubusercontent.com/doppelshq/doppels/main/schemas/capability.schema.json
   https://raw.githubusercontent.com/doppelshq/doppels/main/schemas/recipe.schema.json
   No existe `doppel schema …`. No uses `github.com/doppels/schemas`.
4. Cwd del Run = raíz del Space (donde está `.doppels/`). `produces.file`
   escribe ahí relativo — puede ensuciar el repo.

Detalle: `references/manifest-anatomy.md` + `references/schema-discovery.md`.

## Flujo

### 1. Verificar entorno

```bash
export PATH="$HOME/.local/bin:$PATH"
doppels --version
```

Si falta la CLI: instrucciones en `references/cli-quickstart.md` (o skill
`doppel-setup`). Binario = **`doppels`** (con s).

### 2. Inicializar Space local

Si no existe `.doppels/`:

```bash
doppels init --json
```

Si existe:

```bash
doppels capabilities list
```

### 3. Preguntar intención

> ¿Qué quieres capturar de esta sesión?

Varios logros distintos → preguntar si una Capability por cada uno.

### 4. Analizar la sesión

Comandos, archivos, deps, inputs, outputs. Ver
`references/determinism-rules.md`.

### 5. Escribir YAML a mano

Basarse en el ejemplo greet + `manifest-anatomy.md`. **No** existe un comando
generador `doppel freeze` / `doppels freeze`.

Aplicar:

- Paths POSIX relativos; artefactos relativos al root del Space.
- Redacción de secretos (`references/secrets-redaction.md`).
- Tipos/inputs/produces según anatomy (no inventar `type: object` / `enum` como tipo).

Escribir bajo:

```text
.doppels/capabilities/<name>.yaml
.doppels/recipes/<name>.yaml
```

### 6. Proponer nombre

Mostrar nombre propuesto antes de dar por cerrado el freeze.

### 7. Validar y probar

```bash
doppels validate
doppels run capability/<name> --input key=value --yes
```

Si un input tiene `default` y no es required sin valor, se puede omitir
`--input` para esa clave. Iterar hasta verde.

### 8. Confirmar con el usuario

Nombre, inputs, outputs, `requires`, secretos (solo refs), `impact` /
`approval`. Confirmación explícita.

### 9. Persistir

YAML ya en disco. Commit = decisión del usuario (no auto-commit).

## Restricciones

- NUNCA invocar un generador `doppel freeze` (no existe).
- NUNCA escribir secretos literales; solo `from: host_env`.
- NUNCA freeze automático al cerrar cada tarea.
- SIEMPRE `doppels validate` antes de dar por bueno.
- Si no hay CLI, abortar con quickstart.

## Edge cases

- Sesión con errores: capturar la versión final corregida.
- `requires.commands` con `version: ">=x.y.z"` solo si el formato schema lo
  admite (regex estricto).
- Sin output claro: preguntar.
- Sesión ruidosa: reducir scope.
- Capability similar existente: avisar.

## Referencias

- [cli-quickstart.md](references/cli-quickstart.md)
- [schema-discovery.md](references/schema-discovery.md)
- [manifest-anatomy.md](references/manifest-anatomy.md) — **incluye ejemplo greet completo**
- [determinism-rules.md](references/determinism-rules.md)
- [secrets-redaction.md](references/secrets-redaction.md)
- [examples/](references/examples/) — mismos YAML greet
