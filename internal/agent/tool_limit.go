package agent

import (
	"context"
	"errors"
	"fmt"
)

// ToolLimitRequest describes a turn paused before its next tool call.
type ToolLimitRequest struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
	Count   int    `json:"count"`
	Limit   int    `json:"limit"`
}

type pendingToolLimit struct {
	request  ToolLimitRequest
	decision chan string
}

var ErrToolLimitNotPending = errors.New("agent: tool-call limit prompt not pending")

// PendingToolLimit returns the unresolved prompt, if any.
func (a *Agent) PendingToolLimit() *ToolLimitRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingToolLimit == nil {
		return nil
	}
	copy := a.pendingToolLimit.request
	return &copy
}

// DecideToolLimit resolves the current prompt. "continue" removes the cap for
// this turn; "double" doubles it; "stop" finishes without running more tools.
func (a *Agent) DecideToolLimit(id, decision string) error {
	if decision != "stop" && decision != "continue" && decision != "double" {
		return errors.New("agent: decision must be stop, continue, or double")
	}
	a.mu.Lock()
	pending := a.pendingToolLimit
	if pending == nil || pending.request.ID != id {
		a.mu.Unlock()
		return ErrToolLimitNotPending
	}
	a.pendingToolLimit = nil
	pending.decision <- decision
	a.mu.Unlock()
	return nil
}

func (a *Agent) awaitToolLimit(ctx context.Context, count, limit int) (string, error) {
	a.mu.Lock()
	a.toolLimitSeq++
	pending := &pendingToolLimit{
		request:  ToolLimitRequest{ID: fmt.Sprintf("%s-%d", a.id, a.toolLimitSeq), AgentID: a.id, Count: count, Limit: limit},
		decision: make(chan string, 1),
	}
	a.pendingToolLimit = pending
	a.mu.Unlock()
	a.emit(Event{Type: EventToolLimit, ToolLimit: &pending.request})
	select {
	case decision := <-pending.decision:
		return decision, nil
	case <-ctx.Done():
		a.mu.Lock()
		if a.pendingToolLimit == pending {
			a.pendingToolLimit = nil
		}
		a.mu.Unlock()
		return "", ctx.Err()
	}
}
