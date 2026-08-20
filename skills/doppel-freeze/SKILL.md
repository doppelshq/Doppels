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

Esta skill guía al agente para capturar una sesión de trabajo en una Capability
y Recipe locales deterministas. No asume conocimiento previo de Doppel.

**Cuándo activar (v1):** solo con petición explícita de captura, o si el
usuario pregunta cómo repetir lo que acaba de funcionar (ofrecer freeze
**una vez**). Nunca recipe-first. Nunca preguntar al cierre de cada tarea.

## Flujo

### 1. Verificar entorno

```bash
doppels --version
which doppels
```

Si `doppels` no está disponible, abortar y mostrar al usuario las instrucciones
de instalación de `references/cli-quickstart.md`.

### 2. Inicializar Space local

Si no existe `.doppels/` en el directorio actual:

```bash
doppels init
```

Si existe:

```bash
doppels capabilities list
```

Mostrar al usuario las Capabilities existentes para evitar duplicados.

### 3. Preguntar intención

Antes de analizar la sesión, preguntar al usuario:

> ¿Qué quieres capturar de esta sesión?

Si la sesión tiene varios logros distintos (por ejemplo, configurar DB y
desplegar frontend), preguntar si se debe crear una Capability por cada uno.

### 4. Analizar la sesión

Usar las herramientas nativas del agente para inspeccionar:

- Comandos ejecutados (orden, argumentos, exit codes).
- Archivos creados o modificados.
- Dependencias externas usadas (binarios, librerías, APIs).
- Inputs identificados (variables que el usuario proveyó).
- Outputs producidos (archivos finales, valores retornados).

Para análisis profundo, consultar `references/determinism-rules.md`.

### 5. Escribir manifest a mano

El agente escribe el YAML directamente basándose en
`references/manifest-anatomy.md`. No invocar `doppel freeze` (no existe);
usar la CLI solo para validar y probar.

Aplicar siempre:

- Paths POSIX relativos (`references/determinism-rules.md`).
- Redacción de secretos (`references/secrets-redaction.md`).
- Versionado semántico si se conoce la versión previa.

### 6. Proponer nombre

Proponer nombre basado en la intención del usuario. Mostrar al usuario el
nombre propuesto para que pueda ajustarlo antes de persistir.

### 7. Validar iterativamente

Tras cada cambio significativo en el YAML:

```bash
doppels validate .doppels/capabilities/<name>.yaml
doppels validate .doppels/recipes/<name>.yaml   # si hay Recipe
```

Durante pruebas, ejecutar cuando sea seguro (muta el host):

```bash
doppels run capability/<name> --input ... --yes
```

Iterar hasta que todo pase. Sin una cadencia fija: el ritmo lo marca la
configuración del agente y del editor del usuario.

### 8. Confirmar con el usuario

Mostrar al usuario, antes de persistir:

- Nombre propuesto (ajustable).
- Inputs declarados.
- Outputs declarados.
- Dependencias (`requires`).
- Secretos referenciados (nunca valores).
- Riesgos (`impact`, `approval`).

Pedir confirmación explícita.

### 9. Persistir

El manifest ya está escrito en disco. No crear commit automático: eso es
decisión del usuario.

## Restricciones

- El agente NUNCA invoca un comando `doppel freeze` generador (no existe).
- El agente NUNCA escribe secretos en el YAML; solo referencias (`from:
  host_env`).
- El agente NUNCA inicia freeze solo porque la tarea “parecía operativa”.
- El agente NUNCA pregunta al final de cada operación si persistir.
- El agente SIEMPRE valida antes de pedir confirmación al usuario.
- Si la CLI no está disponible, abortar y dar instrucciones.
- El manifest resultante debe ser YAML legible y editable, no opaco.

## Edge cases

- **Sesión con errores recuperados**: capturar la versión final corregida, no
  los intentos fallidos.
- **Dependencias externas**: `requires.commands` con `version` si Doppel puede
  detectarla.
- **Sin resultado claro**: pedir aclaración del output esperado al usuario.
- **Sesión ruidosa (>50 comandos)**: reducir el scope antes de freezear o
  pedir aclaración.
- **Capability existente similar**: avisar al usuario y proponer refactor o
  nueva Capability.

## Referencias

- [cli-quickstart.md](references/cli-quickstart.md) — Comandos esenciales.
- [schema-discovery.md](references/schema-discovery.md) — Cómo encontrar schemas.
- [manifest-anatomy.md](references/manifest-anatomy.md) — Anatomía de Capability/Recipe.
- [determinism-rules.md](references/determinism-rules.md) — Reglas de normalización.
- [secrets-redaction.md](references/secrets-redaction.md) — Redacción y referencias.
- [examples/](references/examples/) — Manifests completos de referencia.