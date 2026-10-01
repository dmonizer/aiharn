package memory

import (
	"strings"
	"testing"
)

func TestStoreScopesAndUniqueIndexes(t *testing.T) {
	m := NewManager()
	a := m.NewStore()
	b := m.NewStore()

	first, err := a.WriteMemory(ScopeLocal, "local a", "content a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.WriteMemory(ScopeGlobal, "global shared", "shared content")
	if err != nil {
		t.Fatal(err)
	}
	third, err := b.WriteMemory(ScopeLocal, "local b", "content b")
	if err != nil {
		t.Fatal(err)
	}

	if first.Index == second.Index || second.Index == third.Index || first.Index == third.Index {
		t.Fatalf("indexes are not unique: %d %d %d", first.Index, second.Index, third.Index)
	}

	localA, err := a.ListMemories(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(localA) != 1 || localA[0].Index != first.Index {
		t.Fatalf("local A = %+v, want [%d]", localA, first.Index)
	}

	localB, err := b.ListMemories(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(localB) != 1 || localB[0].Index != third.Index {
		t.Fatalf("local B = %+v, want [%d]", localB, third.Index)
	}

	global, err := a.ListMemories(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0].Index != second.Index {
		t.Fatalf("global = %+v, want [%d]", global, second.Index)
	}
	globalB, err := b.ListMemories(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(globalB) != 1 || globalB[0].Content != "shared content" {
		t.Fatalf("store b cannot see global memory: %+v", globalB)
	}

	// get_memory resolves across scopes by unique index; local memories are
	// visible only to the binding that wrote them.
	got, err := b.GetMemory(third.Index)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != string(ScopeLocal) || got.Content != "content b" {
		t.Fatalf("GetMemory(%d) = %+v", third.Index, got)
	}
	got, err = a.GetMemory(second.Index)
	if err != nil || got.Scope != string(ScopeGlobal) {
		t.Fatalf("GetMemory(global) = %+v, %v", got, err)
	}
	if _, err := a.GetMemory(99999); err == nil || !strings.Contains(err.Error(), "no memory") {
		t.Fatalf("GetMemory(missing) err = %v", err)
	}
}

func TestWriteMemoryValidation(t *testing.T) {
	m := NewManager()
	s := m.NewStore()

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
	if _, err := ParseScope("global"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseScope("bogus"); err == nil {
		t.Fatal("expected ParseScope error")
	}
}
