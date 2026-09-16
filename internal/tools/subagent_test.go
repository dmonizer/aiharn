package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aiharn/internal/approval"
	"aiharn/internal/tools"
)

// fakeBackend records subagent operations and returns configurable results.
type fakeBackend struct {
	spawnID    string
	spawnErr   error
	catalog    tools.SubagentCatalog
	catalogErr error

	sendErr error

	status   tools.SubagentStatus
	checkErr error

	list    []tools.SubagentStatus
	listErr error

	closeErr error

	mu           sync.Mutex
	spawnCall    []spawnCall
	sendCalls    []sendCall
	checkCalls   []checkCall
	listCalls    int
	closeCalls   []string
	catalogCalls []string
}

type spawnCall struct{ callerID, agentType, prompt string }
type sendCall struct{ callerID, subagentID, message string }
type checkCall struct{ callerID, subagentID string }

func (f *fakeBackend) ListSubagentTypes(ctx context.Context, callerID string) (tools.SubagentCatalog, error) {
	f.mu.Lock()
	f.catalogCalls = append(f.catalogCalls, callerID)
	f.mu.Unlock()
	return f.catalog, f.catalogErr
}

func (f *fakeBackend) SpawnSubagent(ctx context.Context, callerID, agentType, prompt string) (string, error) {
	f.mu.Lock()
	f.spawnCall = append(f.spawnCall, spawnCall{callerID, agentType, prompt})
	f.mu.Unlock()
	return f.spawnID, f.spawnErr
}

func (f *fakeBackend) SendSubagentMessage(ctx context.Context, callerID, subagentID, message string) error {
	f.mu.Lock()
	f.sendCalls = append(f.sendCalls, sendCall{callerID, subagentID, message})
	f.mu.Unlock()
	return f.sendErr
}

func (f *fakeBackend) CheckSubagent(ctx context.Context, callerID, subagentID string) (tools.SubagentStatus, error) {
	f.mu.Lock()
	f.checkCalls = append(f.checkCalls, checkCall{callerID, subagentID})
	f.mu.Unlock()
	return f.status, f.checkErr
}

func (f *fakeBackend) ListSubagents(ctx context.Context, callerID string) ([]tools.SubagentStatus, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	return f.list, f.listErr
}

func (f *fakeBackend) CloseSubagent(ctx context.Context, callerID, subagentID string) error {
	f.mu.Lock()
	f.closeCalls = append(f.closeCalls, subagentID)
	f.mu.Unlock()
	return f.closeErr
}

func (f *fakeBackend) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawnCall)
}

func (f *fakeBackend) lastSpawn() spawnCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawnCall[0]
}

func TestSpawnSubagentAllowAll(t *testing.T) {
	backend := &fakeBackend{spawnID: "coder-1"}
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.SpawnSubagent(backend, gate, "caller-1"))

	out, err := r.Run(context.Background(), tools.NameSpawnSubagent, json.RawMessage(`{"agent_type":"coder","prompt":"build it"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "spawned subagent coder-1" {
		t.Fatalf("out = %q", out)
	}
	c := backend.lastSpawn()
	if c.callerID != "caller-1" || c.agentType != "coder" || c.prompt != "build it" {
		t.Fatalf("spawn args = %+v", c)
	}
}

func TestSpawnSubagentAskApproved(t *testing.T) {
	backend := &fakeBackend{spawnID: "coder-9"}
	gate := approval.NewGate(approval.ModeAsk)
	r := newRegistry(t, tools.SpawnSubagent(backend, gate, "caller-1"))

	done := make(chan string, 1)
	go func() {
		out, _ := r.Run(context.Background(), tools.NameSpawnSubagent, json.RawMessage(`{"agent_type":"coder","prompt":"task"}`))
		done <- out
	}()

	var req approval.Request
	select {
	case req = <-gate.Pending():
	case <-time.After(time.Second):
		t.Fatal("no approval request surfaced")
	}
	if req.ToolName != tools.NameSpawnSubagent || req.Command != "coder" {
		t.Fatalf("request = %+v", req)
	}
	if err := gate.Decide(req.ID, approval.DecisionApproved); err != nil {
		t.Fatal(err)
	}

	select {
	case out := <-done:
		if out != "spawned subagent coder-9" {
			t.Fatalf("out = %q", out)
		}
	case <-time.After(time.Second):
		t.Fatal("spawn did not return after approval")
	}
	if backend.spawnCount() != 1 {
		t.Fatalf("spawn calls = %d", backend.spawnCount())
	}
}

func TestSpawnSubagentAskDenied(t *testing.T) {
	backend := &fakeBackend{spawnID: "coder-9"}
	gate := approval.NewGate(approval.ModeAsk)
	r := newRegistry(t, tools.SpawnSubagent(backend, gate, "caller-1"))

	done := make(chan string, 1)
	go func() {
		out, _ := r.Run(context.Background(), tools.NameSpawnSubagent, json.RawMessage(`{"agent_type":"coder","prompt":"task"}`))
		done <- out
	}()

	var req approval.Request
	select {
	case req = <-gate.Pending():
	case <-time.After(time.Second):
		t.Fatal("no approval request surfaced")
	}
	if err := gate.Decide(req.ID, approval.DecisionDenied); err != nil {
		t.Fatal(err)
	}

	select {
	case out := <-done:
		if out != "denied by user" {
			t.Fatalf("out = %q", out)
		}
	case <-time.After(time.Second):
		t.Fatal("spawn did not return after denial")
	}
	if backend.spawnCount() != 0 {
		t.Fatalf("spawn ran despite denial (%d calls)", backend.spawnCount())
	}
}

// TestSubagentToolsExemptFromApproval asserts that discovery/send/check/list/close never
// consult the gate, even in Ask mode: they return synchronously with no pending
// request.
func TestSubagentToolsExemptFromApproval(t *testing.T) {
	backend := &fakeBackend{
		catalog: tools.SubagentCatalog{Types: []tools.SubagentType{{Name: "coder", Description: "Implement changes", Model: "gpt", Channel: "local"}}, CanSpawn: true, MaxDepth: 2, MaxOpenAgents: 8, ActiveAgents: 1},
		status:  tools.SubagentStatus{ID: "coder-1", Type: "coder", State: "idle", Depth: 1, Tail: "done"},
		list:    []tools.SubagentStatus{{ID: "coder-1", Type: "coder", State: "idle", Depth: 1}},
	}
	gate := approval.NewGate(approval.ModeAsk)

	tests := []struct {
		name string
		tool tools.Tool
		args string
		want string
	}{
		{tools.NameListSubagentTypes, tools.ListSubagentTypes(backend, "caller-1", true), `{}`, "name=coder model=gpt channel=local allow_subagents=false description=Implement changes"},
		{tools.NameSendSubagentMessage, tools.SendSubagentMessage(backend, "caller-1"), `{"subagent_id":"coder-1","message":"go"}`, "message sent to coder-1"},
		{tools.NameCheckSubagent, tools.CheckSubagent(backend, "caller-1"), `{"subagent_id":"coder-1"}`, "id=coder-1 type=coder state=idle depth=1"},
		{tools.NameListSubagents, tools.ListSubagents(backend, "caller-1"), `{}`, "- id=coder-1 type=coder state=idle depth=1"},
		{tools.NameCloseSubagent, tools.CloseSubagent(backend, "caller-1"), `{"subagent_id":"coder-1"}`, "closed subagent coder-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRegistry(t, tt.tool)
			out, err := r.Run(context.Background(), tt.name, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("out = %q, want substring %q", out, tt.want)
			}
			select {
			case <-gate.Pending():
				t.Fatalf("%s surfaced an approval request but should be exempt", tt.name)
			default:
			}
		})
	}
}

func TestListSubagentTypesReportsLiveConstraintsAndErrors(t *testing.T) {
	backend := &fakeBackend{catalog: tools.SubagentCatalog{
		Types:       []tools.SubagentType{{Name: "operator", Model: "m", Channel: "local", AllowSubagents: false}},
		CallerDepth: 2, MaxDepth: 2, ActiveAgents: 8, MaxOpenAgents: 8,
		BlockedReasons: []string{"maximum agent depth reached", "maximum open agents reached"},
	}}
	r := newRegistry(t, tools.ListSubagentTypes(backend, "planner-1", true))
	out, err := r.Run(context.Background(), tools.NameListSubagentTypes, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name=operator", "caller_depth=2 max_depth=2", "active_agents=8 max_open_agents=8 slots_remaining=0", "spawn_available=false", "maximum agent depth reached", "maximum open agents reached"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog %q missing %q", out, want)
		}
	}
	if len(backend.catalogCalls) != 1 || backend.catalogCalls[0] != "planner-1" {
		t.Fatalf("catalog callers = %v", backend.catalogCalls)
	}
	if _, err := r.Run(context.Background(), tools.NameListSubagentTypes, json.RawMessage(`{"unexpected":true}`)); err == nil {
		t.Fatal("unexpected argument should fail validation")
	}
	backend.catalogErr = errors.New("manager closed")
	if _, err := r.Run(context.Background(), tools.NameListSubagentTypes, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "manager closed") {
		t.Fatalf("manager error = %v", err)
	}
}

func TestListSubagentTypesReportsDisabledSpawnTool(t *testing.T) {
	backend := &fakeBackend{catalog: tools.SubagentCatalog{
		Types:    []tools.SubagentType{{Name: "coder", Model: "m", Channel: "local"}},
		CanSpawn: true, MaxDepth: 2, MaxOpenAgents: 8, ActiveAgents: 1,
	}}
	r := newRegistry(t, tools.ListSubagentTypes(backend, "main", false))
	out, err := r.Run(context.Background(), tools.NameListSubagentTypes, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "spawn_available=false reason=spawn_subagent tool is not enabled for caller") {
		t.Fatalf("catalog = %q, %v", out, err)
	}
}

func TestSubagentToolsPropagateErrors(t *testing.T) {
	backend := &fakeBackend{spawnErr: errors.New("spawn failed")}
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.SpawnSubagent(backend, gate, "caller-1"))

	_, err := r.Run(context.Background(), tools.NameSpawnSubagent, json.RawMessage(`{"agent_type":"coder","prompt":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "spawn failed") {
		t.Fatalf("expected backend error, got %v", err)
	}
}

func TestSubagentToolInvalidArgs(t *testing.T) {
	backend := &fakeBackend{}
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.SpawnSubagent(backend, gate, "caller-1"))

	_, err := r.Run(context.Background(), tools.NameSpawnSubagent, json.RawMessage(`{"prompt":"missing agent_type"}`))
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("expected invalid-arguments error, got %v", err)
	}
	if backend.spawnCount() != 0 {
		t.Fatalf("backend called despite invalid args")
	}
}

func TestListSubagentsEmpty(t *testing.T) {
	backend := &fakeBackend{list: nil}
	r := newRegistry(t, tools.ListSubagents(backend, "caller-1"))

	out, err := r.Run(context.Background(), tools.NameListSubagents, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "no subagents" {
		t.Fatalf("out = %q", out)
	}
}
