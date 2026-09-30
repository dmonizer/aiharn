package execution

import (
	"context"
	"errors"
)

// Session is a long-lived shell bound to one agent. The shell process and its
// stdin/stdout pipes persist across Exec calls, but each command runs in a
// fresh subshell, so working directory, exported variables, functions, and
// shell options do not carry over between calls. Use ExecOptions.Cwd (or chain
// commands with &&) for any state that must span a single command.
type Session interface {
	// Exec runs a command in the session's shell. It returns the command's
	// stdout/stderr/exit code. If the underlying shell dies (or the transport
	// connection is lost), it returns an error wrapping ErrSessionReset; the
	// caller must re-establish the session to continue.
	Exec(ctx context.Context, cmd string, opts ExecOptions) (Result, error)

	// Close terminates the session and releases its connection.
	Close() error
}

// ErrSessionReset reports that the shell became unusable: the interpreter
// process (and its inherited baseline environment) is gone, or the connection
// was lost. It is returned (wrapped) by Exec after a session is no longer
// usable.
var ErrSessionReset = errors.New("execution: session reset, shell no longer usable")
