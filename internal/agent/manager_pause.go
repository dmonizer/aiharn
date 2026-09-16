package agent

import "context"

// SetSubagentPaused holds or releases queued tasks for a subagent owned by the
// caller. A running task completes before the pause takes effect; this avoids
// replaying partially executed commands when the user resumes the agent.
func (m *Manager) SetSubagentPaused(ctx context.Context, callerID, subagentID string, paused bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	caller := m.agents[callerID]
	if caller == nil {
		m.mu.Unlock()
		return ErrCallerNotFound
	}
	if !isOpen(caller) {
		m.mu.Unlock()
		return ErrCallerUnavailable
	}
	sub := m.agents[subagentID]
	if sub == nil {
		m.mu.Unlock()
		return ErrSubagentNotFound
	}
	if !m.isDescendantLocked(callerID, subagentID) {
		m.mu.Unlock()
		return ErrSubagentNotOwned
	}
	if !isOpen(sub) {
		m.mu.Unlock()
		return ErrSubagentUnavailable
	}
	sub.setPaused(paused)
	m.mu.Unlock()
	state := "resumed"
	if paused {
		state = "paused"
	}
	sub.emit(Event{Type: EventPause, Text: state})
	m.notify()
	return nil
}
