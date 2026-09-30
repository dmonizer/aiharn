package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
	"aiharn/internal/sessions"
)

const (
	// defaultQueueSize bounds each session's pending-message queue. It matches
	// the remote API's own queue bound.
	defaultQueueSize = 32
	// defaultMaxSessions bounds how many sessions one process may hold,
	// including the default session.
	defaultMaxSessions = 8
	// defaultSessionID and defaultSessionName identify the session the terminal
	// UI drives.
	defaultSessionID   = "default"
	defaultSessionName = "Default"
)

// queuedPrompt is one top-level prompt waiting for the queue worker, stamped
// with the stop epoch that was current when it was enqueued. beginTurn rejects
// a prompt whose stamp no longer matches, which is what makes Cancel drop
// exactly the prompts queued before the stop without discarding later ones.
type queuedPrompt struct {
	content string
	epoch   uint64
}

// Session is one conversation session: its own Runtime (agent tree, approval
// gate, transports), its own transcript, and its own message queue.
type Session struct {
	id         string
	name       string
	createdAt  time.Time
	rt         *Runtime
	queue      chan queuedPrompt
	transcript sessions.Transcript // nil when transcripts are disabled

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	lastError string
	closed    bool

	// turnMu guards the queue worker's active-turn handle and the stop epoch.
	// Every enqueued prompt carries the epoch current at Submit time, so Cancel
	// can advance the epoch and drop exactly the prompts queued before it.
	turnMu     sync.Mutex
	turnCancel context.CancelFunc
	turnEpoch  uint64
}

var (
	_ sessions.Handle = (*Session)(nil)
	_ sessions.Store  = (*SessionManager)(nil)
)

// newSession wraps an already-built runtime in a session and starts its queue
// worker. transcript, when non-nil, is owned (and closed) by the session.
func newSession(id, name string, rt *Runtime, transcript sessions.Transcript) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		id:         id,
		name:       name,
		createdAt:  time.Now(),
		rt:         rt,
		queue:      make(chan queuedPrompt, defaultQueueSize),
		transcript: transcript,
		ctx:        ctx,
		cancel:     cancel,
	}
	s.wg.Add(1)
	go s.runQueue()
	if s.rt.Manager != nil {
		if err := s.rt.Manager.StartTopLoop(s.rt.Agent.ID()); err != nil {
			// A failure here is unexpected: the default/created session is always
			// interactive. Surface it rather than silently leaving the loop off.
			logging.Debug("app: start main loop", slog.String("session_id", id), slog.Any("err", err))
		}
	}
	return s
}

// ID returns the session's stable identifier.
func (s *Session) ID() string { return s.id }

// Name returns the session's display name.
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// CreatedAt returns when the session was created.
func (s *Session) CreatedAt() time.Time { return s.createdAt }

// Model returns the model config name the session's agents were built with.
func (s *Session) Model() string { return s.rt.Summary.Model }

// Channel returns the session's current execution channel name.
func (s *Session) Channel() string { return s.rt.ChannelName() }

// SetChannel switches the session's active execution channel.
func (s *Session) SetChannel(ctx context.Context, name string) error {
	if s.isClosed() {
		return sessions.ErrClosed
	}
	return s.rt.SetChannel(ctx, name)
}

// Agent returns the session's top-level agent.
func (s *Session) Agent() sessions.Agent { return s.rt.Agent }

// Manager returns the session's agent manager, which owns its subagent tree.
func (s *Session) Manager() *agent.Manager { return s.rt.Manager }

// Gate returns the session's approval gate.
func (s *Session) Gate() *approval.Gate { return s.rt.Gate }

// Submit enqueues content for this session. An empty agentID (or the top-level
// agent's own id) addresses the top-level agent through the session queue; any
// other id must name a subagent of this session and is delivered to its inbox.
func (s *Session) Submit(ctx context.Context, agentID, content string) error {
	top := s.rt.Agent
	if s.isClosed() || top.State() == agent.StateClosed {
		return sessions.ErrClosed
	}
	if agentID != "" && agentID != top.ID() {
		if s.rt.Manager == nil {
			return sessions.ErrAgentNotFound
		}
		// A person typed this in the web console, so it is human-authored.
		err := s.rt.Manager.SendSubagentMessage(ctx, top.ID(), agentID, content, llm.OriginHuman)
		switch {
		case errors.Is(err, agent.ErrSubagentNotFound), errors.Is(err, agent.ErrSubagentNotOwned):
			return sessions.ErrAgentNotFound
		default:
			return err
		}
	}
	// Refuse work before enqueueing: a closed session must never accept a
	// message that the queue worker can no longer run.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return sessions.ErrClosed
	default:
	}
	// Stamp and enqueue while holding turnMu so Cancel's epoch advance and old
	// queue drain form one atomic boundary. A submission after that boundary is
	// guaranteed to survive the stop.
	s.turnMu.Lock()
	prompt := queuedPrompt{content: content, epoch: s.turnEpoch}
	select {
	case s.queue <- prompt:
		s.turnMu.Unlock()
		return nil
	case <-ctx.Done():
		s.turnMu.Unlock()
		return ctx.Err()
	case <-s.ctx.Done():
		s.turnMu.Unlock()
		return sessions.ErrClosed
	default:
		s.turnMu.Unlock()
		return sessions.ErrQueueFull
	}
}

// Queued returns the number of messages waiting in the session queue.
func (s *Session) Queued() int { return len(s.queue) }

// Cancel stops every active and queued piece of work in this session without
// closing it. It mirrors the TUI's stop-everything Esc behaviour: agent turns
// are cancelled, queued top-level prompts and subagent tasks are discarded, and
// pending approval requests are denied so no agent remains blocked. The
// returned count is a best-effort count of affected work items.
func (s *Session) Cancel(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.isClosed() {
		return 0, sessions.ErrClosed
	}

	affected := 0
	// Advance the epoch and drain the old queue under the same lock used by
	// Submit. Any prompt accepted after this critical section is stamped with
	// the new epoch and cannot be consumed by this stop.
	s.turnMu.Lock()
	s.turnEpoch++
	turnCancel := s.turnCancel
	for {
		select {
		case <-s.queue:
			affected++
		default:
			goto drained
		}
	}

drained:
	s.turnMu.Unlock()

	// Manager.CancelAll cancels every active agent turn and discards every
	// agent inbox item; its count is authoritative for agent work.
	if s.rt.Manager != nil {
		n, err := s.rt.Manager.CancelAll()
		affected += n
		if err != nil {
			return affected, err
		}
	}
	// turnCancel covers the narrow window where a prompt passed beginTurn but
	// the agent has not yet installed its own turn-cancel handle. It is not
	// counted separately: the agent turn, when already started, is part of the
	// Manager.CancelAll count above.
	if turnCancel != nil {
		turnCancel()
	}

	if s.rt.Gate != nil {
		for _, req := range s.rt.Gate.PendingRequests() {
			affected++
			_ = s.rt.Gate.Decide(req.ID, approval.DecisionDenied)
		}
	}
	return affected, nil
}

// beginTurn publishes the queue worker's turn cancel handle unless a stop has
// happened since the prompt was enqueued. It returns false when the prompt must
// be discarded.
func (s *Session) beginTurn(epoch uint64, cancel context.CancelFunc) bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if epoch != s.turnEpoch {
		return false
	}
	s.turnCancel = cancel
	return true
}

// endTurn clears the queue worker's turn cancel handle once the turn finishes.
// The queue worker runs one turn at a time, so the current handle is always the
// one being cleared.
func (s *Session) endTurn(_ context.CancelFunc) {
	s.turnMu.Lock()
	s.turnCancel = nil
	s.turnMu.Unlock()
}

// LastError returns the error of the most recent failed turn, or "".
func (s *Session) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

// Close shuts the session down: it marks it closed, cancels the queue worker,
// closes the runtime (agents, transports, gate), waits for the worker to exit,
// and closes the session transcript. It is idempotent; the closed flag guarded
// by mu is the idempotence mechanism (a second call returns immediately).
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	err := s.rt.Close()
	s.wg.Wait()
	if s.transcript != nil {
		// Only a transcript created for this session is closed here; a shared
		// Options.Observer stays alive for the other sessions using it.
		err = errors.Join(err, s.transcript.Close())
	}
	return err
}

// runQueue runs one turn per queued message, mirroring the remote API's worker.
func (s *Session) runQueue() {
	defer s.wg.Done()
	for {
		select {
		case prompt := <-s.queue:
			s.setLastError("")
			turnCtx, cancel := context.WithCancel(s.ctx)
			if !s.beginTurn(prompt.epoch, cancel) {
				// Cancel advanced the epoch after this prompt was enqueued; drop
				// it rather than starting a doomed turn.
				cancel()
				continue
			}
			err := s.rt.Agent.Turn(turnCtx, prompt.content)
			cancel()
			s.endTurn(cancel)
			// A cancellation caused by Cancel or session shutdown is not an
			// error; a genuine turn failure still surfaces through LastError.
			if err != nil && s.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				s.setLastError(err.Error())
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// setName renames the session. Only SessionManager calls it.
func (s *Session) setName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// isClosed reports whether Close has run.
func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Session) setLastError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = msg
}

// SessionManagerOptions configures a SessionManager.
type SessionManagerOptions struct {
	Config      *config.Config
	Build       Options // CLI-level overrides plus the transcript factory
	MaxSessions int     // counts every session, including the default one

	// BuildRuntime builds one session's runtime. It defaults to Build; tests
	// substitute it to avoid real transports and model providers.
	BuildRuntime func(ctx context.Context, cfg *config.Config, opts Options) (*Runtime, error)

	// ID, when set, generates a new session id. It defaults to 8 hex characters
	// from crypto/rand; tests substitute a deterministic generator.
	ID func() string
}

// SessionManager owns every session in the process. Each session has its own
// Runtime, so the session layer is what makes more than one conversation
// possible in a single process.
type SessionManager struct {
	cfg          *config.Config
	build        Options
	buildRuntime func(ctx context.Context, cfg *config.Config, opts Options) (*Runtime, error)
	newID        func() string
	max          int

	mu         sync.Mutex
	sessions   map[string]*Session
	order      []string
	defaultID  string
	closedIDs  map[string]bool // ids already closed, so Close is idempotent
	building   int             // in-flight Creates, counted against the cap
	nextNumber int
	closed     bool
}

// NewSessionManager builds the default session (id "default", name "Default")
// and returns a manager that can create further sessions on demand. The default
// session is the one the terminal UI drives. MaxSessions counts it, so the
// default session always occupies one slot.
func NewSessionManager(ctx context.Context, opts SessionManagerOptions) (*SessionManager, error) {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = defaultMaxSessions
	}
	if opts.BuildRuntime == nil {
		opts.BuildRuntime = Build
	}
	if opts.ID == nil {
		opts.ID = newSessionID
	}
	m := &SessionManager{
		cfg:          opts.Config,
		build:        opts.Build,
		buildRuntime: opts.BuildRuntime,
		newID:        opts.ID,
		max:          opts.MaxSessions,
		sessions:     map[string]*Session{},
		defaultID:    defaultSessionID,
		closedIDs:    map[string]bool{},
	}

	rt, transcript, err := m.buildSession(ctx, defaultSessionID, defaultSessionName)
	if err != nil {
		return nil, err
	}
	s := newSession(defaultSessionID, defaultSessionName, rt, transcript)
	m.sessions[defaultSessionID] = s
	m.order = append(m.order, defaultSessionID)
	return m, nil
}

// Default returns the default session: the one the terminal UI drives.
func (m *SessionManager) Default() sessions.Handle {
	h, _ := m.Lookup(m.defaultID)
	return h
}

// Channels returns every configured execution channel, in config order.
func (m *SessionManager) Channels() []sessions.Channel {
	out := make([]sessions.Channel, 0, len(m.cfg.Channels))
	for _, c := range m.cfg.Channels {
		out = append(out, sessions.Channel{Name: c.Name, Type: c.Type})
	}
	return out
}

// DefaultRuntime returns the default session's runtime, for the terminal UI
// wiring. It returns nil once the default session is gone.
func (m *SessionManager) DefaultRuntime() *Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[m.defaultID]; s != nil {
		return s.rt
	}
	return nil
}

// List returns every session in creation order, default first.
func (m *SessionManager) List() []sessions.Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sessions.Handle, 0, len(m.order))
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Lookup returns the session with the given id. An empty id resolves to the
// default session.
func (m *SessionManager) Lookup(id string) (sessions.Handle, bool) {
	if id == "" {
		id = m.defaultID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	return s, true
}

// Create builds a new session and registers it. An empty name auto-names the
// session "Session N"; any other name is validated with sessions.ValidateName
// and trimmed. The runtime is built without holding the manager lock (it opens
// a transport session and can block), while a reserved slot keeps concurrent
// Creates from exceeding MaxSessions.
func (m *SessionManager) Create(ctx context.Context, name string) (sessions.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auto := name == ""
	if !auto {
		trimmed, err := sessions.ValidateName(name)
		if err != nil {
			return nil, err
		}
		name = trimmed
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, sessions.ErrClosed
	}
	if len(m.sessions)+m.building >= m.max {
		m.mu.Unlock()
		return nil, sessions.ErrLimitReached
	}
	id := m.newID()
	if id == m.defaultID || m.sessions[id] != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: session id %q is already in use", id)
	}
	if auto {
		// The number is assigned under the lock and is not rolled back on a
		// failed build, so concurrent Creates can never reuse one.
		m.nextNumber++
		name = fmt.Sprintf("Session %d", m.nextNumber)
	}
	m.building++
	m.mu.Unlock()

	rt, transcript, err := m.buildSession(ctx, id, name)

	m.mu.Lock()
	m.building--
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if m.closed {
		m.mu.Unlock()
		cerr := rt.Close()
		if transcript != nil {
			cerr = errors.Join(cerr, transcript.Close())
		}
		return nil, errors.Join(sessions.ErrClosed, cerr)
	}
	s := newSession(id, name, rt, transcript)
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.mu.Unlock()
	return s, nil
}

// Rename changes a session's display name. An unknown id returns
// sessions.ErrNotFound; a rejected name returns sessions.ErrNameInvalid and
// leaves the current name untouched.
func (m *SessionManager) Rename(id, name string) (sessions.Handle, error) {
	if id == "" {
		id = m.defaultID
	}
	trimmed, err := sessions.ValidateName(name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, sessions.ErrClosed
	}
	s := m.sessions[id]
	if s == nil {
		return nil, sessions.ErrNotFound
	}
	s.setName(trimmed)
	return s, nil
}

// Close closes one non-default session and removes it from the manager. The
// default session belongs to the terminal UI, so closing it returns
// sessions.ErrDefault. Closing an unknown id returns sessions.ErrNotFound;
// closing an already-closed session is not an error.
func (m *SessionManager) Close(ctx context.Context, id string) error {
	if id == "" || id == m.defaultID {
		return sessions.ErrDefault
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closedIDs[id] {
		m.mu.Unlock()
		return nil
	}
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return sessions.ErrNotFound
	}
	delete(m.sessions, id)
	m.order = removeID(m.order, id)
	m.closedIDs[id] = true
	m.mu.Unlock()
	return s.Close()
}

// Shutdown closes every session, including the default one. It is idempotent:
// a second call returns nil.
func (m *SessionManager) Shutdown() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	list := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil {
			list = append(list, s)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, s := range list {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildSession builds one session's runtime and transcript. It must be called
// without m.mu held, because building opens a transport session.
func (m *SessionManager) buildSession(ctx context.Context, id, name string) (*Runtime, sessions.Transcript, error) {
	opts := m.build
	observer, transcript, err := sessionTranscript(opts, id, name)
	if err != nil {
		return nil, nil, err
	}
	opts.Observer = observer
	rt, err := m.buildRuntime(ctx, m.cfg, opts)
	if err != nil {
		if transcript != nil {
			_ = transcript.Close()
		}
		return nil, nil, err
	}
	if transcript != nil {
		// The recorder writes its header lazily, on the first observed item
		// (recorder.ensureHeaderLocked), so metadata set after Build still
		// lands in the transcript's header line.
		transcript.SetMeta(transcriptMeta(id, name, rt.Summary))
	}
	return rt, transcript, nil
}

// transcriptMeta maps a resolved startup Summary onto a transcript header.
func transcriptMeta(id, name string, summary Summary) sessions.TranscriptMeta {
	return sessions.TranscriptMeta{
		ID:        id,
		Name:      name,
		Model:     summary.Model,
		AgentType: summary.AgentType,
		Channel:   summary.Channel,
		Approval:  summary.Approval,
	}
}

// removeID returns ids without the first occurrence of id.
func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// newSessionID returns 8 hex characters from crypto/rand.
func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// timestamp so session creation cannot fail on id generation.
		return fmt.Sprintf("%08x", uint32(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}
