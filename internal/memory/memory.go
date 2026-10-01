// Package memory stores model-authored memories with two scopes: local
// (private to one conversation session) and global (shared across sessions).
// Indexes are allocated from one sequence so an index is unique across both
// scopes, which is what lets get_memory(index) resolve without a scope.
package memory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Scope selects which store a memory is written to or read from.
type Scope string

const (
	ScopeLocal  Scope = "local"
	ScopeGlobal Scope = "global"
)

// ParseScope converts a tool argument to a Scope.
func ParseScope(s string) (Scope, error) {
	switch Scope(s) {
	case ScopeLocal:
		return ScopeLocal, nil
	case ScopeGlobal:
		return ScopeGlobal, nil
	default:
		return "", fmt.Errorf("memory: invalid scope %q (want %q or %q)", s, ScopeLocal, ScopeGlobal)
	}
}

// Entry is one stored memory.
type Entry struct {
	Index     int       `json:"index"`
	Scope     string    `json:"scope"`
	Summary   string    `json:"summary"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Backend is the runtime capability the memory tools require. app builds a
// per-session Store that shares the process-wide global store.
type Backend interface {
	WriteMemory(scope Scope, summary, content string) (Entry, error)
	ListMemories(scope Scope) ([]Entry, error)
	GetMemory(index int) (Entry, error)
}

// Manager owns the global memory store and the shared index sequence. A
// conversation session binds itself to the manager with NewStore.
type Manager struct {
	mu       sync.Mutex
	next     int
	global   map[int]Entry
	locals   map[int]map[int]Entry
	nextBind int
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{
		global: map[int]Entry{},
		locals: map[int]map[int]Entry{},
	}
}

// NewStore binds a new local store to the manager. Local writes are private to
// this binding; global writes are visible to every binding on the manager.
func (m *Manager) NewStore() *Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextBind++
	m.locals[m.nextBind] = map[int]Entry{}
	return &Store{manager: m, id: m.nextBind}
}

// Store is one session's view of the memory system.
type Store struct {
	manager *Manager
	id      int
	closed  bool
}

var _ Backend = (*Store)(nil)

// Close removes this binding's local memories. Global memories are owned by the
// Manager and are left intact.
func (s *Store) Close() {
	if s == nil || s.manager == nil {
		return
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	s.closed = true
	delete(m.locals, s.id)
}

// WriteMemory validates its arguments, allocates the next index, and stores the
// memory in the requested scope.
func (s *Store) WriteMemory(scope Scope, summary, content string) (Entry, error) {
	switch scope {
	case ScopeLocal, ScopeGlobal:
	default:
		return Entry{}, fmt.Errorf("memory: invalid scope %q (want %q or %q)", scope, ScopeLocal, ScopeGlobal)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return Entry{}, errors.New("memory: summary must not be empty")
	}
	if strings.TrimSpace(content) == "" {
		return Entry{}, errors.New("memory: content must not be empty")
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.closed {
		return Entry{}, errors.New("memory: store is closed")
	}

	m.next++
	entry := Entry{
		Index:     m.next,
		Scope:     string(scope),
		Summary:   summary,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}
	if scope == ScopeLocal {
		m.locals[s.id][entry.Index] = entry
	} else {
		m.global[entry.Index] = entry
	}
	return entry, nil
}

// ListMemories returns the memories in scope ordered by index.
func (s *Store) ListMemories(scope Scope) ([]Entry, error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()

	var src map[int]Entry
	switch scope {
	case ScopeLocal:
		src = m.locals[s.id]
	case ScopeGlobal:
		src = m.global
	default:
		return nil, fmt.Errorf("memory: invalid scope %q (want %q or %q)", scope, ScopeLocal, ScopeGlobal)
	}
	out := make([]Entry, 0, len(src))
	for _, entry := range src {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// GetMemory returns the memory with the given index, searching local first and
// then global. Indexes are unique across both scopes, so no scope is needed.
func (s *Store) GetMemory(index int) (Entry, error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := m.locals[s.id][index]; ok {
		return entry, nil
	}
	if entry, ok := m.global[index]; ok {
		return entry, nil
	}
	return Entry{}, fmt.Errorf("memory: no memory at index %d", index)
}
