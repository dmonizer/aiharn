package execution

import (
	"context"
	"errors"
)

// Session is a persistent stateful shell bound to one agent. Shell state
// (working directory, environment, functions) persists across Exec calls.
type Session interface {
	// Exec runs a command in the session's shell. It returns the command's
	// stdout/stderr/exit code. If the underlying shell dies (or the transport
	// connection is lost), it returns an error wrapping ErrSessionReset; the
	// caller must re-establish the session to continue.
	Exec(ctx context.Context, cmd string, opts ExecOptions) (Result, error)

	// Close terminates the session and releases its connection.
	Close() error
}

// ErrSessionReset reports that the shell died unexpectedly and shell state was
// lost. It is returned (wrapped) by Exec after a session is no longer usable.
var ErrSessionReset = errors.New("execution: session reset, shell state lost")
