# onix-orchestrator

**Plano de control** de OnixGuard. Escrito en **Go**. Expone el **servidor MCP** que usan los agentes, maneja la **pausa/reanudación**, el flujo de **12 etapas** y las **alertas**; a futuro, el canal de control remoto de la PC.

> **Estado (Fase 3 ✅):** implementado. Servidor **MCP** (streamable HTTP en `:8084/mcp`, lib `mark3labs/mcp-go`) con `reportar_al_jefe` / `pedir_al_jefe` / `consultar_plan`. Las dos primeras son **bloqueantes**: crean el reporte, pausan la sesión y no retornan hasta que el jefe responde (vía gateway) o expira el timeout. HTTP de control en `:8085` (`POST /respond`, `/healthz`). Verificado E2E (2026-09-30): reportar → `pausada` → responder → la llamada MCP retorna `{action,message}` → `activa`.

## Cómo identifica al agente
El agente (Claude Code) se conecta al MCP con cabeceras `X-Onix-Session`, `X-Onix-Project`, `X-Onix-Role`; el orchestrator las lee (vía `WithHTTPContextFunc`) para registrar `session`/`agent` y crear el reporte.

## Estructura
```text
cmd/onix-orchestrator/main.go   # MCP server + control HTTP + tools
internal/store/                 # reports, sessions, stages, messages (pgx)
internal/pending/               # registro en memoria de llamadas MCP bloqueadas (report_id→chan)
Dockerfile                      # multi-stage (golang:1.26 → distroless)
```

## Puertos y env
`MCP_PORT` (8084), `PORT` control (8085), `NATS_URL`, `DATABASE_URL`, `REPORT_TIMEOUT_MIN` (15).

> ⚠️ Cloudflare corta conexiones idle (~100s): el MCP bloqueante necesita, en prod, keepalive del transporte streamable + timeouts altos en nginx/Cloudflare, o migrar a long-poll con token de reanudación. El `REPORT_TIMEOUT_MIN` garantiza que la llamada **no cuelga para siempre** (retorna `{action:"timeout"}`).

---

## Responsabilidad

```mermaid
sequenceDiagram
  participant A as Agente (Claude Code)
  participant O as onix-orchestrator (MCP)
  participant G as onix-gateway
  participant J as Jefe (panel)

  A->>O: reportar_al_jefe(stage, summary, …)
  Note over O: crea reporte · sesión = pausada<br/>la llamada MCP NO retorna
  J->>G: Aprobar / Pedir cambios / Responder
  G->>O: acción del jefe
  O-->>A: retorna {action, message}  (reanuda)
```

## Herramientas MCP (para los agentes)

- **`reportar_al_jefe(stage, summary, deliverables[], decisions[])`** → crea el reporte, marca la sesión `pausada` y **no retorna** hasta que el jefe responde; devuelve `{action, message}`.
- **`pedir_al_jefe(question)`** → mismo patrón bloqueante; devuelve `{message}`.
- **`consultar_plan()`** → devuelve el plan de 12 etapas y la etapa actual.

Los shapes de entrada/salida están en `onix-contracts/schemas/mcp.methods.schema.json`.

## Cómo funciona la pausa

Pausar/reanudar = **la llamada MCP se bloquea** (con timeout configurable y estado persistido en `sessions.status`), **no** se mata el proceso del agente. La respuesta del jefe llega vía `onix.ctrl.<sesión>` y retorna la llamada pendiente.

> ⚠️ Riesgo de red: Cloudflare corta conexiones idle (~100s). El MCP bloqueante necesita heartbeat/long-poll o timeouts altos en nginx/Cloudflare (ver `onix-deploy`).

## Contratos

- **Expone:** servidor MCP (HTTPS autenticado) + publica en `onix.ctrl.*`.
- **Lee/escribe:** Postgres (`reports`, `messages`, `sessions`, `stages`).
- **Recibe** acciones del jefe reenviadas por `onix-gateway`.
- **Importa:** `github.com/levapo97-cell/onix-contracts/go/onixcontracts`.

## Estructura (futura)

```text
cmd/onix-orchestrator/main.go
internal/mcp/  internal/ctrl/  internal/store/
Dockerfile
```

---

*Parte de OnixGuard. Ver el plan en `OnixGuard/docs/PLAN.md` §1, §4 (MCP), §6 (flujo) y §7.*
