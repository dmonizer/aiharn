# Execution subsystem

The core seam is `Transport` in `transport.go` and `Session` in `session.go`.
A session is a long-lived shell process that runs one command at a time and
returns stdout, stderr, and an exit code. Each command runs in a fresh subshell,
so working directory, exported variables, functions, and shell options do not
persist across calls. Use `ExecOptions.Cwd` or one chained command when state is
needed within an execution.

`ErrSessionReset` marks an unusable session that callers must discard.
Implementations should carry a compile-time interface assertion such as:

```go
var _ execution.Transport = (*Transport)(nil)
```

Implementations:

- `ssh` uses `golang.org/x/crypto/ssh`, knownhosts verification, password,
  key-file, or SSH-agent authentication, and OpenSSH config alias resolution
  when a channel supplies only name and host.
- `local` uses `os/exec`, `Setsid`, and process-group termination while matching
  SSH semantics. Local channels require no host, user, or authentication.
- `shell` owns the marker framing shared by both transports. Commands are
  base64-encoded and launched with `setsid bash -c`; unique
  `AIHARN-BEGIN/END/ERR` markers plus a 128-bit nonce separate stdout, stderr,
  and exit status without shell quoting.

Framing is sensitive to marker uniqueness, truncation, process-group signals,
and cancellation without corrupting the reusable session. Before changing it,
read `shell/framing.go` and both transports' `Exec` implementations together and
keep their behavior aligned.

The `ssh/harness` package is an in-process SSH server for transport tests.
