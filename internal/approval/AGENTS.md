# Approval subsystem

`Gate` serializes approval requests and lets the TUI or web API decide them.
Pending notifications are UI wake-up hints; the gate's internal id map is the
canonical queue. Requests may be resolved by any client or by context
cancellation, so preserve idempotent, concurrency-safe decision behavior.

Approval ids are sequential per gate and therefore unique only within one
conversation session. `tools/set_approval.go` exposes switching between `ask`
and `allow-all`; existing pending requests still require a decision.
