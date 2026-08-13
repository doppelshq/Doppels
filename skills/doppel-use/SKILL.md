---
name: doppel-use
description: Habilita al agente para consultar, ejecutar o crear Capabilities dentro del scope local o del Space activo, con auditabilidad y aprobación humana. PLACEHOLDER: requiere MCP server de Doppel, todavía no implementado.
status: draft
---

# doppel use — PLACEHOLDER

Esta skill está bloqueada hasta que el MCP server de Doppel esté definido e
implementado. No debe activarse todavía.

## Dependencias pendientes

- MCP server `doppel-mcp` con tools: `doppel_list`, `doppel_describe`,
  `doppel_run`, `doppel_share`, `doppel_freeze`.
- Definición de permisos por Space (lectura, solicitud, aprobación, ejecución).
- Transporte entre agente y CLI local.

## Comportamiento previsto

| Frase del usuario | Acción |
|---|---|
| "use doppel" | Habilitar el agente para consultar/ejecutar Capabilities |
| "list capabilities" | `doppel_mcp.list()` |
| "run <capability>" | `doppel_mcp.run()` con confirmación humana |
| "share <capability>" | `doppel_mcp.share()` |

## Restricciones esperadas

- Toda acción externa al host requiere confirmación humana.
- Toda ejecución con `impact: high | critical` requiere aprobación.
- Secretos nunca se exponen en respuestas del agente.
- Logs y outputs se redactan antes de salir al agente.

## TODO

1. Diseñar contrato MCP server (tools, transport, autenticación).
2. Definir permisos por Identity y Space.
3. Implementar redacción de logs en respuestas al agente.
4. Validar integración con Claude Code, Cursor, Codex.

## Referencias

Esta skill no debe incluirse en releases hasta que el MCP server esté
publicado. Ver [TODO.md](../../TODO.md) en la raíz del repo para el estado
actual.