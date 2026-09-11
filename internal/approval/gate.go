// Package approval mediates command approval between the agent loop and the
// supervising user. It enforces the one-way rule: the model may only tighten
// approval (allow-all → ask), never loosen; loosening is a user-only action.
package approval

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// Mode is the current approval policy.
type Mode int

const (
	// ModeAsk requires an explicit user decision before every gated action.
	ModeAsk Mode = iota
	// ModeAllowAll approves every gated action automatically.
	ModeAllowAll
)

func (m Mode) String() string {
	switch m {
	case ModeAsk:
		return "ask"
	case ModeAllowAll:
		return "allow-all"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// Decision is the outcome of a gated action.
type Decision int

const (
	DecisionApproved Decision = iota
	DecisionDenied
)

// Request describes a gated action awaiting approval.
type Request struct {
	ID       string
	ToolName string
	Command  string
	Args     string
}

var (
	// ErrLoosen is returned when the model attempts to loosen approval.
	ErrLoosen = errors.New("approval: model may only tighten, not loosen")
	// ErrClosed is returned when a Check is attempted after Close.
	ErrClosed = errors.New("approval: gate closed")
)

// Gate holds the current mode and coordinates pending approvals.
type Gate struct {
	mu      sync.Mutex
	mode    Mode
	closed  bool
	seq     int
	pending chan Request
	byID    map[string]chan Decision
}

// NewGate returns a Gate in the given mode.
func NewGate(mode Mode) *Gate {
	return &Gate{
		mode:    mode,
		pending: make(chan Request, 16),
		byID:    make(map[string]chan Decision),
	}
}

// Mode returns the current mode.
func (g *Gate) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// Check returns the decision for req. In AllowAll it approves immediately. In
// Ask it registers req, surfaces it via Pending, and blocks until the user
// calls Decide or ctx is done.
func (g *Gate) Check(ctx context.Context, req Request) (Decision, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return DecisionDenied, ErrClosed
	}
	if g.mode == ModeAllowAll {
		g.mu.Unlock()
		return DecisionApproved, nil
	}
	g.seq++
	req.ID = strconv.Itoa(g.seq)
	ch := make(chan Decision, 1)
	g.byID[req.ID] = ch
	g.mu.Unlock()

	select {
	case g.pending <- req:
	case <-ctx.Done():
		g.unregister(req.ID)
		return DecisionDenied, ctx.Err()
	}

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		g.unregister(req.ID)
		return DecisionDenied, ctx.Err()
	}
}

// Pending delivers requests that are awaiting a decision, for the UI.
func (g *Gate) Pending() <-chan Request { return g.pending }

// Decide resolves a pending request by ID. It returns an error if no such
// request is pending.
func (g *Gate) Decide(id string, d Decision) error {
	g.mu.Lock()
	ch, ok := g.byID[id]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval: no pending request %q", id)
	}
	ch <- d
	return nil
}

// SetMode sets the mode. This is the user-facing path; it may loosen or
// tighten.
func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
}

// ApplyModelMode applies a mode requested by the model. It may only tighten
// (allow-all → ask); a loosening request returns ErrLoosen.
func (g *Gate) ApplyModelMode(m Mode) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrClosed
	}
	if g.mode == ModeAsk && m == ModeAllowAll {
		return ErrLoosen
	}
	g.mode = m
	return nil
}

// Close denies all pending requests and prevents further checks.
func (g *Gate) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	for id, ch := range g.byID {
		select {
		case ch <- DecisionDenied:
		default:
		}
		delete(g.byID, id)
	}
	return nil
}

func (g *Gate) unregister(id string) {
	g.mu.Lock()
	delete(g.byID, id)
	g.mu.Unlock()
}
