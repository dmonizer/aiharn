# aiharn

Aiharn is an AI agent harness. A terminal UI (TUI) drives an agent loop that
calls an LLM, which may invoke tools that execute commands on an execution
channel (local or SSH), gated by an interactive approval prompt. A companion
web console and HTTP API expose the same session for browser-based control.

## Features

- Bubbletea TUI with streamed reasoning, markdown rendering, a shell pane,
  approval prompts, and a subagent roster.
- Local and SSH execution channels, each with per-agent long-lived shell
  sessions and marker framing.
- LLM adapters for OpenAI Responses and OpenAI-compatible Chat Completions.
- Approval gate (`ask` / `allow-all`) plus a `set_approval` tool that can only
  tighten permissions.
- Agent/subagent tree with depth and open-agent limits, typed subagents, and
  inbox messaging.
- Persistent, scoped memory (`local` / `global`) via `write_memory`,
  `list_memories`, and `get_memory`.
- Skill index template expansion and a `/skill install` TUI command.
- Optional HTTP API and web console with bcrypt basic-auth, multiple sessions,
  channel switching, approvals, and tool-call limits.
- JSON-Lines session transcripts under `<aiharn_home>/transcripts`.

## Build

Requires Go.

```sh
make build    # go build -o aiharn ./cmd/aiharn
```

or

```sh
go install ./cmd/aiharn
```

To embed a version string:

```sh
go build -ldflags "-X main.version=..." -o aiharn ./cmd/aiharn
```

## Quick start

Run `aiharn` with no `--config`. If `~/.aiharn/config.toml` does not exist,
Aiharn creates a private starter config plus `~/.aiharn/prompts/main.md`, then
reports the blank API key through normal configuration validation. An explicit
`--config` path is never created automatically.

Edit `~/.aiharn/config.toml` to set a model, then start the TUI:

```sh
./aiharn
```

`config.toml.example` documents every option.

## Configuration

Configuration is TOML, at `~/.aiharn/config.toml` by default. Rules:

- The file is capped at 4 MiB; unknown fields are errors.
- Relative local paths (system prompts, SSH key files, `known_hosts`,
  `aiharn_home`, `api.auth_file`) resolve relative to the config file's
  directory.
- Tilde (`~`) expands only for local paths.
- `${VAR}` interpolates from the environment; a missing referenced variable is
  fatal.
- Secret-bearing fields (`api_key`, `password`) are redacted from errors and
  logs.
- Start from `config.toml.example`; `config.toml` and `*.local.toml` are
  git-ignored, so put real credentials in a local file or reference them via
  `${VAR}` env interpolation.

### models

```toml
[models.deepseek_flash]
provider = "openai_responses"        # openai_responses | openai_chat_completions
base_url = "https://api.deepseek.com"
api_key  = "${DEEPSEEK_API_KEY}"
model    = "deepseek-v4-flash"
reasoning_effort = ""                # default: provider/model decides
reasoning_summary = ""               # Responses API: auto|concise|detailed
```

### channels

The first channel is the default; override with `--channel`.

```toml
[[channels]]
name = "sf"
type = "ssh"               # ssh | local
host = "sf"
# port, user, auth, known_hosts, insecure, keep_alive, default_shell,
# remote_command, and working_dir are optional.
```

When `user`, `auth`, and `known_hosts` are all unset, `host` is treated as an
OpenSSH `~/.ssh/config` alias. A `local` channel runs commands on the same
machine and needs only `name` and `type = "local"`.

### agents

```toml
[agents.main]
model = "zai_glm"
description = "General coordinator"
system_prompt = "prompts/main.md"
channel = "sf"
tools = "all"              # all | none | "tool1,tool2" | TOML array
allow_subagents = true
```

Available tools: `execute_command`, `write_memory`, `list_memories`,
`get_memory`, `set_approval`, `list_subagent_types`, `spawn_subagent`,
`send_subagent_message`, `send_agent_message`, `check_subagent`,
`list_subagents`, `close_subagent`.

### limits

```toml
[limits]
max_agent_depth = 2          # top-level = depth 0
max_open_agents = 8
command_timeout = "30s"
command_output_bytes = 262144
tool_result_bytes = 65536
inbox_depth = 32
event_capacity = 256
toolcalls_per_turn = 64
thinking_timeout = "180s"
request_timeout = "60s"
transcript_max_items = 200
transcript_max_bytes = 1048576
```

### approval

```toml
[approval]
mode = "ask"                 # ask | allow-all
```

### shortcuts

Remappable keys (`f1-f12`, `ctrl+letter`, `alt+letter`, `ctrl+arrow`; Ctrl+C is
reserved):

```toml
[shortcuts]
toggle_actions = "f9"
toggle_thinking = "f10"
toggle_loop = "ctrl+l"
cycle_shell = "ctrl+s"
grow_input = "ctrl+up"
shrink_input = "ctrl+down"
insert_newline = "ctrl+j"
```

### api

Optional current-session HTTP API (disabled by default):

```toml
[api]
listen = "127.0.0.1:7331"
auth_file = "/secure/path/.aiharn-users"
allow_origins = ["https://agent.example.com"]
only = false
max_sessions = 8
```

See [Web console and API](#web-console-and-api) below.

## Command line

```
aiharn [flags]
aiharn add-user [--file PATH] USERNAME
```

| Flag | Meaning |
|---|---|
| `--config PATH` | TOML config file (default `~/.aiharn/config.toml`) |
| `--agent NAME` | top-level agent type (default `main`) |
| `--prompt FILE` | override the system prompt file |
| `--channel NAME` | override the execution channel |
| `--model NAME` | override the model config |
| `--approval MODE` | override approval mode (`ask` \| `allow-all`) |
| `--version` | print version and exit |
| `--debug` | debug logging to stderr |
| `--log PATH` | override the session transcript path; empty disables logging |
| `--api-listen ADDR` | serve the session API (e.g. `127.0.0.1:7331`) |
| `--api-auth-file PATH` | API password file |
| `--api-allow-origin LIST` | comma-separated CORS origins |
| `--api-only` | run without the TUI (requires `--api-listen`) |

## TUI

Slash commands (single-line input starting with `/`):

- `/help` or `/` — show help.
- `/quit` or `/exit` — cancel all work and quit.
- `/clear` — start a new conversation session (same id/name; memories persist).
- `/skill` or `/skills` — list installed skills.
- `/skill install <url>` (or `/skill i <url>`) — install a skill.

Remappable keys (see `[shortcuts]`):

| Key | Action |
|---|---|
| `f9` | toggle approval ask/allow-all |
| `f10` | toggle streamed thinking summaries |
| `ctrl+l` | toggle main loop (auto-process agent inbox) |
| `ctrl+s` | cycle shell view closed/open/maximized |
| `ctrl+up` / `ctrl+down` | grow/shrink input |
| `ctrl+j` | insert newline |

Hardcoded keys: `ctrl+c`/`ctrl+d` quit; `esc` stops all work (double-`esc`
while idle clears the input); `up`/`down` prompt history; `pgup`/`pgdown`
scroll. Approval prompts use `y` (approve), `n` (deny), `a` (switch to
allow-all and approve); a `...` link opens the full command. Tool-limit prompts
use `s` (stop), `c` (continue without limit), `d` (double).

In `ask` mode, `execute_command` and `spawn_subagent` surface an approval line
before proceeding; in `allow-all` mode they proceed without prompting. The
`set_approval` tool can only tighten (`allow-all` → `ask`); `f9` toggles both
ways.

## Skills

System prompts can include `${SKILLS_INDEX}`, replaced at startup with
`<aiharn_home>/skills/index.md` (capped at 4 MiB). Skill files live at
`<aiharn_home>/skills/<skill-name>/SKILL.md`; the `skills/` directory is
optional unless a prompt references the placeholder.

`/skill` lists installed skills locally and does not send anything to an agent.
`/skill install <url>` reads `<aiharn_home>/prompts/skill-install.md`, expands
`${URL}` and `${SKILLS_DIR}`, and sends the resulting prompt to the focused
agent. The repo ships `prompts/skill-install.md`; copy it into
`<aiharn_home>/prompts/` to use `/skill install`.

## Tools

| Tool | Purpose |
|---|---|
| `execute_command` | run a shell command (fresh shell each call; approval-gated unless allow-all) |
| `write_memory` | store a memory in `local` or `global` scope |
| `list_memories` | list memory indexes and summaries |
| `get_memory` | read one memory by index |
| `list_subagent_types` | list subagent types plus caller depth/capacity constraints |
| `spawn_subagent` | spawn a subagent (approval-gated) |
| `send_subagent_message` | enqueue a follow-up to an open subagent (downward only) |
| `send_agent_message` | enqueue a message to a related agent (down or up) |
| `check_subagent` | report a subagent's status and latest output |
| `list_subagents` | list all subagents and statuses |
| `close_subagent` | close a subagent and its descendants |
| `set_approval` | tighten approval mode (`allow-all` → `ask`) |

## Subagents

A top-level agent (depth 0) can spawn typed subagents up to `max_agent_depth`
(default 2) and `max_open_agents` (default 8), subject to each agent's
`allow_subagents` flag. Subagents run a parallel session on the main agent's
current channel, so a top-level `--channel` or runtime switch applies to
subagents spawned afterward. `send_subagent_message` is downward-only;
`send_agent_message` reaches ancestors or descendants (siblings are rejected).

## Memory

`write_memory`, `list_memories`, and `get_memory` read and write scoped memory.
`local` is shared across sessions launched from the same working directory
(`<cwd>/.aiharn/memories/`); `global` is shared across all sessions under one
aiharn home (`<aiharn_home>/memories/`). Each store has an `index.md` plus a
`<summary>/memory.md` per entry. Memories survive `/clear` and session changes.

## Web console and API

The console is static files in `web/` (`index.html`, `config.js`, `app.js`,
`styles.css`). Serve them with `web/serve.sh [port]` (default 8080) or any
static file server, then point `web/config.js`'s `endpoint.url` (or the
console's Settings dialog) at the API.

Enable the API in `[api]` (or via `--api-listen`) and create a user:

```sh
./aiharn add-user --file /secure/path/.aiharn-users alice
```

### Authentication

The API uses HTTP Basic auth against a `user:bcrypt-hash` file.

`aiharn add-user [--file PATH] USERNAME` writes a `user:bcrypt-hash` line
(`bcrypt.DefaultCost`). Usernames are 1-64 of `a-z A-Z 0-9 _ - .`; passwords
must be non-empty and at most 72 bytes. The file must be a regular file with
mode `0600`, at most 1 MiB, and located outside `web/`. Set the server's
`auth_file` to the same path (or set `AIHARN_AUTH_FILE`). Precedence for the
auth file is: config `[api] auth_file` → `AIHARN_AUTH_FILE` → `--api-auth-file`.

### Endpoints

All routes are under `/api/v1`. Optional `session_id` defaults to the default
session; optional `agent_id` selects an agent view. JSON bodies only.

| Method | Path | Purpose |
|---|---|---|
| GET | `/session` | Full snapshot (session, channels, agents, messages, reasoning, pending approvals/tool-limits, capabilities) |
| POST | `/messages` | Submit a message (`content` required, ≤ 65536 bytes) |
| POST | `/approvals/{id}` | Decide an approval: `approve` \| `deny` \| `approve_all` |
| POST | `/tool-limits/{id}` | Decide a tool-call limit: `stop` \| `continue` \| `double` |
| POST | `/session/approval` | Set approval mode: `ask` \| `allow-all` |
| POST | `/session/channel` | Switch execution channel |
| POST | `/session/loop` | Enable/disable the top-level main loop (`active` bool) |
| POST | `/session/cancel` | Stop all active and queued requests |
| POST | `/session/clear` | Replace the session with a fresh one (same id) |
| GET | `/sessions` | List sessions |
| POST | `/sessions` | Create a session (`name` optional) |
| GET | `/sessions/{id}` | Describe one session |
| PATCH | `/sessions/{id}` | Rename a session |
| DELETE | `/sessions/{id}` | Close a session (default session → 409) |

### CORS

Set `[api] allow_origins` (or `--api-allow-origin`, comma-separated) to allow
cross-origin browser access. Each entry is `*` or an `http`/`https` origin.
A disallowed `Origin` is rejected with 403; allowed origins are echoed back
with `Vary: Origin`. `OPTIONS` is answered with 204 for preflight.

`--api-only` runs the API without the TUI and requires `--api-listen`.

## Architecture

Packages depend in one direction:

```
config → llm → execution → approval → tools → agent → app → tui / webapi
```

- **config** — TOML loading, env interpolation, path resolution, validation.
- **llm** — provider-neutral client contract with OpenAI Responses and Chat
  Completions adapters.
- **execution** — `Transport`/`Session` seam over `local` and `ssh` transports
  with shell marker framing.
- **approval** — serializing gate for command and subagent approvals.
- **tools** — tool registry and the subagent tool boundary.
- **agent** — agent loop, manager, depth/open-agent limits, inbox routing.
- **app** — assembles config into a runtime and owns conversation sessions.
- **tui** / **webapi** — the two frontends.

## Development

```sh
make build    # build the binary
make vet      # go vet ./...
make test     # go test ./...
make race     # go test -race ./...
make run      # build and run ./aiharn
```

- `AGENTS.md` documents the architecture and links each subsystem's `AGENTS.md`.
- `BUGS.md` tracks known issues.
- `internal/execution/ssh/harness` supplies an in-process SSH server for
  transport and session tests.
