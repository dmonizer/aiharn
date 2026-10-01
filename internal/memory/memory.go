// Package memory stores model-authored memories with two scopes: local
// (shared across the sessions launched from one working directory) and global
// (shared across all sessions under one aiharn home). Memories are persisted
// to disk so they survive process restarts.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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

// Entry is one stored memory. Content is lazily read from disk for entries
// loaded from an index; the unexported dir and loaded fields track that state.
type Entry struct {
	Index     int       `json:"index"`
	Scope     string    `json:"scope"`
	Summary   string    `json:"summary"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`

	dir    string // on-disk subdirectory name (the index.md link target)
	loaded bool   // whether Content has been read from (or written to) disk
}

// Backend is the runtime capability the memory tools require.
type Backend interface {
	WriteMemory(scope Scope, summary, content string) (Entry, error)
	ListMemories(scope Scope) ([]Entry, error)
	GetMemory(scope Scope, index int) (Entry, error)
}

// Manager owns both memory stores (one per scope), the shared index sequence,
// and the on-disk locations for each scope. Local memories live in one shared
// map so every store binding on the manager sees the same local memories.
type Manager struct {
	mu        sync.Mutex
	next      int // next index to hand out
	global    map[int]Entry
	local     map[int]Entry
	globalDir string
	localDir  string
}

// NewManager returns a Manager that persists global memories under globalDir
// and local memories under localDir. Each non-empty directory's index.md is
// read at startup; a missing index.md is an empty store, not an error, and an
// empty directory disables persistence for that scope.
func NewManager(globalDir, localDir string) (*Manager, error) {
	m := &Manager{
		global:    map[int]Entry{},
		local:     map[int]Entry{},
		globalDir: globalDir,
		localDir:  localDir,
	}
	if err := m.load(ScopeGlobal, globalDir); err != nil {
		return nil, err
	}
	if err := m.load(ScopeLocal, localDir); err != nil {
		return nil, err
	}
	m.next = 1
	for _, e := range m.global {
		if e.Index >= m.next {
			m.next = e.Index + 1
		}
	}
	for _, e := range m.local {
		if e.Index >= m.next {
			m.next = e.Index + 1
		}
	}
	return m, nil
}

// NewStore binds a new store to the manager. All bindings share both scopes.
func (m *Manager) NewStore() *Store {
	return &Store{manager: m}
}

// Store is one session's view of the memory system.
type Store struct {
	manager *Manager
	closed  bool
}

var _ Backend = (*Store)(nil)

// Close marks the store closed. Local memories are owned by the Manager and are
// left intact.
func (s *Store) Close() {
	if s == nil || s.manager == nil {
		return
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	s.closed = true
}

// WriteMemory validates its arguments, allocates the next index, and stores the
// memory in the requested scope. Persisted scopes are written to disk before
// the call returns; a persistence failure rolls the in-memory entry back.
func (s *Store) WriteMemory(scope Scope, summary, content string) (Entry, error) {
	switch scope {
	case ScopeLocal, ScopeGlobal:
	default:
		return Entry{}, invalidScope(scope)
	}
	summary = normalizeSummary(summary)
	if summary == "" {
		return Entry{}, errors.New("memory: summary must not be empty")
	}
	if strings.TrimSpace(content) == "" {
		return Entry{}, errors.New("memory: content must not be empty")
	}
	m := s.manager
	if m == nil {
		return Entry{}, errors.New("memory: store has no manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.closed {
		return Entry{}, errors.New("memory: store is closed")
	}

	idx := m.next
	m.next++
	entry := Entry{
		Index:     idx,
		Scope:     string(scope),
		Summary:   summary,
		Content:   content,
		CreatedAt: time.Now().UTC(),
		dir:       m.dirFor(scope, idx, summary),
		loaded:    true,
	}
	if scope == ScopeLocal {
		m.local[idx] = entry
	} else {
		m.global[idx] = entry
	}
	if err := m.persist(scope, entry); err != nil {
		if scope == ScopeLocal {
			delete(m.local, idx)
		} else {
			delete(m.global, idx)
		}
		return Entry{}, err
	}
	return entry, nil
}

// ListMemories returns the memories in scope ordered by index.
func (s *Store) ListMemories(scope Scope) ([]Entry, error) {
	if scope != ScopeLocal && scope != ScopeGlobal {
		return nil, invalidScope(scope)
	}
	m := s.manager
	if m == nil {
		return nil, errors.New("memory: store has no manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sortedEntries(scope), nil
}

// GetMemory returns the memory at index in the given scope, reading its
// content from disk on first access if it was loaded from an index.
func (s *Store) GetMemory(scope Scope, index int) (Entry, error) {
	if scope != ScopeLocal && scope != ScopeGlobal {
		return Entry{}, invalidScope(scope)
	}
	m := s.manager
	if m == nil {
		return Entry{}, errors.New("memory: store has no manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	src := m.global
	if scope == ScopeLocal {
		src = m.local
	}
	entry, ok := src[index]
	if !ok {
		return Entry{}, fmt.Errorf("memory: no memory at index %d", index)
	}
	dir := m.scopeDir(scope)
	if entry.loaded || dir == "" {
		return entry, nil
	}

	path := filepath.Join(dir, entry.dir, "memory.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("memory: read memory %d: %w", index, err)
	}
	entry.Content = string(data)
	entry.loaded = true
	if info, err := os.Stat(path); err == nil {
		entry.CreatedAt = info.ModTime()
	}
	src[index] = entry
	return entry, nil
}

func (m *Manager) scopeDir(scope Scope) string {
	if scope == ScopeLocal {
		return m.localDir
	}
	return m.globalDir
}

func (m *Manager) sortedEntries(scope Scope) []Entry {
	src := m.global
	if scope == ScopeLocal {
		src = m.local
	}
	out := make([]Entry, 0, len(src))
	for _, e := range src {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// dirFor returns the on-disk subdirectory for a new entry, appending -<index>
// when the sanitized prefix is already used by a different entry in the scope.
func (m *Manager) dirFor(scope Scope, index int, summary string) string {
	base := sanitizeDir(summary)
	src := m.global
	if scope == ScopeLocal {
		src = m.local
	}
	for _, e := range src {
		if e.Index != index && e.dir == base {
			return base + "-" + strconv.Itoa(index)
		}
	}
	return base
}

func (m *Manager) load(scope Scope, dir string) error {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("memory: read %s: %w", filepath.Join(dir, "index.md"), err)
	}
	src := m.global
	if scope == ScopeLocal {
		src = m.local
	}
	for _, line := range strings.Split(string(data), "\n") {
		idx, d, summary, ok := parseIndexLine(line)
		if !ok {
			continue
		}
		src[idx] = Entry{
			Index:   idx,
			Scope:   string(scope),
			Summary: summary,
			dir:     d,
		}
	}
	return nil
}

func (m *Manager) persist(scope Scope, entry Entry) error {
	dir := m.scopeDir(scope)
	if dir == "" {
		return nil
	}
	subdir := filepath.Join(dir, entry.dir)
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		return fmt.Errorf("memory: create memory dir %s: %w", subdir, err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "memory.md"), []byte(entry.Content), 0o600); err != nil {
		return fmt.Errorf("memory: write memory %d: %w", entry.Index, err)
	}
	return m.writeIndex(scope)
}

func (m *Manager) writeIndex(scope Scope) error {
	dir := m.scopeDir(scope)
	if dir == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Memories\n\n")
	for _, e := range m.sortedEntries(scope) {
		fmt.Fprintf(&b, "- [%d](%s/memory.md) %s\n", e.Index, e.dir, e.Summary)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("memory: create memory dir %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("memory: write index: %w", err)
	}
	return nil
}

func invalidScope(scope Scope) error {
	return fmt.Errorf("memory: invalid scope %q (want %q or %q)", scope, ScopeLocal, ScopeGlobal)
}

// normalizeSummary trims surrounding whitespace and flattens newlines to spaces
// so the summary stays on a single index.md line.
func normalizeSummary(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// sanitizeDir turns the first 50 runes of a summary into a filesystem-safe
// directory name: letters, digits, '-', '_', '.', and spaces are kept, every
// other rune becomes '-', and leading/trailing '-', '.', and spaces are
// trimmed. An empty result becomes "memory".
func sanitizeDir(summary string) string {
	runes := []rune(summary)
	if len(runes) > 50 {
		runes = runes[:50]
	}
	var b strings.Builder
	for _, r := range runes {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-. ")
	if s == "" {
		return "memory"
	}
	return s
}

// parseIndexLine parses one "- [N](dir/memory.md) summary" line. It reports ok
// only for a well-formed line with a positive parseable index and non-empty
// summary, so malformed index lines are skipped best-effort on load.
func parseIndexLine(line string) (int, string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "- [") {
		return 0, "", "", false
	}
	rest := line[len("- ["):]
	close := strings.Index(rest, "]")
	if close < 0 {
		return 0, "", "", false
	}
	idx, err := strconv.Atoi(rest[:close])
	if err != nil || idx <= 0 {
		return 0, "", "", false
	}
	rest = rest[close+1:]
	if !strings.HasPrefix(rest, "(") {
		return 0, "", "", false
	}
	rest = rest[len("("):]
	closeParen := strings.Index(rest, ")")
	if closeParen < 0 {
		return 0, "", "", false
	}
	target := rest[:closeParen]
	rest = rest[closeParen+1:]
	if !strings.HasSuffix(target, "/memory.md") {
		return 0, "", "", false
	}
	dir := strings.TrimSuffix(target, "/memory.md")
	if dir == "" || strings.Contains(dir, "/") {
		return 0, "", "", false
	}
	summary := strings.TrimSpace(rest)
	if summary == "" {
		return 0, "", "", false
	}
	return idx, dir, summary, true
}
