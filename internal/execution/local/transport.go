// Package local implements the execution.Transport/Session interfaces over a
// persistent shell running on this machine, with no remote connection or
// credentials. It shares the marker framing protocol with the ssh transport.
package local

import (
	"context"
	"errors"
	"sync"

	"aiharn/internal/execution"
)

// Options are the channel settings needed to launch a local shell. They are the
// local subset of config.ChannelConfig, mirroring the ssh package's Options.
type Options struct {
	DefaultShell  string // e.g. "/bin/bash"
	RemoteCommand string // optional command run verbatim in place of DefaultShell
}

// Transport implements execution.Transport.
type Transport struct {
	opts Options

	mu     sync.Mutex
	closed bool
}

// compile-time assertion
var _ execution.Transport = (*Transport)(nil)

// NewTransport validates options and returns a Transport. It does not start a
// shell; sessions are created lazily on the first NewSession.
func NewTransport(opts Options) (*Transport, error) {
	if opts.DefaultShell == "" {
		opts.DefaultShell = "/bin/bash"
	}
	return &Transport{opts: opts}, nil
}

// NewSession starts a persistent local shell.
func (t *Transport) NewSession(ctx context.Context) (execution.Session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("local: transport closed")
	}
	return openSession(t.opts)
}

// Close marks the transport closed; already-open sessions own their processes.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}
