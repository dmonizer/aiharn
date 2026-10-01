package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newTestManager(t *testing.T, globalDir, localDir string) *Manager {
	t.Helper()
	m, err := NewManager(globalDir, localDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestWriteMemoryPersistsToCorrectDirs(t *testing.T) {
	globalDir := t.TempDir()
	localDir := t.TempDir()
	s := newTestManager(t, globalDir, localDir).NewStore()

	g, err := s.WriteMemory(ScopeGlobal, "global summary", "global content")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.WriteMemory(ScopeLocal, "local summary", "local content")
	if err != nil {
		t.Fatal(err)
	}

	globalIndex := readFile(t, filepath.Join(globalDir, "index.md"))
	localIndex := readFile(t, filepath.Join(localDir, "index.md"))

	wantGlobalDir := sanitizeDir("global summary")
	wantLocalDir := sanitizeDir("local summary")

	if g.Index != 1 || l.Index != 2 {
		t.Fatalf("indexes = %d, %d, want 1, 2", g.Index, l.Index)
	}
	if got := readFile(t, filepath.Join(globalDir, wantGlobalDir, "memory.md")); got != "global content" {
		t.Fatalf("global memory.md = %q", got)
	}
	if got := readFile(t, filepath.Join(localDir, wantLocalDir, "memory.md")); got != "local content" {
		t.Fatalf("local memory.md = %q", got)
	}

	wantGlobalIndex := "# Memories\n\n- [1](" + wantGlobalDir + "/memory.md) global summary\n"
	wantLocalIndex := "# Memories\n\n- [2](" + wantLocalDir + "/memory.md) local summary\n"
	if globalIndex != wantGlobalIndex {
		t.Fatalf("global index.md = %q, want %q", globalIndex, wantGlobalIndex)
	}
	if localIndex != wantLocalIndex {
		t.Fatalf("local index.md = %q, want %q", localIndex, wantLocalIndex)
	}

	// Each scope directory only contains its own scope's entry.
	if strings.Contains(globalIndex, "local summary") {
		t.Fatalf("global index.md leaked a local entry: %q", globalIndex)
	}
	if strings.Contains(localIndex, "global summary") {
		t.Fatalf("local index.md leaked a global entry: %q", localIndex)
	}
}

func TestNewManagerReloadsIndexesAndLazyLoads(t *testing.T) {
	globalDir := t.TempDir()
	localDir := t.TempDir()

	s1 := newTestManager(t, globalDir, localDir).NewStore()
	g, err := s1.WriteMemory(ScopeGlobal, "reloaded global", "global body")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s1.WriteMemory(ScopeLocal, "reloaded local", "local body")
	if err != nil {
		t.Fatal(err)
	}

	s2 := newTestManager(t, globalDir, localDir).NewStore()

	globals, err := s2.ListMemories(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(globals) != 1 || globals[0].Index != g.Index || globals[0].Summary != "reloaded global" {
		t.Fatalf("reloaded globals = %+v", globals)
	}
	if globals[0].Content != "" {
		t.Fatalf("reloaded entry content was read eagerly: %q", globals[0].Content)
	}

	locals, err := s2.ListMemories(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(locals) != 1 || locals[0].Index != l.Index || locals[0].Summary != "reloaded local" {
		t.Fatalf("reloaded locals = %+v", locals)
	}

	got, err := s2.GetMemory(ScopeGlobal, g.Index)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "global body" || got.loaded != true {
		t.Fatalf("lazy GetMemory = %+v", got)
	}

	// A new write after reload continues the index sequence.
	e, err := s2.WriteMemory(ScopeGlobal, "after reload", "new body")
	if err != nil {
		t.Fatal(err)
	}
	if e.Index != 3 {
		t.Fatalf("index after reload = %d, want 3", e.Index)
	}
}

func TestLocalMemoriesAreSharedAndSurviveClose(t *testing.T) {
	m := newTestManager(t, "", "")
	a := m.NewStore()
	b := m.NewStore()

	e, err := a.WriteMemory(ScopeLocal, "shared local", "content")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.ListMemories(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Index != e.Index {
		t.Fatalf("store b local list = %+v, want [%d]", got, e.Index)
	}
	read, err := b.GetMemory(ScopeLocal, e.Index)
	if err != nil || read.Content != "content" {
		t.Fatalf("store b GetMemory = %+v, %v", read, err)
	}

	// Closing one binding must not remove the shared local memories.
	a.Close()
	after, err := b.ListMemories(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Index != e.Index {
		t.Fatalf("local memories did not survive a.Close: %+v", after)
	}
}

func TestWriteMemoryValidation(t *testing.T) {
	s := newTestManager(t, "", "").NewStore()

	if _, err := s.WriteMemory(ScopeLocal, "  ", "content"); err == nil {
		t.Fatal("expected empty summary error")
	}
	if _, err := s.WriteMemory(ScopeLocal, "summary", "  "); err == nil {
		t.Fatal("expected empty content error")
	}
	if _, err := s.WriteMemory("bogus", "summary", "content"); err == nil {
		t.Fatal("expected invalid scope error")
	}
	if _, err := s.ListMemories("bogus"); err == nil {
		t.Fatal("expected invalid list scope error")
	}
	if _, err := s.GetMemory("bogus", 1); err == nil {
		t.Fatal("expected invalid get scope error")
	}
	if _, err := ParseScope("global"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseScope("bogus"); err == nil {
		t.Fatal("expected ParseScope error")
	}
}

func TestWriteMemoryNormalizesSummaryNewlines(t *testing.T) {
	s := newTestManager(t, t.TempDir(), "").NewStore()
	e, err := s.WriteMemory(ScopeGlobal, "line one\nline two\r\nline three\r", "content")
	if err != nil {
		t.Fatal(err)
	}
	if e.Summary != "line one line two line three" {
		t.Fatalf("summary = %q", e.Summary)
	}
	idx := readFile(t, filepath.Join(s.manager.globalDir, "index.md"))
	// The index body must be a single line for the entry itself.
	if strings.Contains(idx, "line one\nline two") {
		t.Fatalf("index.md contains a multi-line summary: %q", idx)
	}
}

func TestGetMemoryErrors(t *testing.T) {
	s := newTestManager(t, "", "").NewStore()
	e, err := s.WriteMemory(ScopeGlobal, "global only", "content")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetMemory(ScopeGlobal, 99999); err == nil || !strings.Contains(err.Error(), "no memory at index 99999") {
		t.Fatalf("GetMemory(missing) err = %v", err)
	}
	if _, err := s.GetMemory(ScopeLocal, e.Index); err == nil || !strings.Contains(err.Error(), "no memory") {
		t.Fatalf("GetMemory(wrong scope) err = %v", err)
	}
}

func TestDirectoryNaming(t *testing.T) {
	if got := sanitizeDir(""); got != "memory" {
		t.Fatalf("sanitizeDir(empty) = %q, want memory", got)
	}
	if got := sanitizeDir("  ...---   "); got != "memory" {
		t.Fatalf("sanitizeDir(only trimmed) = %q, want memory", got)
	}
	if got := sanitizeDir("Hello, World!"); got != "Hello- World" {
		t.Fatalf("sanitizeDir = %q", got)
	}
	// First 50 runes are kept; the rest is dropped.
	long := strings.Repeat("x", 60) + "tail"
	if got := sanitizeDir(long); got != strings.Repeat("x", 50) {
		t.Fatalf("sanitizeDir(long) = %q, want 50 x's", got)
	}
}

func TestDirectoryCollisionDisambiguation(t *testing.T) {
	dir := t.TempDir()
	s := newTestManager(t, dir, "").NewStore()

	a, err := s.WriteMemory(ScopeGlobal, strings.Repeat("a", 60), "one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.WriteMemory(ScopeGlobal, strings.Repeat("a", 60), "two")
	if err != nil {
		t.Fatal(err)
	}

	base := strings.Repeat("a", 50)
	if a.dir != base {
		t.Fatalf("first dir = %q, want %q", a.dir, base)
	}
	want := base + "-" + strconv.Itoa(b.Index)
	if b.dir != want {
		t.Fatalf("second dir = %q, want %q", b.dir, want)
	}

	idx := readFile(t, filepath.Join(dir, "index.md"))
	if !strings.Contains(idx, "- ["+strconv.Itoa(a.Index)+"]("+base+"/memory.md)") {
		t.Fatalf("index.md missing first link: %q", idx)
	}
	if !strings.Contains(idx, "- ["+strconv.Itoa(b.Index)+"]("+want+"/memory.md)") {
		t.Fatalf("index.md missing disambiguated second link: %q", idx)
	}
}

func TestEmptyDirsAreInMemoryOnly(t *testing.T) {
	m := newTestManager(t, "", "")
	s := m.NewStore()

	e, err := s.WriteMemory(ScopeGlobal, "in memory", "content")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMemory(ScopeGlobal, e.Index)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "content" || !got.loaded {
		t.Fatalf("in-memory GetMemory = %+v", got)
	}
	list, err := s.ListMemories(ScopeGlobal)
	if err != nil || len(list) != 1 {
		t.Fatalf("in-memory list = %+v, %v", list, err)
	}
}

func TestPersistenceExactLayout(t *testing.T) {
	globalDir := t.TempDir()
	localDir := t.TempDir()
	s := newTestManager(t, globalDir, localDir).NewStore()

	e, err := s.WriteMemory(ScopeGlobal, "Hello, World!", "raw content bytes")
	if err != nil {
		t.Fatal(err)
	}

	wantDir := "Hello- World"
	if e.dir != wantDir {
		t.Fatalf("entry dir = %q, want %q", e.dir, wantDir)
	}
	memoryPath := filepath.Join(globalDir, wantDir, "memory.md")
	if got := readFile(t, memoryPath); got != "raw content bytes" {
		t.Fatalf("memory.md = %q, want raw content", got)
	}
	idx := readFile(t, filepath.Join(globalDir, "index.md"))
	wantIdx := "# Memories\n\n- [1](" + wantDir + "/memory.md) Hello, World!\n"
	if idx != wantIdx {
		t.Fatalf("index.md = %q, want %q", idx, wantIdx)
	}
	// The subdirectory must exist and contain exactly memory.md.
	entries, err := os.ReadDir(filepath.Join(globalDir, wantDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "memory.md" {
		t.Fatalf("memory subdir entries = %v", entries)
	}
	// Local dir was not used and therefore has no index yet.
	if _, err := os.Stat(filepath.Join(localDir, "index.md")); !os.IsNotExist(err) {
		t.Fatalf("local index.md should not exist yet: %v", err)
	}
}
