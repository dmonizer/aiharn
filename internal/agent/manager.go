package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"aiharn/internal/tools"
)

// SpawnSpec is what the Manager needs to construct a subagent: a unique id, the
// configured agent type, its depth, and its caller.
type SpawnSpec struct {
	ID       string
	Type     string
	Depth    int
	CallerID string
}

// Builder constructs a fully-wired Agent for a SpawnSpec. The app layer supplies
// it, closing over the config, model/transport factories, and the Manager itself
// so subagent tools can reach the runtime.
type Builder func(ctx context.Context, spec SpawnSpec) (*Agent, error)

// ManagerOptions configures a Manager. Builder is required.
type ManagerOptions struct {
	MaxDepth      int
	MaxAgents     int
	InboxCapacity int
	EventCapacity int
	Builder       Builder
}

// Sentinel errors returned by Manager methods, surfaced to the model via tools.
var (
	ErrClosed             = errors.New("agent: manager closed")
	ErrCallerNotFound     = errors.New("agent: caller not found")
	ErrSubagentsNotAllowed = errors.New("agent: caller may not spawn subagents")
	ErrMaxDepth           = errors.New("agent: maximum agent depth reached")
	ErrMaxAgents          = errors.New("agent: maximum open agents reached")
	ErrSubagentNotFound   = errors.New("agent: subagent not found")
	ErrCannotMessageSelf  = errors.New("agent: cannot message self")
	ErrSubagentUnavailable = errors.New("agent: subagent is not open")
	ErrInboxFull          = errors.New("agent: subagent inbox is full")
)

// Manager owns every agent (the top-level agent and all subagents), assigns
// unique ids, enforces the spawn limits (allow_subagents, depth, open count),
// routes messages, and provides idempotent shutdown.
type Manager struct {
	mu         sync.Mutex
	agents     map[string]*Agent
	seq        int
	building   int // in-flight spawns, counted against the open limit
	maxDepth   int
	maxAgents  int
	inboxCap   int
	eventCap   int
	builder    Builder
	closed     bool

	wg           sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

// NewManager returns a Manager. opts.Builder is required.
func NewManager(opts ManagerOptions) *Manager {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 2
	}
	if opts.MaxAgents <= 0 {
		opts.MaxAgents = 8
	}
	if opts.InboxCapacity <= 0 {
		opts.InboxCapacity = 32
	}
	if opts.EventCapacity <= 0 {
		opts.EventCapacity = 256
	}
	return &Manager{
		agents:       map[string]*Agent{},
		maxDepth:     opts.MaxDepth,
		maxAgents:    opts.MaxAgents,
		inboxCap:     opts.InboxCapacity,
		eventCap:     opts.EventCapacity,
		builder:      opts.Builder,
		shutdownDone: make(chan struct{}),
	}
}

// RegisterTop registers the top-level agent (depth 0). It installs the agent's
// lifecycle context but starts no run loop; the TUI drives the top-level agent
// directly.
func (m *Manager) RegisterTop(a *Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, ok := m.agents[a.ID()]; ok {
		return fmt.Errorf("agent: duplicate id %q", a.ID())
	}
	a.setContext(context.WithCancel(context.Background()))
	m.agents[a.ID()] = a
	return nil
}

// Agent returns the agent with the given id, or nil.
func (m *Manager) Agent(id string) *Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agents[id]
}

// SpawnSubagent implements tools.SubagentBackend. It validates the caller's
// right to spawn, reserves a slot against the open-agent limit, builds the
// subagent off the lock, then registers it and starts its run loop.
func (m *Manager) SpawnSubagent(ctx context.Context, callerID, agentType, prompt string) (string, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", ErrClosed
	}
	caller := m.agents[callerID]
	if caller == nil {
		m.mu.Unlock()
		return "", ErrCallerNotFound
	}
	if !caller.AllowSubagents() {
		m.mu.Unlock()
		return "", ErrSubagentsNotAllowed
	}
	depth := caller.Depth() + 1
	if depth > m.maxDepth {
		m.mu.Unlock()
		return "", ErrMaxDepth
	}
	if m.openCountLocked()+m.building >= m.maxAgents {
		m.mu.Unlock()
		return "", ErrMaxAgents
	}
	m.seq++
	id := fmt.Sprintf("%s-%d", agentType, m.seq)
	m.building++
	builder := m.builder
	m.mu.Unlock()

	sub, err := builder(ctx, SpawnSpec{ID: id, Type: agentType, Depth: depth, CallerID: callerID})
	if err != nil {
		m.mu.Lock()
		m.building--
		m.mu.Unlock()
		return "", fmt.Errorf("agent: spawn %q: %w", agentType, err)
	}

	m.mu.Lock()
	m.building--
	if m.closed {
		m.mu.Unlock()
		sub.Close()
		return "", ErrClosed
	}
	subCtx, subCancel := context.WithCancel(context.Background())
	sub.setContext(subCtx, subCancel)
	sub.setOnComplete(func(result string) {
		caller.Send(fmt.Sprintf("[subagent %s (%s)] %s", agentType, id, result))
	})
	m.agents[id] = sub
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		sub.run(subCtx)
	}()
	m.mu.Unlock()

	sub.Send(prompt)
	return id, nil
}

// SendSubagentMessage implements tools.SubagentBackend.
func (m *Manager) SendSubagentMessage(ctx context.Context, callerID, subagentID, message string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if callerID == subagentID {
		m.mu.Unlock()
		return ErrCannotMessageSelf
	}
	if m.agents[callerID] == nil {
		m.mu.Unlock()
		return ErrCallerNotFound
	}
	sub := m.agents[subagentID]
	if sub == nil {
		m.mu.Unlock()
		return ErrSubagentNotFound
	}
	m.mu.Unlock()

	switch sub.State() {
	case StateClosed, StateErrored:
		return ErrSubagentUnavailable
	}
	if !sub.Send(message) {
		return ErrInboxFull
	}
	return nil
}

// CheckSubagent implements tools.SubagentBackend.
func (m *Manager) CheckSubagent(ctx context.Context, callerID, subagentID string) (tools.SubagentStatus, error) {
	m.mu.Lock()
	sub := m.agents[subagentID]
	m.mu.Unlock()
	if sub == nil {
		return tools.SubagentStatus{}, ErrSubagentNotFound
	}
	return statusOf(sub), nil
}

// ListSubagents implements tools.SubagentBackend. It lists all subagents
// (depth > 0) in id order, regardless of state.
func (m *Manager) ListSubagents(ctx context.Context, callerID string) ([]tools.SubagentStatus, error) {
	m.mu.Lock()
	subs := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		if a.Depth() > 0 {
			subs = append(subs, a)
		}
	}
	m.mu.Unlock()

	sort.Slice(subs, func(i, j int) bool { return subs[i].ID() < subs[j].ID() })
	out := make([]tools.SubagentStatus, 0, len(subs))
	for _, a := range subs {
		out = append(out, statusOf(a))
	}
	return out, nil
}

// CloseSubagent implements tools.SubagentBackend. It closes the subagent and,
// recursively, every descendant it spawned.
func (m *Manager) CloseSubagent(ctx context.Context, callerID, subagentID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.agents[subagentID] == nil {
		m.mu.Unlock()
		return ErrSubagentNotFound
	}
	toClose := m.descendantsLocked(subagentID)
	m.mu.Unlock()

	for _, a := range toClose {
		a.Close()
	}
	return nil
}

// Shutdown closes every agent and blocks until all subagent run loops have
// exited. It is idempotent and safe to call concurrently.
func (m *Manager) Shutdown() error {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		agents := make([]*Agent, 0, len(m.agents))
		for _, a := range m.agents {
			agents = append(agents, a)
		}
		m.mu.Unlock()

		for _, a := range agents {
			a.Close()
		}
		m.wg.Wait()
		close(m.shutdownDone)
	})
	<-m.shutdownDone
	return nil
}

// openCountLocked counts agents in a live (open) state. Callers hold m.mu.
func (m *Manager) openCountLocked() int {
	n := 0
	for _, a := range m.agents {
		switch a.State() {
		case StateStarting, StateRunning, StateIdle:
			n++
		}
	}
	return n
}

// descendantsLocked returns rootID and every agent transitively spawned by it.
// Callers hold m.mu.
func (m *Manager) descendantsLocked(rootID string) []*Agent {
	var out []*Agent
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		a := m.agents[id]
		if a == nil {
			continue
		}
		out = append(out, a)
		for childID, child := range m.agents {
			if child.CallerID() == id {
				queue = append(queue, childID)
			}
		}
	}
	return out
}

func statusOf(a *Agent) tools.SubagentStatus {
	return tools.SubagentStatus{
		ID:    a.ID(),
		Type:  a.Type(),
		State: a.State().String(),
		Depth: a.Depth(),
		Tail:  a.lastAssistant(),
	}
}
