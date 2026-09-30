# App and conversation-session subsystem

`internal/app` assembles validated configuration into a `Runtime`, including an
agent manager, approval gate, and a per-channel transport cache shared by all
agents in that runtime.

`SessionManager` owns one default conversation session (`default`, `Default`)
plus API-created sessions, bounded by `api.max_sessions` (default 8). Every
conversation session owns its own runtime, transcript, top-level prompt queue,
and error state. The default session is also the runtime driven by the TUI.

Do not confuse a conversation session with `execution.Session`, which is one
long-lived shell process for an agent.

Top-level human prompts go through the bounded session queue; messages addressed
to a subagent go to that agent's inbox. Queue submission takes the same lock as
the cancellation epoch transition so the stop boundary is atomic. `Cancel`
discards prompts and agent inbox work accepted before that boundary, cancels
active turns, and denies pending approvals. Prompts accepted afterward must
survive. A cancelled session stays open and reusable.

Transcripts are per-session by default through `recorder.NewSessionFile`.
`--log` selects one shared process recorder; an explicitly empty `--log`
disables transcripts.
