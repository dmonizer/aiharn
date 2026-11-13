package filetools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"

	"aiharn/internal/execution"
)

// Target identifies the execution target a file operation is bound to. The app
// resolves it per call, so a channel switch is visible to an in-flight
// approval and can be rejected.
type Target struct {
	// Channel is the configured channel name the operation runs against.
	Channel string
	// Root is the channel's configured root directory ("" = not configured).
	Root string
	// Generation increases on every successful channel switch. A proposal
	// captured at generation N is rejected when the commit observes generation
	// N+1, even if the channel switched away and back.
	Generation uint64
	// FS is the channel-bound confined filesystem backend.
	FS execution.Filesystem
}

// targetKey is the coordination key for an execution target. Targets with the
// same host and root share mutation serialization and the mutation ledger even
// when they are reached through different conversations.
func (t Target) targetKey() string {
	if t.FS == nil {
		return ""
	}
	return t.FS.Target()
}

// Registry holds the process-wide coordination state: one mutex and mutation
// ledger per execution target. Receipts live here too, scoped by conversation
// runtime and agent, so an untracked change observed by any conversation
// invalidates every receipt for that file.
type Registry struct {
	mu      sync.Mutex
	targets map[string]*targetState
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{targets: map[string]*targetState{}}
}

// TargetFunc resolves the currently selected execution target. The app
// implements it over its live channel selection, so a channel switch between an
// approval prompt and its commit is observable.
type TargetFunc func(ctx context.Context) (Target, error)

// NewSession opens a conversation-runtime-scoped view of the registry. resolve
// must be non-nil; it is called at the start of every operation and again
// before a mutation is published.
func (r *Registry) NewSession(runtimeID string, limits Limits, resolve TargetFunc) *Session {
	return &Session{registry: r, runtimeID: runtimeID, limits: limits.withDefaults(), resolve: resolve}
}

// targetState is the coordination state of one execution target.
type targetState struct {
	// mu serializes file reads and mutations for the target. It is never held
	// while an approval prompt waits for a human.
	mu       sync.Mutex
	files    map[string]*fileRecord
	receipts map[string]*receipt
}

func newTargetState() *targetState {
	return &targetState{files: map[string]*fileRecord{}, receipts: map[string]*receipt{}}
}

// fileRecord is the ledger entry for one canonical root-relative path: the last
// version the tools observed (or produced) and the edits they performed.
type fileRecord struct {
	version   execution.FileVersion
	mutations []mutation
}

// mutation is one tracked edit: the start line of the replaced range.
type mutation struct {
	start int
	end   int
}

// receipt is a server-owned read receipt. It records the exact range returned
// to one agent in one conversation on one channel and root.
type receipt struct {
	id      string
	runtime string
	agent   string
	channel string
	root    string
	path    string
	start   int
	end     int
}

// receiptRequirement describes the read context an edit needs.
type receiptRequirement struct {
	kind  requirementKind
	start int
	end   int
}

type requirementKind int

const (
	// requireAny accepts any receipt for the file (insertion into an empty file).
	requireAny requirementKind = iota
	// requireCoverage accepts one receipt covering [start, end).
	requireCoverage
)

func (r *receipt) satisfies(req receiptRequirement) bool {
	switch req.kind {
	case requireAny:
		return true
	default:
		return r.start <= req.start && r.end >= req.end
	}
}

// atomicSuffix returns the target state for t, creating it when needed.
func (r *Registry) targetState(t Target) *targetState {
	key := t.targetKey()
	if key == "" {
		return newTargetState()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.targets[key]
	if !ok {
		st = newTargetState()
		r.targets[key] = st
	}
	return st
}

// locked runs fn while holding the target's coordination mutex.
func (s *Session) locked(t Target, fn func(st *targetState) error) error {
	st := s.registry.targetState(t)
	st.mu.Lock()
	defer st.mu.Unlock()
	return fn(st)
}

// recordVersion notes the observed version of path. It reports false when a
// version was already known and differs: an untracked modification. Callers
// must invalidate receipts in that case (dropping the path's receipts is
// enough, and every receipt for a path is invalidated, not only those covering
// the changed region).
func (st *targetState) recordVersion(path string, version execution.FileVersion) bool {
	rec, ok := st.files[path]
	if !ok {
		st.files[path] = &fileRecord{version: version}
		return true
	}
	if !rec.version.Equal(version) {
		return false
	}
	return true
}

// forgetPath drops a path's ledger entry and every receipt for it.
func (st *targetState) forgetPath(path string) {
	delete(st.files, path)
	st.dropReceipts(path)
}

// dropReceipts removes every receipt for path.
func (st *targetState) dropReceipts(path string) {
	for id, r := range st.receipts {
		if r.path == path {
			delete(st.receipts, id)
		}
	}
}

// mintReceipt stores a new receipt owned by agentID and returns its opaque id.
// The id is random so a model cannot guess or forge one.
func (st *targetState) mintReceipt(runtimeID, agentID string, t Target, path string, start, end int) (string, error) {
	id, err := newReceiptID()
	if err != nil {
		return "", err
	}
	st.receipts[id] = &receipt{
		id:      id,
		runtime: runtimeID,
		agent:   agentID,
		channel: t.Channel,
		root:    t.Root,
		path:    path,
		start:   start,
		end:     end,
	}
	return id, nil
}

func newReceiptID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", Errorf(CodeIOError, "cannot allocate a read receipt", "retry the operation")
	}
	return hex.EncodeToString(buf[:]), nil
}

// Session is one conversation runtime's view of the registry. All receipts it
// mints are scoped to this runtime and to the calling agent.
type Session struct {
	registry  *Registry
	runtimeID string
	limits    Limits
	resolve   TargetFunc
}

// Limits returns the effective limits of the session.
func (s *Session) Limits() Limits { return s.limits }

// ClearAgent drops every receipt owned by agentID in this conversation runtime.
// It is called when the agent closes.
func (s *Session) ClearAgent(agentID string) {
	s.registry.forgetReceipts(s.runtimeID, agentID, false)
}

// Close drops every receipt owned by this conversation runtime, whichever agent
// minted it. It is called when the conversation ends, is cleared, or is
// replaced. Receipts never survive a process restart.
func (s *Session) Close() {
	s.registry.forgetReceipts(s.runtimeID, "", true)
}

// forgetReceipts removes the receipts of one conversation runtime. With
// allAgents set it sweeps the whole runtime; otherwise it removes only agentID's
// receipts. Each target is handled under its own lock.
func (r *Registry) forgetReceipts(runtimeID, agentID string, allAgents bool) {
	r.mu.Lock()
	targets := make([]*targetState, 0, len(r.targets))
	for _, st := range r.targets {
		targets = append(targets, st)
	}
	r.mu.Unlock()
	for _, st := range targets {
		st.mu.Lock()
		for id, rec := range st.receipts {
			if rec.runtime != runtimeID {
				continue
			}
			if allAgents || rec.agent == agentID {
				delete(st.receipts, id)
			}
		}
		st.mu.Unlock()
	}
}
