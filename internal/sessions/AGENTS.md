# Session interfaces

This package declares the `Handle` and `Store` boundaries consumed by the web
API and implemented by `internal/app` as `Session` and `SessionManager`.

Keep the interfaces about conversation sessions, not execution shells. A handle
exposes its top-level agent, manager, approval gate, model/channel metadata,
bounded prompt queue, cancellation, and last error. `queued_messages` and
`last_error` are session-local and are reported only for the selected top-level
agent.

Preserve compatibility for callers that omit `session_id`: API operations must
continue to default to the default conversation session.
