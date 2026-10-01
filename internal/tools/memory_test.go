package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aiharn/internal/memory"
)

func newMemStore(t *testing.T) memory.Backend {
	t.Helper()
	m, err := memory.NewManager("", "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m.NewStore()
}

func newMemRegistry(t *testing.T, store memory.Backend) *Registry {
	t.Helper()
	reg := New()
	for _, tool := range []Tool{WriteMemory(store), ListMemories(store), GetMemory(store)} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func runTool(t *testing.T, reg *Registry, name string, args string) (string, error) {
	t.Helper()
	return reg.Run(context.Background(), name, json.RawMessage(args))
}

func TestMemoryTools(t *testing.T) {
	reg := newMemRegistry(t, newMemStore(t))

	writeLocal := func(summary, content string) string {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"scope": "local", "summary": summary, "content": content})
		out, err := runTool(t, reg, NameWriteMemory, string(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	writeLocal("first", "content one")
	out := writeLocal("second", "content two")
	if !strings.HasPrefix(out, "wrote memory ") {
		t.Fatalf("write result = %q", out)
	}

	globalArgs, _ := json.Marshal(map[string]string{"scope": "global", "summary": "shared", "content": "shared content"})
	if _, err := runTool(t, reg, NameWriteMemory, string(globalArgs)); err != nil {
		t.Fatal(err)
	}

	list, err := runTool(t, reg, NameListMemories, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "index=1 summary=first") || !strings.Contains(list, "index=2 summary=second") {
		t.Fatalf("local list = %q", list)
	}
	if strings.Contains(list, "shared") {
		t.Fatalf("local list must default to local scope: %q", list)
	}

	listGlobal, err := runTool(t, reg, NameListMemories, `{"scope":"global"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listGlobal, "index=3 summary=shared") {
		t.Fatalf("global list = %q", listGlobal)
	}

	got, err := runTool(t, reg, NameGetMemory, `{"scope":"global","index":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "scope: global") || !strings.Contains(got, "shared content") {
		t.Fatalf("get result = %q", got)
	}

	if _, err := runTool(t, reg, NameGetMemory, `{"index":999}`); err == nil {
		t.Fatal("expected missing memory error")
	}
}

func TestWriteMemoryDefaultsToLocal(t *testing.T) {
	reg := newMemRegistry(t, newMemStore(t))

	args, _ := json.Marshal(map[string]string{"summary": "no scope", "content": "body"})
	out, err := runTool(t, reg, NameWriteMemory, string(args))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(local)") {
		t.Fatalf("write output did not report local scope: %q", out)
	}
	list, err := runTool(t, reg, NameListMemories, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "summary=no scope") {
		t.Fatalf("local list missing defaulted write: %q", list)
	}
	globalList, err := runTool(t, reg, NameListMemories, `{"scope":"global"}`)
	if err != nil {
		t.Fatal(err)
	}
	if globalList != "no memories" {
		t.Fatalf("global list = %q, want no memories", globalList)
	}
}

func TestGetMemoryScopeDefaults(t *testing.T) {
	reg := newMemRegistry(t, newMemStore(t))

	globalArgs, _ := json.Marshal(map[string]string{"scope": "global", "summary": "shared", "content": "shared content"})
	if _, err := runTool(t, reg, NameWriteMemory, string(globalArgs)); err != nil {
		t.Fatal(err)
	}

	// Without a scope, get_memory reads the local scope, which has no memory
	// at this index even though the global scope does.
	if _, err := runTool(t, reg, NameGetMemory, `{"index":1}`); err == nil {
		t.Fatal("expected missing local memory error when scope omitted")
	}
	got, err := runTool(t, reg, NameGetMemory, `{"scope":"global","index":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "scope: global") || !strings.Contains(got, "shared content") {
		t.Fatalf("global get result = %q", got)
	}
}
