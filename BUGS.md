# Known bugs

This file records confirmed defects and contract mismatches in Aiharn. Each
entry carries a stable id, a `Status` field, and enough evidence (exact commands
and output) to re-verify it. Entries are append-only: do not renumber or reuse an
id, and do not rewrite an existing entry. When new information arrives, append to
the entry's `Notes` and update `Status` (`open`, `needs triage`, `fixed`,
`wontfix`, `not reproducible`).

## BUG-001 Session is documented as "stateful", but no shell state survives an Exec call

- Status: open
- Severity: Medium (documentation/contract defect; no code depends on the missing
  behaviour, but an agent that assumes `cd`/`export` persists can silently run a
  command in an unexpected directory)
- Area: internal/execution/shell (framing), internal/execution/local,
  internal/execution/ssh; docs: internal/execution/session.go, AGENTS.md
- First recorded: 2026-09-16

### Summary

`execution.Session` is documented as a "persistent, stateful shell" whose "shell
state (working directory, environment, functions) persists across Exec calls"
(`internal/execution/session.go:8-9`, echoed at `transport.go:8`,
`local/transport.go:2-3`, `local/session.go:24`, `ssh/transport.go:2-3`,
`ssh/session.go:25`, `AGENTS.md:47-48`, `AGENTS.md:106`). That is inaccurate.

Each `Exec` writes a wrapper script to the session's shell stdin; the wrapper runs
the command in a **freshly spawned child** (`setsid bash -c '...eval "$2"'`,
`internal/execution/shell/framing.go:71`) and the parent shell only waits for it
(`framing.go:74`). What persists across `Exec` calls is therefore the *interpreter
process*, its inherited environment and cwd baseline, and the stdin/stdout pipes
(for SSH, also the connection and the channel). Nothing else does: cwd, exported
variables, functions/aliases, and shell options (`set -e`, `set -u`) set by one
command are gone by the next.

Verified empirically over the local transport (no SSH server needed). The SSH
transport was verified by code inspection only (no live SSH connection was used): it
writes the same `shell.BuildWrapperScript` to the same kind of stdin-reading shell
(`ssh/session.go:192`, `ssh/session.go:390-395`), so the result is identical.

### Expected

Per the doc comments above: state set by one `Exec` is visible to the next.

```text
Exec("cd /tmp")                                  -> exit 0
Exec("pwd")                                      -> /tmp
Exec("export FOO=bar")                           -> exit 0
Exec("echo $FOO")                                -> bar
Exec("f() { echo hi; }") ; Exec("f")             -> hi
```

### Actual

State does not carry over; only the shell process and its streams do.

```text
Exec("cd /tmp")                                  -> exit 0
Exec("printf 'cwd-after-cd: %s\n' \"$PWD\"")     -> cwd-after-cd: /home/ubuntu   (not /tmp)
Exec("export FOO=bar")                           -> exit 0
Exec("printf 'FOO=[%s]\n' \"$FOO\"")             -> FOO=[]
Exec("f() { echo hi-from-f; }")                  -> exit 0
Exec("f")                                        -> exit 127, stderr: _: line 1: f: command not found
Exec("set -e") ; Exec("false; echo reached-after-false")
                                                 -> exit 0, stdout: reached-after-false   (set -e lost)
Exec("set -u") ; Exec("printf 'UNSET=[%s]\n' \"$UNSET_VAR_XYZ\"")
                                                 -> exit 0, stdout: UNSET=[]             (set -u lost)
```

### Reproduction

Go's internal-package visibility rule blocks an out-of-tree module from importing
`aiharn/internal/...` (verified: `main.go:6:2: use of internal package
aiharn/internal/execution/local not allowed`). The harness is therefore a copy of
the module with a probe `main` added inside the copy. Nothing in the repository is
modified:

```sh
# 1. copy the module source (read-only copy; no repo file touched)
rm -rf /tmp/aiharn-probe && mkdir -p /tmp/aiharn-probe
cd /home/ubuntu/Development/aiharn
cp -a go.mod go.sum internal cmd /tmp/aiharn-probe/

# 2. add /tmp/aiharn-probe/cmd/probe/main.go, which opens a local session
#    (local.NewTransport(local.Options{DefaultShell: "/bin/bash"})), calls
#    Exec(cmd, ExecOptions{MaxOutputBytes: 1<<20}) N times in sequence on that one
#    session, and prints exit_code/stdout/stderr for each call (Go %q, so "\n"
#    below is the literal escape printed by the probe).

# 3. build and run from a directory that is not /tmp, so the base cwd is obvious
cd /tmp/aiharn-probe && go build -o /tmp/aiharn-probe-bin ./cmd/probe
cd /home/ubuntu && /tmp/aiharn-probe-bin
```
Observed output (verbatim excerpts; the probe prints Go `%q`, so `\n` below is a
literal escape):

```text
--- A. working directory ---
Exec("printf 'cwd-before: %s\n' \"$PWD\"")     stdout: "cwd-before: /home/ubuntu\n"
Exec("cd /tmp")                                exit_code: 0
Exec("printf 'cwd-after-cd: %s\n' \"$PWD\"")   stdout: "cwd-after-cd: /home/ubuntu\n"   # not /tmp
Exec("pwd", Cwd: "/etc")                       stdout: "/etc\n"                        # per-command cwd works
Exec("pwd")                                    stdout: "/home/ubuntu\n"

--- B. exported environment ---
Exec("export FOO=bar")                         exit_code: 0
Exec("printf 'FOO=[%s]\n' \"$FOO\"")           stdout: "FOO=[]\n"
Exec("printf 'PATH_len=%d\n' \"${#PATH}\"")    stdout: "PATH_len=191\n"                # inherited baseline present

--- C. shell functions and aliases ---
Exec("f() { echo hi-from-f; }")                exit_code: 0
Exec("f")                                      exit_code: 127
                                               stderr: "_: line 1: f: command not found"

--- D. shell options (set -e / set -u) ---
Exec("set -e"); Exec("false; echo reached-after-false")
                                               exit_code: 0, stdout: "reached-after-false\n"
Exec("set -u"); Exec("printf 'UNSET=[%s]\n' \"$UNSET_VAR_XYZ\"")
                                               stdout: "UNSET=[]\n"
```

Relative paths fail for the same reason (`/tmp/bug001/rel.txt` is created out of
band by the probe):

```text
Exec("cd /tmp/bug001"); Exec("cat rel.txt")    exit_code: 1
                                               stderr: "cat: rel.txt: No such file or directory"
Exec("cat rel.txt", Cwd: "/tmp/bug001")        exit_code: 0, stdout: "relative-file-contents\n"
Exec("cd /tmp/bug001 && cat rel.txt")          exit_code: 0, stdout: "relative-file-contents\n"
```

What *is* persistent: the shell process and the pipes. The parent PID is constant
across calls while each command's own `$$` differs, and `ps` shows one long-lived
`bash -s` child of the aiharn process plus one short-lived `setsid`'d command child
per call (which is what keeps `kill -TERM -- -<pid>` safe):

```text
Exec("echo \"child_pid=$$ ppid=$PPID\"") x3    ppid=484950 every time
                                               $$ = 484958 / 484968 / 484978
Exec("ps -o pid=,ppid=,pgid=,sid=,args= --ppid 484445")
                                               "484450 484445 484450 484450 /bin/bash --noprofile --norc -s"
```

Side effects outside the shell *do* persist: a background process started in one
`Exec` is still visible to a later `Exec` (and `pkill` on it can end the session).

```text
Exec("nohup sleep 45 & echo bg_started")       stdout: "bg_started\n"
Exec("pgrep -a -f 'sleep 45'")                 stdout: "484652 sleep 45\n"
```

### Root cause

- Each command runs in a new child interpreter: `internal/execution/shell/framing.go:71`
  writes `setsid bash -c 'if [ -n "$1" ]; then cd -- "$1" ...; fi; eval "$2"' _ "$__a_cd" "$__a_cmd"`,
  and `framing.go:74` (`wait $__a_pid`) is all the parent shell does with the
  command. The child exits after `eval`, taking its cwd, environment, functions,
  and options with it. The parent shell only executes the wrapper's own
  bookkeeping (`mktemp`, `base64 -d`, `printf` markers, `wait`, `cat`, `rm`) and
  re-uses the same `__a_*` variables on every call; nothing from a command is
  `eval`'d in the parent, so nothing from a command can leak into it.
- Why the child exists (intentional, not an accident): `framing.go:59-62`
  documents it — a `setsid`'d subshell owns its process group, so cancellation can
  `kill -TERM -- -pgid` only the command. `docs/implementation_plan_issues.md:42-70`
  (IP-002) anticipated the tradeoff in the same words: "wrapping each command in a
  child process makes PID cancellation easier but prevents `cd`, exported
  variables, functions, and other shell state from persisting".
- The persistent parts are the process and the pipes: the local transport starts
  one `bash --noprofile --norc -s` with `Setsid: true` and stdin/stdout pipes
  (`internal/execution/local/session.go:54-86`) and writes every wrapper script to
  that same `s.stdin` (`local/session.go:139-145`); SSH starts one non-PTY channel
  running `shell.ShellCommand(opts.DefaultShell)` (`ssh/session.go:44-109`) and
  writes to `s.stdin` (`ssh/session.go:192-198`). Hence the constant shell PID and
  constant `$PPID` above.
- `ExecOptions.Cwd` (`internal/execution/result.go:18`) is applied as a `cd`
  **inside the child** (`framing.go:71`), i.e. per command. An empty `Cwd` means the
  persistent shell's cwd, which is the cwd Aiharn was started in and can never be
  changed by a command.
- `ErrSessionReset`'s wording is wrong in both directions: `session.go:21-23`
  ("session reset, shell state lost") describes losing state that never existed
  across commands; the real loss is the interpreter process and its inherited
  environment/cwd baseline.
- Planning documents that assert the persistence contract and are contradicted by
  the implementation: `docs/plan.md:243` ("a persistent shell bound to one agent
  (isolated, stateful)"), `docs/plan.md:416-421` (Phase 3A requires proving
  persistent `cd`/env/function state, or narrowing and documenting the contract),
  `docs/plan.md:431` (requires "state persistence" unit tests),
  `docs/plan_issues.md:174-176` (UP-006: "The shell's subsequent `cd` commands
  should persist for that agent"), `docs/plan.md:164-166` (keep-alive rationale
  phrased in terms of shell state).
- `stopped.md:25-26` already states the real behaviour and contradicts the rest:
  "Because the SSH wrapper runs each command in a subshell, working_dir is applied
  as a per-command default cwd, not a one-time persistent `cd`."
- No code or test relies on cross-`Exec` state, which is why the mismatch is
  invisible:
  - The only production `Exec` call sites are `internal/tools/exec_command.go:116`
    (one command per tool call) and `internal/app/app.go:293,322` (a self-contained
    `printf "$HOME"` and `mkdir -p`). `default_cwd` reaches the shell only as
    `ExecOptions.Cwd` (`tools/exec_command.go:106-119`, `app.go:282-331`), so it is
    applied as a per-command `cd` and does not depend on persistence at all.
  - `internal/execution/local/session_test.go` passes `Cwd` explicitly per call
    (`:66-77`) and only asserts that the session survives a timeout (`:106-119`);
    `internal/execution/shell/framing_test.go` covers markers, truncation, and
    sink errors only. No test asserts that cwd/env/functions persist (or that they
    do not), so neither behaviour is pinned.
  - The TUI "shell pane" (`internal/tui/shell.go:58-99`) renders the agent's
    `execute_command` calls; it does not let a human type into the session, so
    there is no interactive path that could expose this to a user directly.

### Impact

Affected:

- Sequential `cd`: a model that runs `cd /some/dir` in one tool call and then a
  relative command in the next gets the base cwd instead. Visible as
  `No such file or directory` (exit 1) for reads, and as wrong-target mutation for
  writes (`rm -rf ./*`, `> file`, `git ...`) when the intended directory was
  non-trivial. The base cwd is whatever directory Aiharn was launched from — the
  repository or `$HOME` in normal use — so the failure mode is not merely a
  no-op.
- Environment: `export FOO=bar` (or a credential/token exported for a later
  command) is invisible to the next `Exec`. Beyond the environment
  inherited at shell start, the `Env` set then (`ssh/transport.go:43-46`,
  `ssh/session.go:75-83`) is the only environment change that persists, and it is
  fixed for the life of the session.
- Functions/aliases: definitions are lost (`f: command not found`, exit 127);
  aliases from dotfiles never apply at all, because the command child is a
  non-interactive `bash -c` (and the session shell is started
  `--noprofile --norc`, `shell/command.go:13-18`).
- Shell options: `set -e`, `set -u`, `set -o pipefail` do not affect later
  commands, so "fail fast" setups silently do not hold.
- Contract reasoning: a session reset reports "shell state lost"
  (`session.go:23`); the phrase misleads an agent (and future implementers) about
  what a session actually guarantees.

Not affected:

- Everything outside the shell process: files written, processes started
  (demonstrated above), package installs, services, remote state. Those persist
  normally; only intra-shell state is transient.
- `default_cwd` / `working_dir`: applied per command via `ExecOptions.Cwd`, so it
  is unaffected by the missing persistence.
- Chaining inside one command: `cd /tmp && pwd` returns `/tmp`, and
  `cd /tmp && ./x` works, because that is a single child shell.
- SSH `keep_alive`, shared clients, and per-agent isolation: unaffected; the
  reused object is the connection/channel and each agent still owns its own shell
  process.
- One-command-at-a-time serialization and cancellation semantics: unaffected.

### Notes / suggested direction

Assessment: the mechanism is intentional (it is the cancellation design), so this
is a documentation/contract bug rather than a regression. Recommended fix:
**correct the docs to describe actual behaviour** rather than make state persist.

Doc fix (small, low risk):

- Rewrite `internal/execution/session.go:8-9` and `transport.go:8` to say what is
  actually persistent: one long-lived interpreter process per agent, with each
  `Exec` running its command in a fresh `setsid`'d subshell, so cwd/exported
  env/functions/options do not carry across calls; state must be re-established
  per command (`cd X && cmd`, or `ExecOptions.Cwd`). Fix the same claim in
  `local/transport.go:2-3`, `local/session.go:24`, `ssh/transport.go:2-3`,
  `ssh/session.go:25`, `AGENTS.md:47-48` (committed) and `AGENTS.md:106` (currently
  an uncommitted working-tree addition), plus `docs/plan.md:243,431` and
  `docs/plan_issues.md:176`, and reword `ErrSessionReset`
  (`internal/execution/session.go:21-23`) so it does not promise state that never
  existed.
- Add a short line to the `execute_command` tool description
  (`internal/tools/exec_command.go:43-57`) stating that every command starts in a
  fresh shell and that state does not persist; today the model can only discover
  this from a failed command.
- Add the missing regression test (local transport, no SSH needed): assert that
  cwd, exported env, a function, and `set -e` do **not** survive an `Exec`, pinning
  the documented contract. `docs/plan.md:431` asked for state-persistence tests;
  none exists in either direction.

Implementation fix (make the claim true) — tradeoffs; do not start without an
explicit decision:

- Serialize child state back into the parent (child dumps cwd/`export -p`/
  `declare -f` to a file, next wrapper `source`s it). Quoting and replay of
  arbitrary `declare -p` values is fragile, `cd` into a deleted directory, traps,
  `ulimit`, job control, and option semantics (`set -e` mid-script, `set -u` with
  unset vars) cannot be represented faithfully; the result would be a state model
  that is subtly wrong instead of predictably empty, and silent partial success is
  worse for an agent than a documented reset.
- Or run commands directly in the persistent shell instead of a child, which
  makes cwd/env/functions genuinely persist but forfeits the clean per-process-
  group termination that `framing.go:59-62` relies on; cancellation and timeout
  would then risk killing the shell itself (exactly what
  `TestTimeoutLeavesShellUsable`, `local/session_test.go:106`, protects).
- Both options add failure modes to a protocol `AGENTS.md:60-64` already flags as
  subtle. The current contract (fresh shell per command, explicit cwd per command)
  is the more robust one; document it and keep it.

Related observation (not part of BUG-001; unaddressed here): `framing.go:71`
hardcodes `bash` for the command child, while the session interpreter comes from
`default_shell`/`remote_command` (`local/session.go:56-60`,
`ssh/session.go:390-395`). A channel configured with `default_shell = "/bin/sh"`
still runs each command under `bash -c`, and every command then depends on `bash`
and `setsid` being on `PATH` on the target.
