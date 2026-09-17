package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"aiharn/internal/llm"
	"aiharn/internal/tools"
)

// SpawnSpec is what the Manager needs to construct a subagent: a unique id, the
// configured agent type, its depth, and its caller.
type SpawnSpec struct {
	ID            string
	Name          string
	Type          string
	Depth         int
	CallerID      string
	InboxCapacity int
	EventCapacity int
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
	SubagentTypes []tools.SubagentType
	Builder       Builder
}

// Sentinel errors returned by Manager methods, surfaced to the model via tools.
var (
	ErrClosed              = errors.New("agent: manager closed")
	ErrCallerNotFound      = errors.New("agent: caller not found")
	ErrCallerUnavailable   = errors.New("agent: caller is not open")
	ErrSubagentsNotAllowed = errors.New("agent: caller may not spawn subagents")
	ErrMaxDepth            = errors.New("agent: maximum agent depth reached")
	ErrMaxAgents           = errors.New("agent: maximum open agents reached")
	ErrSubagentNotFound    = errors.New("agent: subagent not found")
	ErrSubagentNotOwned    = errors.New("agent: subagent is outside caller's subtree")
	ErrCannotMessageSelf   = errors.New("agent: cannot message self")
	ErrSubagentUnavailable = errors.New("agent: subagent is not open")
	ErrInboxFull           = errors.New("agent: subagent inbox is full")
	// ErrAgentsNotRelated is returned by SendAgentMessage when the target is
	// neither an ancestor nor a descendant of the caller (for example a sibling
	// subagent under a different parent).
	ErrAgentsNotRelated = errors.New("agent: agents are not in the same hierarchy")
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
	agentTypes []tools.SubagentType
	builder    Builder
	closed     bool

	wg           sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}

	roster chan struct{} // pinged (non-blocking) whenever a subagent appears, closes, or changes state
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
	types := append([]tools.SubagentType(nil), opts.SubagentTypes...)
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	return &Manager{
		agents:       map[string]*Agent{},
		maxDepth:     opts.MaxDepth,
		maxAgents:    opts.MaxAgents,
		inboxCap:     opts.InboxCapacity,
		eventCap:     opts.EventCapacity,
		agentTypes:   types,
		builder:      opts.Builder,
		shutdownDone: make(chan struct{}),
		roster:       make(chan struct{}, 1),
	}
}

// ListSubagentTypes implements tools.SubagentBackend. It returns the configured
// spawn targets plus a live snapshot of the same constraints SpawnSubagent
// enforces. The snapshot is advisory; SpawnSubagent rechecks under the lock.
func (m *Manager) ListSubagentTypes(ctx context.Context, callerID string) (tools.SubagentCatalog, error) {
	if err := ctx.Err(); err != nil {
		return tools.SubagentCatalog{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return tools.SubagentCatalog{}, ErrClosed
	}
	caller := m.agents[callerID]
	if caller == nil {
		return tools.SubagentCatalog{}, ErrCallerNotFound
	}
	if !isOpen(caller) {
		return tools.SubagentCatalog{}, ErrCallerUnavailable
	}

	active := m.openCountLocked() + m.building
	catalog := tools.SubagentCatalog{
		Types:         append([]tools.SubagentType(nil), m.agentTypes...),
		CallerDepth:   caller.Depth(),
		MaxDepth:      m.maxDepth,
		ActiveAgents:  active,
		MaxOpenAgents: m.maxAgents,
	}
	if !caller.AllowSubagents() {
		catalog.BlockedReasons = append(catalog.BlockedReasons, "caller has allow_subagents=false")
	}
	if caller.Depth()+1 > m.maxDepth {
		catalog.BlockedReasons = append(catalog.BlockedReasons, "maximum agent depth reached")
	}
	if active >= m.maxAgents {
		catalog.BlockedReasons = append(catalog.BlockedReasons, "maximum open agents reached")
	}
	catalog.CanSpawn = len(catalog.Types) > 0 && len(catalog.BlockedReasons) == 0
	if len(catalog.Types) == 0 {
		catalog.BlockedReasons = append(catalog.BlockedReasons, "no subagent types are configured")
	}
	return catalog, nil
}

// RegisterTop registers the top-level agent (depth 0). It installs the agent's
// lifecycle context but starts no run loop; the TUI drives the top-level agent
// directly.
func (m *Manager) RegisterTop(a *Agent) error {
	if a == nil {
		return errors.New("agent: top-level agent is nil")
	}
	if a.Depth() != 0 || a.CallerID() != "" {
		return errors.New("agent: top-level agent must have depth 0 and no caller")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, ok := m.agents[a.ID()]; ok {
		return fmt.Errorf("agent: duplicate id %q", a.ID())
	}
	if !isOpen(a) {
		return ErrCallerUnavailable
	}
	for _, existing := range m.agents {
		if existing.Depth() == 0 {
			return errors.New("agent: top-level agent is already registered")
		}
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

// Roster returns a channel that receives a notification whenever the set of
// subagents or their statuses may have changed. Subscribers should treat it as
// a coalescing hint to re-read ListSubagents. The channel is never closed.
func (m *Manager) Roster() <-chan struct{} { return m.roster }

// notify pings the roster channel without blocking; a full buffer means a
// subscriber has not yet drained the previous notification, which coalesces.
func (m *Manager) notify() {
	select {
	case m.roster <- struct{}{}:
	default:
	}
}

// SpawnSubagent implements tools.SubagentBackend. It validates the caller's
// right to spawn, reserves a slot against the open-agent limit, builds the
// subagent off the lock, then registers it and starts its run loop.
func (m *Manager) SpawnSubagent(ctx context.Context, callerID, agentType, name, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	if !isOpen(caller) {
		m.mu.Unlock()
		return "", ErrCallerUnavailable
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
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	m.building++
	m.wg.Add(1)
	builder := m.builder
	m.mu.Unlock()
	defer m.wg.Done()
	if builder == nil {
		m.mu.Lock()
		m.building--
		m.mu.Unlock()
		return "", errors.New("agent: no subagent builder configured")
	}

	sub, err := builder(ctx, SpawnSpec{
		ID: id, Name: name, Type: agentType, Depth: depth, CallerID: callerID,
		InboxCapacity: m.inboxCap, EventCapacity: m.eventCap,
	})
	if err != nil {
		m.mu.Lock()
		m.building--
		m.mu.Unlock()
		return "", fmt.Errorf("agent: spawn %q: %w", agentType, err)
	}
	if sub == nil {
		m.mu.Lock()
		m.building--
		m.mu.Unlock()
		return "", fmt.Errorf("agent: spawn %q: builder returned nil agent", agentType)
	}
	if sub.ID() != id || sub.Name() != name || sub.Type() != agentType || sub.Depth() != depth || sub.CallerID() != callerID {
		m.mu.Lock()
		m.building--
		m.mu.Unlock()
		sub.Close()
		return "", fmt.Errorf("agent: spawn %q: builder returned an agent that does not match its spawn spec", agentType)
	}

	m.mu.Lock()
	m.building--
	if m.closed {
		m.mu.Unlock()
		sub.Close()
		return "", ErrClosed
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		sub.Close()
		return "", err
	}
	caller = m.agents[callerID]
	if caller == nil || !isOpen(caller) {
		m.mu.Unlock()
		sub.Close()
		return "", ErrCallerUnavailable
	}
	subCtx, subCancel := context.WithCancel(context.Background())
	sub.setContext(subCtx, subCancel)
	sub.setOnComplete(func(result string) {
		// Deliver the finished report from this subagent to its caller, exactly
		// once. The delivery records the direction relative to the SENDER: a
		// subagent reporting to its caller travels "up" to an ancestor. Agent
		// origin keeps the console from rendering the report as the human's own
		// message.
		caller.SendAgent(
			fmt.Sprintf("[subagent %s (%s)] %s", agentType, id, result),
			&llm.Delivery{From: id, To: callerID, Direction: llm.DirectionUp, Kind: llm.KindReport},
		)
	})
	sub.setOnStateChange(m.notify)
	m.agents[id] = sub
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		sub.run(subCtx)
		if sub.State() == StateErrored {
			sub.releaseResources()
		}
	}()
	m.mu.Unlock()

	m.notify()
	// The initial task prompt is written by the parent model, not a person, and
	// travels "down" to a descendant.
	if sub.SendAgent(prompt, &llm.Delivery{
		From: callerID, To: id, Direction: llm.DirectionDown, Kind: llm.KindTask,
	}) {
		sub.emit(Event{Type: EventTaskQueued, Text: prompt})
	}
	return id, nil
}

// SendSubagentMessage implements tools.SubagentBackend. The CALLER supplies the
// message's origin, because this path serves two very different authors: the
// model's send_subagent_message tool (agent origin) and a person typing in the
// terminal UI or web console (human origin). Deriving it here would mislabel one
// of them, so origin is a parameter rather than a property of this method.
func (m *Manager) SendSubagentMessage(ctx context.Context, callerID, subagentID, message string, origin llm.Origin) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if callerID == subagentID {
		m.mu.Unlock()
		return ErrCannotMessageSelf
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
	m.mu.Unlock()

	switch sub.State() {
	case StateClosed, StateErrored:
		return ErrSubagentUnavailable
	}
	var ok bool
	if origin == llm.OriginAgent {
		// The model's send_subagent_message tool: a mid-conversation message to a
		// descendant, recorded with delivery metadata (down to the recipient).
		ok = sub.SendAgent(message, &llm.Delivery{
			From: callerID, To: subagentID, Direction: llm.DirectionDown, Kind: llm.KindMessage,
		})
	} else {
		// A person typed this in the terminal UI or web console: human origin,
		// and deliberately no delivery metadata.
		ok = sub.Send(message)
	}
	if !ok {
		return ErrInboxFull
	}
	sub.emit(Event{Type: EventTaskQueued, Text: message})
	return nil
}

// SendAgentMessage enqueues text from callerID to a related agent: a DESCENDANT
// (a subagent anywhere in the caller's subtree) or an ANCESTOR (the agent that
// spawned the caller, or one of its ancestors). A Manager serves exactly one
// session, so both agents necessarily share a hierarchy. kind labels why the
// message was sent (llm.KindMessage for an ordinary message). The delivery's
// direction is relative to the SENDER: down to a descendant, up to an ancestor.
func (m *Manager) SendAgentMessage(ctx context.Context, callerID, targetID, text, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if callerID == targetID {
		m.mu.Unlock()
		return ErrCannotMessageSelf
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
	target := m.agents[targetID]
	if target == nil {
		m.mu.Unlock()
		return ErrSubagentNotFound
	}
	var direction string
	switch {
	case m.isDescendantLocked(callerID, targetID):
		direction = llm.DirectionDown
	case m.isAncestorLocked(callerID, targetID):
		direction = llm.DirectionUp
	default:
		m.mu.Unlock()
		return ErrAgentsNotRelated
	}
	m.mu.Unlock()

	switch target.State() {
	case StateClosed, StateErrored:
		return ErrSubagentUnavailable
	}
	delivery := &llm.Delivery{From: callerID, To: targetID, Direction: direction, Kind: kind}
	if !target.SendAgent(text, delivery) {
		return ErrInboxFull
	}
	return nil
}

// PendingMessage is one agent-authored message queued in a recipient's inbox but
// not yet injected into its history. A polling API client, which has no event
// stream, reads these through Manager.PendingMessages.
type PendingMessage struct {
	From      string
	To        string
	Direction string
	Kind      string
	Text      string
}

// PendingMessages returns every agent-authored message currently queued in any
// agent's inbox and not yet drained into a history, in enqueue order. A message
// stops being pending the moment its recipient takes it from the inbox, whether
// or not the resulting turn then succeeds.
func (m *Manager) PendingMessages() []PendingMessage {
	m.mu.Lock()
	agents := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	m.mu.Unlock()

	type sequenced struct {
		seq uint64
		msg PendingMessage
	}
	var all []sequenced
	for _, a := range agents {
		for _, item := range a.pendingAgentMessages() {
			if item.delivery == nil {
				continue
			}
			all = append(all, sequenced{
				seq: item.seq,
				msg: PendingMessage{
					From: item.delivery.From, To: item.delivery.To,
					Direction: item.delivery.Direction, Kind: item.delivery.Kind,
					Text: item.text,
				},
			})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	out := make([]PendingMessage, 0, len(all))
	for _, s := range all {
		out = append(out, s.msg)
	}
	return out
}

// CheckSubagent implements tools.SubagentBackend.
func (m *Manager) CheckSubagent(ctx context.Context, callerID, subagentID string) (tools.SubagentStatus, error) {
	if err := ctx.Err(); err != nil {
		return tools.SubagentStatus{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return tools.SubagentStatus{}, ErrClosed
	}
	caller := m.agents[callerID]
	if caller == nil {
		m.mu.Unlock()
		return tools.SubagentStatus{}, ErrCallerNotFound
	}
	if !isOpen(caller) {
		m.mu.Unlock()
		return tools.SubagentStatus{}, ErrCallerUnavailable
	}
	sub := m.agents[subagentID]
	if sub == nil {
		m.mu.Unlock()
		return tools.SubagentStatus{}, ErrSubagentNotFound
	}
	if !m.isDescendantLocked(callerID, subagentID) {
		m.mu.Unlock()
		return tools.SubagentStatus{}, ErrSubagentNotOwned
	}
	m.mu.Unlock()
	return statusOf(sub), nil
}

// ListSubagents implements tools.SubagentBackend. It lists all subagents
// (depth > 0) in id order, regardless of state.
func (m *Manager) ListSubagents(ctx context.Context, callerID string) ([]tools.SubagentStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	caller := m.agents[callerID]
	if caller == nil {
		m.mu.Unlock()
		return nil, ErrCallerNotFound
	}
	if !isOpen(caller) {
		m.mu.Unlock()
		return nil, ErrCallerUnavailable
	}
	subs := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		if m.isDescendantLocked(callerID, a.ID()) {
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
	if m.agents[subagentID] == nil {
		m.mu.Unlock()
		return ErrSubagentNotFound
	}
	if !m.isDescendantLocked(callerID, subagentID) {
		m.mu.Unlock()
		return ErrSubagentNotOwned
	}
	toClose := m.descendantsLocked(subagentID)
	m.mu.Unlock()

	for _, a := range toClose {
		a.Close()
	}
	m.notify()
	return nil
}

// CancelAll cancels every agent's current turn and discards queued subagent
// tasks. Agents and their execution sessions remain open and reusable. The
// returned count is the number of agents whose active or queued work was
// affected.
func (m *Manager) CancelAll() (int, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, ErrClosed
	}
	agents := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	m.mu.Unlock()

	cancelled := 0
	for _, a := range agents {
		// The top-level inbox contains completed subagent messages rather than
		// queued tasks, so preserve it. Subagent inboxes are work queues.
		if a.cancelWork(a.Depth() > 0) {
			cancelled++
		}
	}
	m.notify()
	return cancelled, nil
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
	m.notify()
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
		ID:     a.ID(),
		Name:   a.Name(),
		Type:   a.Type(),
		State:  a.State().String(),
		Depth:  a.Depth(),
		Tail:   a.lastAssistant(),
		Paused: a.Paused(),
	}
}

func isOpen(a *Agent) bool {
	switch a.State() {
	case StateStarting, StateRunning, StateIdle:
		return true
	default:
		return false
	}
}

// isDescendantLocked reports whether targetID is below callerID in the spawn
// tree. The caller itself is intentionally not considered a descendant.
func (m *Manager) isDescendantLocked(callerID, targetID string) bool {
	for target := m.agents[targetID]; target != nil && target.CallerID() != ""; target = m.agents[target.CallerID()] {
		if target.CallerID() == callerID {
			return true
		}
	}
	return false
}

// isAncestorLocked reports whether targetID is above callerID in the spawn tree:
// the agent that spawned callerID, or one of its ancestors. The caller itself is
// intentionally not considered an ancestor. Callers hold m.mu.
func (m *Manager) isAncestorLocked(callerID, targetID string) bool {
	for a := m.agents[callerID]; a != nil && a.CallerID() != ""; a = m.agents[a.CallerID()] {
		if a.CallerID() == targetID {
			return true
		}
	}
	return false
}
