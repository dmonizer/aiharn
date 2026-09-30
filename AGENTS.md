# AGENTS.md

Repository-wide guidance for coding agents working on Aiharn. Read this file
first, then read every linked subsystem guide relevant to the files you will
change. A nested `AGENTS.md` extends these root instructions for its directory.

## Commands

```sh
make build    # go build -o aiharn ./cmd/aiharn
make vet      # go vet ./...
make test     # go test ./...
make race     # go test -race ./...
make run      # build and run ./aiharn
```

Run one test with a package-scoped command:

```sh
go test ./internal/agent/ -run TestName -v
```

The `internal/execution/ssh/harness` package supplies an in-process SSH server,
so transport and session tests do not require a real SSH daemon.

## Architecture

Aiharn is an AI agent harness: a Bubbletea TUI drives an agent loop that calls
an LLM, which may invoke tools that execute commands on an execution channel,
gated by an approval prompt. `README.md` covers user-facing setup and `BUGS.md`
tracks known issues.

Packages depend only in this direction; preserve the acyclic boundary:

```text
config → llm → execution → approval → tools → agent → app → tui / webapi
```

## Subsystem index

- [Configuration](internal/config/AGENTS.md)
- [LLM provider-neutral contract and adapters](internal/llm/AGENTS.md)
- [Execution transports, sessions, and framing](internal/execution/AGENTS.md)
- [Approval gate](internal/approval/AGENTS.md)
- [Tool registry and subagent tool boundary](internal/tools/AGENTS.md)
- [Agent loop and manager](internal/agent/AGENTS.md)
- [Runtime assembly and conversation sessions](internal/app/AGENTS.md)
- [Session interfaces](internal/sessions/AGENTS.md)
- [Terminal UI](internal/tui/AGENTS.md)
- [Web API](internal/webapi/AGENTS.md)
- [Web console](web/AGENTS.md)
- [Test doubles](internal/testutil/AGENTS.md)

For cross-subsystem changes, read all affected guides before editing. Keep
provider SDK types inside LLM adapters, transport details behind execution
interfaces, and agent implementation details out of `tools`.
