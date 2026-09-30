# Terminal UI subsystem

The Bubbletea TUI drives the default conversation session and renders agent
events, approvals, tool-limit prompts, the roster, and execution-shell panes.
Its uses of “session” generally mean the execution shell pane, not an API
conversation session.

Esc while work is active is stop-everything: cancel the top-level turn and all
agent turns, discard queued top-level prompts and agent inbox work, and clear or
deny blocking approval/tool-limit UI. Agents remain reusable afterward.

Render agent-to-agent traffic only from delivered `EventAgentMessage` events.
The direction header (`↑` ancestor, `↓` descendant) and delivery kind must remain
visually distinct from human and assistant messages. Do not render pending inbox
entries or recreate enqueue/delivery duplicates.

Keep key handling, bridge notifications, and late cancellation notifications
safe under concurrent state changes. Use Bubbletea commands for asynchronous
work rather than blocking `Update`.
