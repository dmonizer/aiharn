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

### Sessions (`internal/app`, `internal/webapi`)

"Session" is overloaded in this codebase; disambiguate before changing either
meaning:

- **Execution session** (`internal/execution.Session`): one long-lived, stateful
  shell per agent. Unrelated to the API's "session".
- **Conversation session**: the top-level agent plus its subagent tree, its
  history, and its transcript — what `GET /api/v1/session` returns. (The TUI
  never uses the word for the conversation; its "session" mentions are all about
  the execution-shell pane.)

There is exactly one conversation session per process today, and that is
load-bearing:

- `Manager.RegisterTop` rejects a second depth-0 agent, so one `Manager` means
  one top-level agent means one conversation session.
- Exactly three routes exist, registered in `internal/webapi/api.go`: `GET
  /api/v1/session` (optional `?agent_id=` addresses a subagent), `POST
  /api/v1/messages`, and `POST /api/v1/approvals/{id}`. There is no session
  list, create, switch, or delete.
- `webapi` binds one `Agent` + `Manager` + `Gate` at construction
  (`cmd/aiharn/main.go`) and owns the single message queue (`chan string` plus a
  worker goroutine), so `queued_messages` and `last_error` are server-global
  rather than per-agent, and are reported only when the top-level agent is
  selected.
- `cmd/aiharn/main.go` creates one `recorder` per process and passes it as every
  agent's observer, so the top-level agent and all subagents append to a single
  transcript file. Approval ids are sequential per `Gate`, so they are unique
  only within that gate.
- `docs/remote.md` records the MVP boundary: previous-session discovery and
  selection are deliberately deferred to a later API version.

Planned and awaiting approval; none of the following exists yet:

- a session layer in `internal/app` where each session owns its own `Runtime`
  (own `Manager`, `Gate`, transports, recorder, and message queue);
- `GET`/`POST /api/v1/sessions` plus `PATCH`/`DELETE /api/v1/sessions/{id}`;
- an optional `session_id` on the three existing routes, defaulting to the
  default session (the one the TUI drives), so v1 clients keep working;
- a new `[api] max_sessions` cap;
- a web sidebar that lists session names, with API endpoint and token
  configuration moved into a settings dialog.

### Web console (`web/`)

`web/app.js` is a single IIFE and `web/index.html` carries no inline script or
style plus a strict CSP (`script-src 'self'`, so no inline handlers and no
`eval`); new UI must be `addEventListener`-based code in `app.js`/`styles.css`.
State lives in memory plus `localStorage` (endpoint list and active selection)
and `sessionStorage` (bearer token, keyed per endpoint). The transcript is
rendered from a full `GET /api/v1/session` snapshot polled once per second —
there is no streaming, and the client keeps no history of its own.

### Test doubles

`internal/testutil/{llm,execution,approval}` hold fakes for the interfaces above;
use them for unit tests of the agent/tools/app layers rather than real services.
