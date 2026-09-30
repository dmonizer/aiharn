# Tools subsystem

The registry exposes `execute_command`, `set_approval`, and the subagent tools.
`SubagentBackend` is declared here and implemented by `agent.Manager`; this
interface is the boundary that prevents a tools-to-agent import cycle. Never
import `internal/agent` from this package.

`execute_command` runs against an execution session. Each invocation gets a
fresh subshell, so its model-facing description must continue to explain that
cwd, exported variables, functions, and shell options do not persist between
calls. Approval is required unless the gate is in `allow-all` mode.

Prefer the fakes under `internal/testutil` for unit tests rather than real model,
transport, or approval services.
