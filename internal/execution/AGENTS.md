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

## Filesystem capability

`filesystem.go` adds a structured filesystem capability next to the shell seam:
`Filesystem` (bounded `ReadFile`, exclusive `CreateFile`, versioned atomic
`ReplaceFile`, `Target`, `Close`), `FilesystemProvider` (implemented by the local
and SSH transports), `FileVersion` (device, inode, size, mtime, ctime, and a
SHA-256 digest of the whole content), and the stable `FSError` codes that reach
the model. It never runs commands and never interpolates names into a shell.

- `fscore` is the Linux implementation: it opens the root once and resolves one
  `openat` step per component with `O_NOFOLLOW`, re-validates the leaf it
  actually opened, reads bounded (`maxBytes+1`), stages mutations in the
  destination directory, and publishes with `linkat` (create, no replace) or
  `renameat` (replace) after preserving mode and ownership and re-checking the
  expected version. Multiply linked files are rejected. Other platforms report
  `unsupported_filesystem`; do not add a weaker fallback.
- `local` adapts `fscore` through `Transport.NewFilesystem`.
- `fsproto` is the framed JSON protocol (4-byte length prefix, 16 MiB frame cap,
  protocol version, `init`/`read`/`create`/`replace`/`ping`/`close`), and
  `fshelper` serves it for the `aiharn fs-helper` subcommand.
- `ssh.Transport.NewFilesystem` dials a dedicated client, starts the helper
  (`Options.FSHelper`, default `aiharn fs-helper`), performs the init handshake,
  and serializes one outstanding request per connection. A missing or
  mismatched helper is `unsupported_filesystem`; a connection lost during a
  mutation is `outcome_unknown` (never "failed", never "rolled back").
- `fstest` holds the contract suite that both backends must pass unchanged.

Concurrency: callers must assume no compare-and-swap against arbitrary external
writers. Serialize Aiharn mutations per `Target()` (the app's registry does) and
document the residual race between the final version check and `renameat`.
