// Package execution defines the transport/session abstraction for running
// commands on a remote host. It has no knowledge of the agent, LLM, or config
// packages; transport implementations (e.g. execution/ssh) live beneath it.
package execution

import "io"

// Result is the outcome of a single command execution.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool // output exceeded the per-command cap
}

// ExecOptions controls a single Exec call.
type ExecOptions struct {
	Cwd string // working directory for this command; empty = the shell's cwd

	// Stream, if non-nil, receives stdout as it is produced. The MVP TUI does
	// not use it; it exists so callers can render live output.
	Stream io.Writer

	// MaxOutputBytes caps the bytes retained in Result.Stdout+Stderr. Zero
	// means no cap. Truncation happens while streaming; the session never
	// buffers unbounded output. Bytes beyond the cap are still consumed from
	// the shell (to keep framing aligned) but are not retained.
	MaxOutputBytes int64
}
