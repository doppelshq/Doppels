# RFC 001 — Doppels Runner IPC Protocol (v1)

| | |
|---|---|
| Estado | draft (Fase 0 del plan Desktop-first) |
| Protocolo | `doppels.so/ipc/v1` — `protocolVersion: 1` |
| Implementación | `apps/cli/apps/cli/internal/runner` + `cmd/doppels-runner` |
| Decisiones base | `.memory/decisions/2026-09-11-desktop-runner-decisions.md` (repo internal) |

## 1. Objetivo

Definir el protocolo de comunicación local entre el **Doppels Runner**
(daemon Go persistente) y sus clientes (**Desktop/Tauri** y **CLI**). El
Runner es el único proceso con autoridad de ejecución; los clientes consumen
sus capacidades por IPC.

No-objetivos de v1: proxy de llamadas cloud (`cloud/*` llega en Fase 5),
Named Pipe nativo de Windows (alpha es WSL2 → UDS), fulfillment manual
interactivo (`complete_manual`). Los **live logs sí están en v1** (básicos,
best-effort; ver `v1/subscribeRunLogs`).

## 2. Modelo de dominio: Capability, Node, Identity, Request, Run

Principio: **delegate the request, not the authority**.

| Concepto | Definición |
|---|---|
| **Node** | dónde existe el poder de ejecución: Recipes, credenciales, red, tooling y entorno del host |
| **Identity** | quién tiene autoridad para operar, aprobar o administrar ese poder |
| **Capability** | qué poder se expone (contrato público de inputs/outputs) |
| **Request** | intención de utilizar ese poder; se hace **contra una Capability** |
| **Run** | intento concreto de ejecución en un Node, con Evidence append-only |

Flujo conceptual:

```text
Request → Capability → resolución de Node(s) elegibles
        → autorización / approval (Identity o Policy)
        → ejecución en Node → Run + Evidence
```

Reglas:

- Una Request **no "va a una Identity"**: se hace contra una Capability. Las
  Identities participan como solicitante (`requestedBy`), autorizador o
  administrador — nunca como destino de la Request.
- **Identity ≠ Node**: una Identity puede operar varios Nodes; un Node puede
  ser operado por varias Identities. Ningún extremo posee al otro.
- Una Capability puede estar disponible en uno o varios Nodes elegibles.
- Este protocolo IPC es **node-local**: el Runner ES un Node. La resolución
  de Nodes elegibles cuando hay varios vive fuera de este protocolo (ver
  OPEN QUESTIONS).

### Roles diferenciados

Pueden coincidir en la misma persona; el modelo no los fusiona.

| Rol | Pregunta que responde | Nodo personal | Nodo compartido |
|---|---|---|---|
| ownership | ¿quién administra el Node? | owner | equipo / admins |
| operation | ¿quién puede operarlo (estado, logs, runs)? | owner | operators |
| approval | ¿quién aprueba requests/steps? | owner | roles, grupos o policies |
| execution | ¿dónde ejecuta? | el Node | el Node (siempre) |

### Casos que el modelo debe soportar

1. **Nodo personal** (`This Mac`): operado y aprobado principalmente por su
   owner; capabilities ligadas al entorno personal del usuario. **v1 de este
   protocolo implementa exactamente este caso**: single-user;
   `runner.token` autentica al *cliente* (software), no a una Identity — el
   usuario del SO es la Identity local implícita.
2. **Nodo compartido** (p.ej. `staging-node`): operable por varias
   Identities; approvals y permisos pertenecen a roles, grupos o policies;
   ninguna Identity concreta posee las Requests. **No está en v1**; ver
   OPEN QUESTIONS.

### Terminología en los mensajes

- `source` de un Run (`cli`/`desktop`/`share`/`space`) = **superficie de
  origen**, no una Identity ni un Node.
- `nodeId` de un Run = dónde ejecutó (Run = intento en un Node).
- `v1/decideApproval` atribuye la decisión al "operador local" sin
  granularidad de Identity: suficiente en nodo personal, insuficiente en
  compartido.

### OPEN QUESTIONS

- **OQ-1 — Routing multi-node**: con una Capability disponible en varios
  Nodes elegibles, ¿quién resuelve y con qué estrategia (control plane vs
  cliente; capacidad, etiquetas, afinidad, round-robin)? ABIERTA — fuera del
  scope node-local de este protocolo.
- **OQ-2 — Identity en IPC para nodos compartidos**: ¿token por Identity,
  delegación del control plane, sesión del Desktop? v1 no distingue
  Identities. ABIERTA.
- **OQ-3 — Policies locales de approval**: quién puede aprobar qué en un
  nodo compartido. ABIERTA.
- **OQ-4 — Node como recurso de primera clase en cloud**: hoy `Run.node_id`
  es un string y el control plane asigna Requests a Identities
  (`assigned_to`); esa asignación debería entenderse como mecanismo de
  routing/autorización, no como ownership de la Request. ABIERTA — cambio en
  control plane, no en este protocolo.

## 3. Transporte

| Plataforma | Transporte | Dirección |
|---|---|---|
| macOS / Linux / WSL2 | Unix Domain Socket | `<UserConfigDir>/doppels/runner.sock` |
| Windows nativo (post-alpha) | Named Pipe | `\\.\pipe\doppels.runner` (ACL por usuario) |

- `<UserConfigDir>/doppels/` es el mismo directorio que usa `configstore`
  (`profile.json`, `credentials.json`), con permisos `0700`.
- El socket se crea `0600` propiedad del usuario. Solo procesos del mismo
  usuario pueden conectar.
- **Single instance**: el Runner adquiere el socket al arrancar; si ya existe
  un listener vivo, el proceso recién lanzado termina con código `0` tras
  verificar el handshake (el spawneador simplemente conecta).
- Al hacer `shutdown` o recibir SIGTERM: deja de aceptar conexiones, responde
  las RPC en vuelo, cancela Runs activos (eventos terminales garantizados),
  hace flush del outbox y desvincula el socket.

### Supervisión y ciclo de vida

El Runner **nunca se auto-relanza**. La supervisión es externa y está
definida por contexto:

| Contexto | Supervisor |
|---|---|
| Desktop abierto | Tauri (spawn del sidecar, restart on crash con backoff) |
| Autostart habilitado | OS service (launchd login item / systemd user unit / schtasks) |
| Solo CLI / CI / headless | Nadie: `shutdown` o SIGTERM lo detiene y permanece caído |

`v1/shutdown` (§9) es la vía del protocolo para pedir un arranque limpio;
**quién relanza** es responsabilidad del supervisor, no del Runner. La
acción "restart" de la UI de Desktop = `shutdown` + relaunch por Tauri.

## 4. Framing

NDJSON: una línea UTF-8 por mensaje JSON, terminada en `\n`. Sin long-lived
writes parciales: cada mensaje se escribe completo.

- Tamaño máximo por línea: **4 MiB**. El límite se aplica al payload JSON
  sin contar el `\n`, y es simétrico: lo que un extremo acepta al leer es
  exactamente lo que el otro puede escribir. El Runner nunca emite eventos
  que lo excedan (los payloads de logs van por `getRunLogs` paginado, no por
  eventos).
- Sin compresión, sin batching. Un mensaje JSON-RPC por línea.
- El cliente debe cerrar la conexión ante un frame ilegible (no hay
  mecanismo de resincronización de framing).

Debugging: `nc -U <socket>` o cualquier cliente NDJSON; el Runner puede
loguear frames con `DOPPELS_RUNNER_TRACE=1`.

## 5. Formato de mensaje

JSON-RPC 2.0 exacto, con estas restricciones:

- Campos en **camelCase** (igual que el resto de contratos Doppels:
  `manifestSha256`, `shareId`, …).
- `id`: string o entero; obligatorio en `request`; **ausente** en
  notification. El Runner no acepta batch (array) — error `-32600`.
- `jsonrpc: "2.0"` en todo mensaje.
- Campos desconocidos en cualquier dirección **se ignoran** (política
  aditiva, ver §7).

## 6. Handshake y auth

Todo cliente **debe** enviar `v1/initialize` como primer mensaje. Cualquier
otro método antes del handshake → `-32000 notInitialized`. El Runner cierra
conexiones sin handshake a los 10 s.

Token: `<UserConfigDir>/doppels/runner.token` (0600, 32 bytes hex, creado por
el Runner en el primer arranque; rotación manual borrando el fichero con el
Runner parado). La auth es doble: permisos del socket **y** token correcto.

```json
→ {"jsonrpc":"2.0","id":1,"method":"v1/initialize",
   "params":{"token":"<runner.token>","client":{"name":"doppels-desktop","version":"0.1.0"}}}
← {"jsonrpc":"2.0","id":1,"result":{
     "protocolVersion":1,
     "runnerVersion":"1.7.0",
     "capabilities":["liveLogs"],
     "nodeStatus":{ …NodeStatus… }}}
```

- `protocolVersion` distinta de la que soporta el cliente → el cliente debe
  mostrar error y abortar (`-32002 versionMismatch` lo facilita).
- Features opcionales: comprobar `capabilities` antes de usarlas; ausencia =
  el Runner no la soporta (degradar con gracia, no es error).

## 7. Versionado

- `protocolVersion` es un entero major (`1`); **no existe concepto de
  minor**. Todo lo que no sea rompiente vive dentro de la misma major.
- Descubrimiento de adiciones por **capability negotiation**: el resultado de
  `v1/initialize` incluye `capabilities: string[]` (p.ej. `["liveLogs"]`).
  El cliente consulta ahí antes de usar un método/feature opcional; las
  capacidades solo se añaden o deprecian dentro de una major, nunca se
  cambian de significado.
- Namespace de métodos: `v1/…`. Un método puede deprecarse (presente en
  `capabilities` como `deprecated:<method>`) pero no eliminarse dentro de la
  major.
- Al congelar la implementación, cada mensaje se fija como JSON Schema bajo
  `https://doppels.so/ipc/v1/<name>.schema.json` en `schemas/`, con digest
  de bundle según `schemas/VERSIONING.md`.
- Cambios rompientes → `v2/` + `protocolVersion: 2`; el Runner puede servir
  ambos namespaces en paralelo durante una ventana de migración.

## 8. Tipos compartidos

Notación TypeScript; structs Go canónicas en `internal/runner/proto`, tipos
TS generados (`tygo`) en `packages/proto`.

```ts
type NodeStatus = {
  state: "online" | "degraded";            // degraded = sin cloud esperado, workspace con errores, etc.
  runnerVersion: string;
  protocolVersion: number;
  startedAt: string;                        // RFC3339
  workspaces: WorkspaceSummary[];
  cloud: { connected: boolean; server: string; organization: string | null } | null;
};
type WorkspaceSummary = {
  root: string;                             // path absoluto del root con .doppels/
  space: string;                            // nombre del Space local
  capabilities: number;
  recipes: number;
  gitBranch: string | null;
  health: "ok" | "invalidManifests" | "missingRoot";
};
type CapabilitySummary = {
  name: string; version: string;            // metadata.version del manifiesto
  workspace: string;                        // root
  space: string;
  manifestSha256: string;                   // digest del manifiesto (64 hex)
  runtime: "shell" | "manual" | "none";     // resolución de Recipe: automática / manual / sin Recipe
  recipe: { name: string; version: string; manifestSha256: string } | null;
  pin: "pinned" | "stale" | "unpinned";     // estado respecto a doppels.lock
  readiness: { command: string; ok: boolean }[]; // CheckRequires
};
type RunEventPayload = {                    // mismo vocabulario que events.jsonl y cloud RunEvent
  runId: string;
  sequence: number;                         // contiguo desde 0
  type: string;                             // run_created | validation_succeeded | validation_failed
                                            // | approval_requested | step_started | step_succeeded
                                            // | step_failed | run_succeeded | run_failed
                                            // | run_cancelled | run_interrupted
  stepId?: string;
  data: Record<string, unknown>;
  occurredAt: string;                       // RFC3339
};
type RunSummary = {
  runId: string; requestId: string;
  capability: string; recipe: string | null;
  nodeId: string;                          // dónde ejecutó (Run = intento en un Node; §2)
  status: "running" | "succeeded" | "failed" | "cancelled" | "interrupted" | "pendingManual";
  source: "local" | "cli" | "desktop" | "share" | "space"; // superficie de origen, no Identity (§2)
  workspace: string;
  createdAt: string; finishedAt: string | null;
};
```

Invariantes heredados: secuencia contigua, terminal único, RunEvent nunca
reescribe historial (decisiones `run-event-terminal-invariants`,
`run-result-contract`).

## 9. Métodos v1

| Método | Params → Result | Notas |
|---|---|---|
| `v1/initialize` | §6 | obligatorio primero |
| `v1/ping` | `{}` → `{ pong: string }` | keepalive |
| `v1/getNodeStatus` | `{}` → `NodeStatus` | |
| `v1/subscribeNode` | `{}` → `NodeStatus` (snapshot) | luego notifications `v1/nodeEvent` |
| `v1/listWorkspaces` | `{}` → `WorkspaceSummary[]` | |
| `v1/addWorkspace` | `{ root }` → `WorkspaceSummary` | descubre+valida; idempotente |
| `v1/removeWorkspace` | `{ root }` → `{}` | no borra nada del disco |
| `v1/listCapabilities` | `{ workspace? }` → `CapabilitySummary[]` | sin workspace = agrega todos |
| `v1/getCapability` | `{ workspace, name, version? }` → `{ summary, manifest, inputs, outputs, runs: number }` | manifest completo |
| `v1/startRun` | ver abajo → `{ requestId, runId }` | idempotente por `idempotencyKey` |
| `v1/cancelRun` | `{ runId, reason? }` → `{ status }` | idempotente; garantiza evento terminal |
| `v1/getRun` | `{ runId, includeEvents? }` → `{ summary, request, events? }` | |
| `v1/listRuns` | `{ workspace?, capability?, status?, limit?, cursor? }` → `{ runs: RunSummary[], nextCursor? }` | orden `createdAt DESC` |
| `v1/getRunLogs` | `{ runId, stepId?, offset?, limit? }` → `{ files: { stepId, stream, path, size, truncated }[], content? }` | `content` solo con `stepId`; pages ≤ 4 MiB |
| `v1/subscribeRun` | `{ runId, fromSequence? }` → `{ events: RunEventPayload[], status }` | replay + luego notifications |
| `v1/subscribeRunLogs` | `{ runId, stepId? }` → `{ active: boolean }` | luego notifications `v1/runLog` (capability `liveLogs`) |
| `v1/listPendingApprovals` | `{}` → `{ runId, stepId, name, requestedAt }[]` | approvals HITL pendientes |
| `v1/decideApproval` | `{ runId, stepId, decision: "approve" \| "reject" }` → `{}` | |
| `v1/shutdown` | `{ reason? }` → `{}` | graceful shutdown; el supervisor decide relanzar (ver §3). conexión cerrada tras ack |

`v1/startRun`:

```json
{ "workspace": "/Users/ada/dev/acme",
  "capability": "db/backup",           // "name" o "name@version"
  "recipe": "backup-nightly",           // opcional; si omitted → ResolveRecipe determinista
  "inputs": { "environment": "staging" },
  "approvalMode": "interactive",       // "interactive" = HITL vía decideApproval; "auto" = auto-aprueba steps
  "idempotencyKey": "deploy-2026-09-11" }
```

- Validación de inputs contra el contract de la Capability antes de crear el
  Run; fallo → `-32008 invalidInputs` con diagnósticos en `error.data`.
- Con `doppels.lock` stale y capability pinneada → `-32009 stalePin`
  (espeja `--strict` de la CLI).
- `source` del Run derivado del `client.name` del handshake (`cli`/`desktop`).

## 10. Events (notifications)

Llegan como requests JSON-RPC **sin `id`**; el cliente no responde.

- `v1/runEvent` — payload `RunEventPayload` (§8). Solo a subscribers activos
  del run y **siempre** tras haberse persistido en `events.jsonl` + índice
  (persistencia antes que notificación, igual que el engine hoy).
- `v1/nodeEvent` — `{ kind, payload }` con `kind ∈
  { workspaceAdded, workspaceRemoved, workspaceChanged, runStarted,
  runFinished, approvalPending, cloudConnected, cloudDisconnected,
  nodeDegraded }`.
- `v1/runLog` — **live logs** (best-effort, capability `liveLogs`):
  `{ runId, stepId, stream: "stdout" | "stderr", data: string, truncated: boolean }`.
  Chunks UTF-8 incrementales en orden por stream; aplica la misma redacción
  de secretos y el cap de 16 MiB por stream que el engine (`truncated: true`
  en el último chunk antes de cortar). Semántica best-effort: puede perderse
  output sin señalar gap; el fichero completo y autoritativo se recupera con
  `getRunLogs`. La implementación tee-incremental usa el fan-out de
  `Options.Stdout/Stderr` del engine.

Semántica de entrega:

- `subscribeRun` devuelve primero el **replay** (`events` desde
  `fromSequence`, por defecto 0) y después el flujo en vivo, sin duplicados
  ni huecos (suscripción atómica respecto al event loop del run).
- Buffer por subscriber: 1024 eventos / 4 MiB. Si desborda (cliente lento),
  el Runner envía `v1/nodeEvent { kind: "runEventGap", payload: { runId,
  fromSequence } }` y deja de emitir ese run; el cliente resincroniza con
  `getRun { includeEvents }` y re-suscribe con `fromSequence`.
- Desconexión = unsubscribe implícito de todo. Reconexión = nuevo
  `initialize` + re-subscripciones.
- Orden por run garantizado por `sequence`; entre runs no hay orden global.

## 11. Códigos de error

JSON-RPC estándar: `-32700` parse, `-32600` invalid request (incluye batch),
`-32601` method not found, `-32602` invalid params, `-32603` internal.

Rango Doppels `-32000..-32099`:

| Código | name | Cuándo |
|---|---|---|
| -32000 | `notInitialized` | método antes de `v1/initialize` |
| -32001 | `authFailed` | token ausente/incorrecto |
| -32002 | `versionMismatch` | `protocolVersion` incompatible (data: `{expected, supported}`) |
| -32003 | `workspaceNotFound` | root no registrado / sin `.doppels/` |
| -32004 | `capabilityNotFound` | nombre/version no resuelto en el workspace |
| -32005 | `recipeAmbiguous` | varias Recipes y ninguna seleccionada (data: candidatos) |
| -32006 | `runNotFound` | |
| -32007 | `approvalNotFound` | no pendiente, ya decidida o de otro step |
| -32008 | `invalidInputs` | contract violation (data: diagnósticos) |
| -32009 | `stalePin` | lock stale en modo estricto |
| -32010 | `busy` | operación rechazada por estado (p.ej. shutdown en curso) |

El cliente CLI mapea estos códigos a sus exit codes existentes
(`ExitContract`, `ExitOperational`, …) — `--json` de la CLI no cambia.

## 12. Concurrencia, idempotencia, cancelación

- Múltiples clientes simultáneos (Desktop + CLI): Runs de todos los clientes
  visibles vía `listRuns`/`subscribeRun`; no hay aislamiento por cliente.
- `startRun` con `idempotencyKey`: **persistido**, no en memoria. La clave se
  guarda en `runs.db` (tabla `idempotency`: `(workspace, capability, key) →
  { run_id, request_fingerprint }`) y vive tanto como el historial del Run;
  sobrevive a reinicios del Runner, de la máquina y a versiones.
  `request_fingerprint` = SHA-256 del JSON canónico (claves ordenadas
  recursivamente, UTF-8, sin espacios) de
  `{ capability: "name@version", recipe, inputs }`.
  Forma canónica (obligatoria para cualquier cliente, incluido el futuro
  cliente Rust):
  - Claves ordenadas por sus bytes UTF-8 ya decodificados, de forma
    recursiva; `"\u0062"` y `"b"` son la misma clave.
  - **Claves duplicadas: rechazo.** Un payload cuyo significado dependa de
    si el parser conserva la primera o la última ocurrencia no puede tener
    fingerprint estable.
  - Números: expansión decimal exacta, sin exponente, sin ceros a la
    izquierda ni ceros finales de la parte fraccionaria, signo conservado y
    `-0` → `0`. Nunca se pasa por `float64`: `9007199254740993` y
    `9007199254740992` son distintos. `1`, `1.0` y `1e0` son el mismo
    número y colisionan a propósito.
  - Un exponente cuya expansión supere 1024 dígitos (o la longitud del
    literal original, si es mayor) se rechaza con `-32602`: `1e100000` son
    ocho bytes en el wire y 100 KB al expandir.
  Retry con misma clave y **mismo fingerprint** → devuelve el
  `{requestId, runId}` original; misma clave con **fingerprint distinto** →
  `-32602` (protege contra repetir una operación peligrosa tras un restart).
  Espeja la idempotencia `[space, key]` del cloud, incluida su comprobación
  de equivalencia de payload.
- `cancelRun` idempotente: sobre run terminado devuelve el estado actual sin
  error. La cancelación materializa `run_cancelled`/`run_interrupted`
  (semántica existente del engine, SIGKILL al process group incluido).
- Aprobaciones: con `approvalMode: "interactive"`, el Run queda bloqueado en el
  step hasta `decideApproval`, timeout configurable del Runner (default:
  sin timeout; `run_cancelled` si el Run se cancela).

## 13. Modelo de seguridad

- Superficie local, mismo usuario: socket 0600 + token 0600.
- El token nunca se loguea ni viaja fuera de la máquina. No hay tokens de
  cloud en el protocolo v1 (Fase 5 decide el proxy `cloud/*`).
- Inputs, env (`host_env` redacted), logs y artifacts mantienen las
  garantías del engine (`internal/execution`): confinamiento de paths,
  redacción de secretos, caps de 16 MiB por stream.
- `shutdown` y `addWorkspace` son las únicas operaciones con efecto de
  lifecycle/fs; ambas requieren handshake válido.

## 14. Transcripciones de ejemplo

Run feliz:

```text
→ v1/initialize … ← result {protocolVersion:1, …}
→ v1/startRun {…}                           ← {requestId, runId:"r-123"}
→ v1/subscribeRun {"runId":"r-123"}
← result {events:[run_created,…], status:"running"}
→ v1/subscribeRunLogs {"runId":"r-123"}
← v1/runLog {runId:"r-123",stepId:"dump",stream:"stdout",data:"dumping…"}
← v1/runEvent {runId:"r-123",sequence:3,type:"step_started",…}
← v1/runEvent {runId:"r-123",sequence:7,type:"run_succeeded",data:{returns…}}
```

Aprobación HITL:

```text
← v1/runEvent {type:"approval_requested", stepId:"migrate"}
← v1/nodeEvent {kind:"approvalPending"}
→ v1/listPendingApprovals ← [{runId, stepId:"migrate", …}]
→ v1/decideApproval {runId, stepId:"migrate", decision:"approve"} ← {}
← v1/runEvent {type:"step_started", stepId:"migrate", …}
```

Reconexión tras gap:

```text
(conexión perdida durante 2 eventos)
→ v1/initialize → v1/subscribeRun {"runId":"r-123","fromSequence":4}
← result {events:[seq 4, 5, …], status}
```

## 15. Conformance

Un cliente v1 **debe**: handshake primero; ignorar campos y notifications
desconocidos; comprobar `capabilities` antes de usar features opcionales
(p.ej. `liveLogs`) y degradar con gracia si ausentes; resincronizar tras
`runEventGap`; re-subscribir tras reconexión; tratar `-32002` como error
fatal con mensaje accionable.

El Runner **debe**: rechazar pre-handshake (`-32000`); cerrar sin handshake
a los 10 s; persistir antes de notificar; garantizar secuencia contigua y
evento terminal; respetar los límites de framing.

Suite de conformance: escenarios JSON fijos (§14 + errores) ejecutados por
el cliente Go (`internal/runnerclient`) y el cliente Rust de Tauri contra el
mismo servidor en CI.

## 16. Historial

- v1 (draft, 2026-09-11): primera edición. Pendiente de implementación
  (Fases 1–2 del plan); al congelarse se fijan los JSON Schemas en
  `schemas/` con `$id` `https://doppels.so/ipc/v1/…`.
- v1 (draft, rev 2026-09-11): §2 Modelo de dominio (Capability/Node/
  Identity/Request/Run, roles ownership/operation/approval/execution, OQ-1
  a OQ-4); `nodeId` en `RunSummary`; `source` aclarado como superficie de
  origen.
