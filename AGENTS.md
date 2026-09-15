# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make build    # go build -o aiharn ./cmd/aiharn
make vet      # go vet ./...
make test     # go test ./...
make race     # go test -race ./...
make run      # build and run ./aiharn
```

Run a single test (path-scoped so only that package's tests execute):

```sh
go test ./internal/agent/ -run TestName -v
```

The `internal/execution/ssh/harness` package provides an in-process SSH server used
by the transport/session tests, so those tests run without a real SSH daemon.

## Architecture

Aiharn is an AI agent harness: a Bubbletea TUI drives an agent loop that calls an
LLM, which may invoke tools that execute commands on an execution channel, gated by
an approval prompt. `docs/plan.md` is the canonical design document; `stopped.md`
records completed-phase notes that post-date it.

Packages depend only in one direction (acyclic):

```
config → llm → execution → approval → tools → agent → app → tui / webapi
```

`internal/config` sits at the root and imports nothing from the rest of the project.
It loads TOML, interpolates `${VAR}` from the environment, resolves local paths
(relative to the config file; `~` expands only for local paths), applies defaults,
and semantically validates. Secret-bearing fields (`api_key`, `password`) are
redacted in every error/log boundary. Committed `config.toml` references only
environment variables; real credentials go in git-ignored `*.local.toml`.

### Execution abstraction (`internal/execution`)

The core seam is two interfaces in `internal/execution/transport.go` and
`session.go`: `Transport` (connection lifecycle) and `Session` (a persistent,
stateful shell that runs one command at a time and returns a `Result` of stdout,
stderr, and exit code). `ErrSessionReset` marks an unrecoverable session that
must be discarded. Implementations satisfy the interface via a compile-time
assertion (`var _ execution.Transport = (*Transport)(nil)`).

- `internal/execution/ssh` — SSH transport (golang.org/x/crypto/ssh), with
  host-key verification via knownhosts, password / key-file / SSH-agent auth,
  and OpenSSH `~/.ssh/config` alias resolution when a channel specifies only
  name+host.
- `internal/execution/local` — runs commands on this machine via `os/exec` with
  `Setsid` and process-group kill, mirroring SSH semantics. Requires no host,
  user, or auth fields.
- `internal/execution/shell` — shared marker-based framing protocol that both
  transports use. A command is base64-encoded and run via `setsid bash -c` in the
  background; unique `AIHARN-BEGIN/END/ERR` markers plus a 128-bit nonce separate
  stdout, stderr, and exit code without shell quoting.

The framing protocol is subtle (marker uniqueness, truncation, process-group
signal handling, cancellation without leaving the session unusable). Before
changing it, read `internal/execution/shell/framing.go` and both transports'
`Exec` implementations together; keep them in sync.

### LLM (`internal/llm`)

`internal/llm/types.go` defines the provider-neutral contract (`Item`,
`Request`, `Client.Stream`) and a streaming event model (TextDelta → terminal
Completed/Failed). Two adapters translate it to wire protocols:
`responses` (OpenAI Responses API) and `chatcompletions` (Chat Completions API).
The rest of the program never imports SDK types. The agent owns conversation
history and rebuilds a full stateless request each turn.

### Tools and approval

`internal/tools` defines the callable tools (`execute_command`, `set_approval`,
and five subagent tools) behind a registry. The `SubagentBackend` interface is
declared in `tools` and implemented by `agent.Manager` — this is what breaks the
agent↔tools import cycle (`tools` never imports `agent`).

`internal/approval` is a `Gate` that prompts the user before a command runs.
`internal/tools/set_approval.go` lets the model switch the gate between `ask` and
`allow-all`.

### Agent and app

`internal/agent` is the agent loop (`starting → running → idle → closed | errored`)
plus a `Manager` that owns all agents, assigns ids, enforces limits
(`max_agent_depth`, `max_open_agents`, `allow_subagents`), and routes subagent
inbox messages. `internal/app` assembles everything from validated config into a
`Runtime`, including a per-channel transport cache shared by all agents.

`internal/tui` is the Bubbletea UI; `internal/webapi` (with `web/`) is the
optional current-session HTTP API and static frontend (see `docs/remote.md`).

### Test doubles

`internal/testutil/{llm,execution,approval}` hold fakes for the interfaces above;
use them for unit tests of the agent/tools/app layers rather than real services.
