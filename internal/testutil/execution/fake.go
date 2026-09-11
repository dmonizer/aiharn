// Package execution provides reusable test doubles for the execution.Transport
// and execution.Session contracts.
package execution

import (
	"context"
	"sync"

	"aiharn/internal/execution"
)

// Handler runs a single Exec and returns its result. It may be nil, in which
// case Exec returns an empty Result and no error.
type Handler func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error)

// Call is one recorded Exec invocation.
type Call struct {
	Cmd  string
	Opts execution.ExecOptions
}

// Session is a scripted execution.Session.
type Session struct {
	mu       sync.Mutex
	handler  Handler
	calls    []Call
	closed   bool
	closeErr error
}

var _ execution.Session = (*Session)(nil)

// NewSession returns a Session whose Exec delegates to h.
func NewSession(h Handler) *Session { return &Session{handler: h} }

// Exec implements execution.Session.
func (s *Session) Exec(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, Call{Cmd: cmd, Opts: opts})
	h := s.handler
	s.mu.Unlock()
	if h == nil {
		return execution.Result{}, nil
	}
	return h(ctx, cmd, opts)
}

// Close implements execution.Session.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.closeErr
}

// Calls returns a copy of the recorded Exec invocations.
func (s *Session) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Closed reports whether Close has been called.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// SetCloseErr sets the error Close returns.
func (s *Session) SetCloseErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeErr = err
}

// Transport is a scripted execution.Transport that hands out Sessions from a
// queue in order. When the queue is empty it yields a fresh no-op Session.
type Transport struct {
	mu       sync.Mutex
	queue    []execution.Session
	newErr   error
	closed   bool
	closeErr error
	opened   int
}

var _ execution.Transport = (*Transport)(nil)

// NewTransport returns a Transport yielding the given sessions in order.
func NewTransport(sessions ...execution.Session) *Transport {
	return &Transport{queue: sessions}
}

// NewSession implements execution.Transport.
func (t *Transport) NewSession(ctx context.Context) (execution.Session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.newErr != nil {
		return nil, t.newErr
	}
	t.opened++
	if len(t.queue) == 0 {
		return NewSession(nil), nil
	}
	s := t.queue[0]
	t.queue = t.queue[1:]
	return s, nil
}

// Close implements execution.Transport.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return t.closeErr
}

// SetNewErr makes NewSession return err.
func (t *Transport) SetNewErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newErr = err
}

// SetCloseErr sets the error Close returns.
func (t *Transport) SetCloseErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeErr = err
}

// Closed reports whether Close has been called.
func (t *Transport) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Opened reports how many sessions have been handed out.
func (t *Transport) Opened() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opened
}
