package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aiharn/internal/memory"
)

func TestMemoryTools(t *testing.T) {
	store := memory.NewManager().NewStore()
	reg := New()
	for _, tool := range []Tool{WriteMemory(store), ListMemories(store), GetMemory(store)} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}

	writeLocal := func(summary, content string) string {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"scope": "local", "summary": summary, "content": content})
		out, err := reg.Run(context.Background(), NameWriteMemory, args)
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
	if _, err := reg.Run(context.Background(), NameWriteMemory, globalArgs); err != nil {
		t.Fatal(err)
	}

	list, err := reg.Run(context.Background(), NameListMemories, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "index=1 summary=first") || !strings.Contains(list, "index=2 summary=second") {
		t.Fatalf("local list = %q", list)
	}
	if strings.Contains(list, "shared") {
		t.Fatalf("local list must default to local scope: %q", list)
	}

	listGlobal, err := reg.Run(context.Background(), NameListMemories, json.RawMessage(`{"scope":"global"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listGlobal, "index=3 summary=shared") {
		t.Fatalf("global list = %q", listGlobal)
	}

	got, err := reg.Run(context.Background(), NameGetMemory, json.RawMessage(`{"index":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "scope: global") || !strings.Contains(got, "shared content") {
		t.Fatalf("get result = %q", got)
	}

	if _, err := reg.Run(context.Background(), NameGetMemory, json.RawMessage(`{"index":999}`)); err == nil {
		t.Fatal("expected missing memory error")
	}
}
