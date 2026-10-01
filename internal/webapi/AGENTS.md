# Web API subsystem

The API binds a `sessions.Store`; all mutable work belongs to the addressed
conversation session. Optional `session_id` defaults to the default session, and
optional `agent_id` selects an agent view within it.

Registered routes:

- `GET /api/v1/session`
- `POST /api/v1/messages`
- `POST /api/v1/approvals/{id}`
- `POST /api/v1/tool-limits/{id}`
- `POST /api/v1/session/approval`
- `POST /api/v1/session/channel`
- `POST /api/v1/session/loop`
- `POST /api/v1/session/cancel`
- `POST /api/v1/session/clear`
- `GET`/`POST /api/v1/sessions`
- `GET`/`PATCH`/`DELETE /api/v1/sessions/{id}`

The cancel endpoint mirrors TUI stop-everything behavior. `loop_active` exposes
the top-level inbox-loop setting. `queued_messages` and `last_error` are returned
only when the top-level agent is selected.

`pending_agent_messages` remains an always-present array for polling/API
compatibility, but UI clients intentionally do not render it. Delivered
agent-authored history items carry `origin` and `delivery` metadata so clients
can render them exactly once and separately from human messages.

Capabilities advertise optional routes/features. Keep session snapshots and
session descriptors consistent when adding or changing a capability. API
handlers must use the injected store rather than server-global queue/error state.
