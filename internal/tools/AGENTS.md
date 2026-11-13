# Tools subsystem

The registry exposes `execute_command`, `set_approval`, the memory tools
(`write_memory`, `list_memories`, `get_memory`), `set_approval`, and the
subagent tools. The memory tools take a `memory.Backend` implemented by the
session runtime; their indexes are allocated by that backend and must be unique.
`SubagentBackend` is declared here and implemented by `agent.Manager`; this
interface is the boundary that prevents a tools-to-agent import cycle. Never
import `internal/agent` from this package.

`execute_command` runs against an execution session. Each invocation gets a
fresh subshell, so its model-facing description must continue to explain that
cwd, exported variables, functions, and shell options do not persist between
calls. Approval is required unless the gate is in `allow-all` mode.

Prefer the fakes under `internal/testutil` for unit tests rather than real model,
transport, or approval services.

## Text file tools

`read`, `write`, and `edit` (files.go) are thin adapters over `filetools`:
schemas, model-facing descriptions, approval, and JSON results live here, while
line handling, receipts, and publication live in `internal/filetools`. They reach
the runtime through `FileBackend`, which the app implements over one agent's
`filetools.Session`; the app also owns channel selection, so the tools never see
channel names, roots, or generations.

`read` runs without approval. `write` and `edit` call the gate explicitly:
`Propose*` validates and returns the exact unpublished result (with a bounded
preview), the gate decides, and `Commit` revalidates and publishes. A denial or
cancellation returns a `filetools` error with a stable code
(`approval_denied`, `cancelled`) and publishes nothing. Preview text goes into
`approval.Request.Command`; `Args` stays compact and never contains file content.
