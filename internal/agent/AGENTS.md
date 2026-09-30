# Agent subsystem

The agent lifecycle is `starting → running → idle → closed | errored`.
`Manager` owns the agent tree, assigns ids, enforces `max_agent_depth`,
`max_open_agents`, and `allow_subagents`, routes inbox messages, and implements
the tool layer's `SubagentBackend` interface.

Each conversation session starts a top-level inbox loop by default. An
agent-authored inbox message becomes its own turn as soon as the recipient is
available. A human top-level turn is driven separately by the app session queue
and must not drain the agent inbox. The loop can be toggled through
`SetMainLoopActive`, the TUI shortcut, or the web API.

Agent-to-agent enqueueing is a scheduling detail, not a transcript event.
`EventAgentMessage` is emitted exactly once, when the message enters recipient
history, with its `llm.Delivery` metadata. `pendingInbox` and
`Manager.PendingMessages` remain internal/API polling state; do not reintroduce
enqueue/delivery duplicate rendering.

Stop-all cancellation increments each agent's work epoch, cancels its active
turn, and discards every inbox item accepted before the boundary. The epoch also
invalidates an item already taken by a loop but still waiting for `turnMu`, where
no `turnCancel` handle exists yet. The epoch check and `turnCancel` publication
must remain atomic under the agent mutex. Messages accepted after the boundary
remain runnable, and agents plus execution sessions stay reusable.

Use the LLM, execution, and approval fakes under `internal/testutil` for unit
tests. Add race-focused regression coverage when changing inbox scheduling,
turn serialization, or cancellation.
