package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/llm"
	testllm "aiharn/internal/testutil/llm"
	"aiharn/internal/tools"
)

// finalTurn is a script entry producing one completed assistant message.
func finalTurn(content string) []llm.Event {
	return []llm.Event{{
		Type: llm.EventCompleted,
		Items: []llm.Item{
			{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: content},
		},
	}}
}

// builderWithScript returns a Builder that gives each agent a fresh client whose
// script is a copy of s, and which may spawn subagents.
func builderWithScript(s [][]llm.Event) agent.Builder {
	return func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		cpy := make([][]llm.Event, len(s))
		for i := range s {
			cpy[i] = append([]llm.Event(nil), s[i]...)
		}
		return agent.New(agent.Spec{
			ID:             spec.ID,
			Name:           spec.Name,
			Type:           spec.Type,
			Depth:          spec.Depth,
			CallerID:       spec.CallerID,
			AllowSubagents: true,
			Client:         &testllm.FakeClient{Script: cpy},
		}), nil
	}
}

// blockingClient's Stream blocks until ctx is cancelled, then closes the channel
// (no terminal event), emulating a long-running model request.
type blockingClient struct{}

func (blockingClient) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	out := make(chan llm.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

func builderBlocking() agent.Builder {
	return func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{
			ID:       spec.ID,
			Type:     spec.Type,
			Depth:    spec.Depth,
			CallerID: spec.CallerID,
			Client:   blockingClient{},
		}), nil
	}
}

func builderError() agent.Builder {
	return func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{
			ID:       spec.ID,
			Type:     spec.Type,
			Depth:    spec.Depth,
			CallerID: spec.CallerID,
			Client:   &testllm.FakeClient{Script: [][]llm.Event{{{Type: llm.EventFailed, Err: errors.New("boom")}}}},
		}), nil
	}
}

// newTop returns a top-level agent with the given allow_subagents flag and a
// client that completes a turn with "top done".
func newTop(t *testing.T, allowSubagents bool) *agent.Agent {
	t.Helper()
	return agent.New(agent.Spec{
		ID:             "main",
		Type:           "main",
		AllowSubagents: allowSubagents,
		Client:         &testllm.FakeClient{Script: [][]llm.Event{finalTurn("top done")}},
	})
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTopLevelDepthZero(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{Builder: builderWithScript(nil)})
	if err := mgr.RegisterTop(newTop(t, false)); err != nil {
		t.Fatal(err)
	}
	if got := mgr.Agent("main").Depth(); got != 0 {
		t.Fatalf("top-level depth = %d, want 0", got)
	}
}

func TestListSubagentTypesCatalogAndSpawnConstraints(t *testing.T) {
	manager := agent.NewManager(agent.ManagerOptions{
		MaxDepth: 1, MaxAgents: 2,
		SubagentTypes: []tools.SubagentType{
			{Name: "operator", Model: "quick", Channel: "local"},
			{Name: "coder", Description: "Code changes", Model: "reasoner", Channel: "ssh", AllowSubagents: true},
		},
		Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{
				ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				AllowSubagents: true, Client: blockingClient{},
			}), nil
		},
	})
	t.Cleanup(func() { _ = manager.Shutdown() })
	if err := manager.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootCatalog, err := manager.ListSubagentTypes(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !rootCatalog.CanSpawn || rootCatalog.CallerDepth != 0 || rootCatalog.ActiveAgents != 1 ||
		len(rootCatalog.Types) != 2 || rootCatalog.Types[0].Name != "coder" || rootCatalog.Types[1].Name != "operator" {
		t.Fatalf("root catalog = %+v", rootCatalog)
	}
	rootCatalog.Types[0].Name = "modified"
	again, err := manager.ListSubagentTypes(ctx, "main")
	if err != nil || again.Types[0].Name != "coder" {
		t.Fatalf("manager catalog mutated: %+v, %v", again, err)
	}

	childID, err := manager.SpawnSubagent(ctx, "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	childCatalog, err := manager.ListSubagentTypes(ctx, childID)
	if err != nil || childCatalog.CanSpawn || childCatalog.CallerDepth != 1 || childCatalog.ActiveAgents != 2 ||
		len(childCatalog.BlockedReasons) != 2 {
		t.Fatalf("child constraints = %+v, %v", childCatalog, err)
	}
	rootCatalog, err = manager.ListSubagentTypes(ctx, "main")
	if err != nil || rootCatalog.CanSpawn || rootCatalog.ActiveAgents != 2 || len(rootCatalog.BlockedReasons) != 1 {
		t.Fatalf("root after spawn = %+v, %v", rootCatalog, err)
	}
	if _, err := manager.ListSubagentTypes(ctx, "missing"); !errors.Is(err, agent.ErrCallerNotFound) {
		t.Fatalf("missing caller error = %v", err)
	}
}

func TestListSubagentTypesNoPermissionOrTypes(t *testing.T) {
	manager := agent.NewManager(agent.ManagerOptions{Builder: builderWithScript(nil)})
	t.Cleanup(func() { _ = manager.Shutdown() })
	if err := manager.RegisterTop(newTop(t, false)); err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.ListSubagentTypes(context.Background(), "main")
	if err != nil || catalog.CanSpawn || len(catalog.BlockedReasons) != 2 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
}

func TestSpawnDepthLimit(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxDepth: 1, MaxAgents: 8, Builder: builderWithScript(nil)})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id1, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t1")
	if err != nil {
		t.Fatalf("spawn depth 1: %v", err)
	}
	if mgr.Agent(id1).Depth() != 1 {
		t.Fatalf("depth = %d, want 1", mgr.Agent(id1).Depth())
	}

	if _, err := mgr.SpawnSubagent(context.Background(), id1, "coder", "", "t2"); !errors.Is(err, agent.ErrMaxDepth) {
		t.Fatalf("spawn depth 2 err = %v, want ErrMaxDepth", err)
	}
}

func TestSpawnMaxAgents(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 2, Builder: builderWithScript(nil)})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t1"); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if _, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t2"); !errors.Is(err, agent.ErrMaxAgents) {
		t.Fatalf("second spawn err = %v, want ErrMaxAgents", err)
	}
}

func TestSpawnSimultaneous(t *testing.T) {
	entered := make(chan struct{}, 1)
	start := make(chan struct{})

	builder := func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-start
		return agent.New(agent.Spec{
			ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
			Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("done")}},
		}), nil
	}

	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 2, Builder: builder})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t")
		}(i)
	}

	<-entered // one spawn has reserved its slot and is inside the builder
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, agent.ErrMaxAgents):
		default:
			t.Fatalf("unexpected spawn error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want 1 (limit must hold under concurrency)", successes)
	}
}

func TestSendSubagentMessageOriginIsChosenByCaller(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{
		finalTurn("done1"), finalTurn("done2"), finalTurn("done3"),
	})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first task to complete", func() bool { return mgr.Agent(id).State() == agent.StateIdle })

	// The same method serves a person (TUI/console -> human) and the model's
	// send_subagent_message tool (-> agent). Each call must keep its origin.
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, "typed by a person", llm.OriginHuman); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "human message to arrive", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle && len(mgr.Agent(id).History()) >= 4
	})
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, "from the model tool", llm.OriginAgent); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "agent message to arrive", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle && len(mgr.Agent(id).History()) >= 6
	})

	hist := mgr.Agent(id).History()
	// history: [task1(agent), done1, human msg(human), reply, agent msg(agent), reply]
	if got := hist[0]; got.Content != "task1" || got.Origin != llm.OriginAgent {
		t.Fatalf("initial task = %+v, want agent origin", got)
	}
	// The spawn prompt is a task travelling down to a descendant.
	if d := hist[0].Delivery; d == nil || d.From != "main" || d.To != id ||
		d.Direction != llm.DirectionDown || d.Kind != llm.KindTask {
		t.Fatalf("spawn prompt delivery = %+v", hist[0].Delivery)
	}
	var human, agentMsg *llm.Item
	for i := range hist {
		switch hist[i].Content {
		case "typed by a person":
			human = &hist[i]
		case "from the model tool":
			agentMsg = &hist[i]
		}
	}
	if human == nil || human.Origin != llm.OriginHuman {
		t.Fatalf("human-delivered message = %+v, want human origin", human)
	}
	// A human message carries no agent delivery metadata.
	if human.Delivery != nil {
		t.Fatalf("human-delivered message must have nil delivery: %+v", human.Delivery)
	}
	if agentMsg == nil || agentMsg.Origin != llm.OriginAgent {
		t.Fatalf("model-delivered message = %+v, want agent origin", agentMsg)
	}
	// The old downward-only path still records a KindMessage delivery.
	if d := agentMsg.Delivery; d == nil || d.From != "main" || d.To != id ||
		d.Direction != llm.DirectionDown || d.Kind != llm.KindMessage {
		t.Fatalf("downward message delivery = %+v", agentMsg.Delivery)
	}
}

// TestSendAgentMessageDown proves caller -> descendant delivery.
func TestSendAgentMessageDown(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{
		finalTurn("done1"), finalTurn("done2"),
	})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first task to complete", func() bool { return mgr.Agent(id).State() == agent.StateIdle })

	if err := mgr.SendAgentMessage(context.Background(), "main", id, "keep going", llm.KindMessage); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "downstream message to arrive", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle && len(mgr.Agent(id).History()) >= 4
	})

	got := findHistoryItem(t, mgr.Agent(id).History(), "keep going")
	if got.Origin != llm.OriginAgent {
		t.Fatalf("downstream message origin = %q, want agent", got.Origin)
	}
	if d := got.Delivery; d == nil || d.From != "main" || d.To != id ||
		d.Direction != llm.DirectionDown || d.Kind != llm.KindMessage {
		t.Fatalf("downstream message delivery = %+v", got.Delivery)
	}
}

// TestSendAgentMessageUp proves descendant -> ancestor delivery, that the
// message waits in the ancestor's inbox until its next turn, and that
// PendingMessages reports it until then.
func TestSendAgentMessageUp(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{finalTurn("sub done")})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	top := newTop(t, true)
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to complete", func() bool { return mgr.Agent(id).State() == agent.StateIdle })

	if err := mgr.SendAgentMessage(context.Background(), id, "main", "progress report", llm.KindMessage); err != nil {
		t.Fatal(err)
	}
	// It waits in the ancestor's inbox; it must not appear before a turn.
	waitFor(t, 2*time.Second, "up message to be queued", func() bool {
		for _, pm := range mgr.PendingMessages() {
			if pm.Text == "progress report" {
				return true
			}
		}
		return false
	})
	if got := top.History(); len(got) != 0 {
		t.Fatalf("up message leaked into history before the turn boundary: %+v", got)
	}

	if err := top.Turn(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	got := findHistoryItem(t, top.History(), "progress report")
	if got.Origin != llm.OriginAgent {
		t.Fatalf("up message origin = %q, want agent", got.Origin)
	}
	if d := got.Delivery; d == nil || d.From != id || d.To != "main" ||
		d.Direction != llm.DirectionUp || d.Kind != llm.KindMessage {
		t.Fatalf("up message delivery = %+v", got.Delivery)
	}
}

// TestSendAgentMessageRejectsSelfSiblingAndEscape covers the hierarchy rules.
func TestSendAgentMessageRejectsSelfSiblingAndEscape(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxDepth: 5, MaxAgents: 16, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	left, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "right")
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.SendAgentMessage(context.Background(), left, left, "me", llm.KindMessage); !errors.Is(err, agent.ErrCannotMessageSelf) {
		t.Fatalf("self message err = %v, want ErrCannotMessageSelf", err)
	}
	if err := mgr.SendAgentMessage(context.Background(), left, right, "sibling", llm.KindMessage); !errors.Is(err, agent.ErrAgentsNotRelated) {
		t.Fatalf("sibling message err = %v, want ErrAgentsNotRelated", err)
	}
	if err := mgr.SendAgentMessage(context.Background(), left, "main", "up", llm.KindMessage); err != nil {
		t.Fatalf("ancestor message err = %v, want nil", err)
	}
	// The old downward-only path still refuses a non-descendant.
	if err := mgr.SendSubagentMessage(context.Background(), left, right, "intrude", llm.OriginAgent); !errors.Is(err, agent.ErrSubagentNotOwned) {
		t.Fatalf("old-path sibling err = %v, want ErrSubagentNotOwned", err)
	}
}

// TestPendingMessagesListsQueuedAndDropsOnDrain proves the polling view the web
// console depends on: a queued agent message is listed until the recipient's
// turn drains it.
func TestPendingMessagesListsQueuedAndDropsOnDrain(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderBlocking()})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	top := newTop(t, true)
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to start running", func() bool { return mgr.Agent(id).State() == agent.StateRunning })

	if err := mgr.SendAgentMessage(context.Background(), id, "main", "ping", llm.KindMessage); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, pm := range mgr.PendingMessages() {
		if pm.Text == "ping" {
			found = true
			if pm.From != id || pm.To != "main" || pm.Direction != llm.DirectionUp || pm.Kind != llm.KindMessage {
				t.Fatalf("pending message = %+v", pm)
			}
		}
	}
	if !found {
		t.Fatalf("queued message not listed as pending: %+v", mgr.PendingMessages())
	}

	if err := top.Turn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, pm := range mgr.PendingMessages() {
		if pm.Text == "ping" {
			t.Fatalf("drained message still pending: %+v", pm)
		}
	}
}

// findHistoryItem returns the history item with the given content, failing if
// none exists.
func findHistoryItem(t *testing.T, hist []llm.Item, content string) llm.Item {
	t.Helper()
	for _, it := range hist {
		if it.Content == content {
			return it
		}
	}
	t.Fatalf("no history item with content %q: %+v", content, hist)
	return llm.Item{}
}

func TestSubagentReuse(t *testing.T) {
	script := [][]llm.Event{finalTurn("done1"), finalTurn("done2")}
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript(script)})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to finish first task", func() bool {
		a := mgr.Agent(id)
		return a != nil && a.State() == agent.StateIdle
	})

	// Still open (reusable).
	if mgr.Agent(id) == nil || mgr.Agent(id).State() != agent.StateIdle {
		t.Fatalf("subagent not idle/open after completion: %+v", mgr.Agent(id))
	}

	// A follow-up sent with agent origin stands in for the model's
	// send_subagent_message tool.
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, "task2", llm.OriginAgent); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to process second task", func() bool {
		return len(mgr.Agent(id).History()) == 4
	})

	hist := mgr.Agent(id).History()
	if hist[2].Role != llm.RoleUser || hist[2].Content != "task2" {
		t.Fatalf("second task not recorded: %+v", hist[2])
	}
	if hist[3].Content != "done2" {
		t.Fatalf("second result = %q", hist[3].Content)
	}
	// The subagent run loop must preserve the origin carried by each queued
	// task: both prompts came from the parent model, not a person.
	if hist[0].Origin != llm.OriginAgent || hist[2].Origin != llm.OriginAgent {
		t.Fatalf("subagent task origins = %q, %q; want %q", hist[0].Origin, hist[2].Origin, llm.OriginAgent)
	}
}

func TestPauseSubagentHoldsQueuedWorkAndResumes(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{Builder: builderWithScript([][]llm.Event{
		finalTurn("first"), finalTurn("second"),
	})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first task to complete", func() bool { return mgr.Agent(id).State() == agent.StateIdle })
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, "task2", llm.OriginAgent); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(mgr.Agent(id).History()); got != 2 {
		t.Fatalf("paused agent history items = %d, want 2", got)
	}
	status, err := mgr.CheckSubagent(context.Background(), "main", id)
	if err != nil || !status.Paused {
		t.Fatalf("paused status = %+v, err=%v", status, err)
	}
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "second task after resume", func() bool { return len(mgr.Agent(id).History()) == 4 })
	if err := mgr.CloseSubagent(context.Background(), "main", id); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, true); !errors.Is(err, agent.ErrSubagentUnavailable) {
		t.Fatalf("pause closed subagent: %v", err)
	}
}

func TestSubagentDeliversExactlyOnceAtTurnBoundary(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{finalTurn("sub done")})})
	top := newTop(t, true)
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to complete", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle
	})

	// The result sits in the caller's inbox, not yet in its history: delivery is
	// deferred to the caller's next legal turn boundary.
	if got := top.History(); len(got) != 0 {
		t.Fatalf("result leaked into history before the turn boundary: %+v", got)
	}

	if err := top.Turn(context.Background(), "continue"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	hist := top.History()
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3 (delivered result + input + reply): %+v", len(hist), hist)
	}
	if !strings.Contains(hist[0].Content, "sub done") || hist[0].Role != llm.RoleUser {
		t.Fatalf("delivered result = %+v", hist[0])
	}
	if !strings.HasPrefix(hist[0].Content, "[subagent coder (") {
		t.Fatalf("delivered result not prefixed with subagent id: %q", hist[0].Content)
	}
	if hist[0].Origin != llm.OriginAgent {
		t.Fatalf("delivered result origin = %q, want %q", hist[0].Origin, llm.OriginAgent)
	}
	if d := hist[0].Delivery; d == nil || d.From != id || d.To != "main" ||
		d.Direction != llm.DirectionUp || d.Kind != llm.KindReport {
		t.Fatalf("delivered report delivery = %+v", hist[0].Delivery)
	}
	if hist[1].Content != "continue" {
		t.Fatalf("input = %+v", hist[1])
	}
	if hist[1].Origin != llm.OriginHuman {
		t.Fatalf("human input origin = %q, want %q", hist[1].Origin, llm.OriginHuman)
	}
	if hist[2].Content != "top done" {
		t.Fatalf("reply = %+v", hist[2])
	}
}

func TestCloseSubagentIdle(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to become idle", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle
	})

	if err := mgr.CloseSubagent(context.Background(), "main", id); err != nil {
		t.Fatal(err)
	}
	if mgr.Agent(id).State() != agent.StateClosed {
		t.Fatalf("state = %v, want closed", mgr.Agent(id).State())
	}
}

func TestCloseSubagentBusy(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderBlocking()})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to start running", func() bool {
		return mgr.Agent(id).State() == agent.StateRunning
	})

	if err := mgr.CloseSubagent(context.Background(), "main", id); err != nil {
		t.Fatal(err)
	}
	if mgr.Agent(id).State() != agent.StateClosed {
		t.Fatalf("state = %v, want closed", mgr.Agent(id).State())
	}

	// Shutdown waits for the cancelled run loop to exit; a hang here is a leak.
	done := make(chan struct{})
	go func() { _ = mgr.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown hung: subagent run loop leaked")
	}
}

func TestCloseSubagentRecursive(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxDepth: 5, MaxAgents: 16, Builder: func(_ context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{
			ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
			AllowSubagents: true, Client: blockingClient{},
		}), nil
	}})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id1, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t1")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := mgr.SpawnSubagent(context.Background(), id1, "coder", "", "t2")
	if err != nil {
		t.Fatal(err)
	}
	id3, err := mgr.SpawnSubagent(context.Background(), id2, "coder", "", "t3")
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.CloseSubagent(context.Background(), "main", id1); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{id1, id2, id3} {
		if mgr.Agent(id).State() != agent.StateClosed {
			t.Fatalf("descendant %q state = %v, want closed", id, mgr.Agent(id).State())
		}
	}
	if mgr.Agent("main").State() == agent.StateClosed {
		t.Fatal("top-level agent was closed by a subagent close")
	}
}

func TestSubagentOperationsCannotEscapeCallerSubtree(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxDepth: 5, MaxAgents: 16, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	defer mgr.Shutdown()
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	left, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "right")
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.SendSubagentMessage(context.Background(), left, right, "intrude", llm.OriginAgent); !errors.Is(err, agent.ErrSubagentNotOwned) {
		t.Fatalf("send err = %v", err)
	}
	if _, err := mgr.CheckSubagent(context.Background(), left, right); !errors.Is(err, agent.ErrSubagentNotOwned) {
		t.Fatalf("check err = %v", err)
	}
	if err := mgr.CloseSubagent(context.Background(), left, "main"); !errors.Is(err, agent.ErrSubagentNotOwned) {
		t.Fatalf("close parent err = %v", err)
	}
	subs, err := mgr.ListSubagents(context.Background(), left)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("left can see agents outside its subtree: %+v", subs)
	}
}

func TestRegisterTopRejectsSecondOrClosedRoot(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RegisterTop(agent.New(agent.Spec{ID: "other", Type: "main", Client: &testllm.FakeClient{}})); err == nil {
		t.Fatal("expected second top-level agent to be rejected")
	}

	closedManager := agent.NewManager(agent.ManagerOptions{})
	closed := agent.New(agent.Spec{ID: "closed", Type: "main", Client: &testllm.FakeClient{}})
	closed.Close()
	if err := closedManager.RegisterTop(closed); !errors.Is(err, agent.ErrCallerUnavailable) {
		t.Fatalf("closed root err = %v", err)
	}
}

func TestErroredAgentReleasesSlot(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 2, Builder: builderError()})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id1, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "subagent to error", func() bool {
		return mgr.Agent(id1).State() == agent.StateErrored
	})

	// The errored subagent no longer counts against the open-agent limit.
	if _, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "t2"); err != nil {
		t.Fatalf("spawn after error should release a slot: %v", err)
	}
}

func TestRosterNotifiesOnStateChange(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}

	// Drain the spawn notification so only a later state change can re-ping.
	select {
	case <-mgr.Roster():
	default:
	}

	waitFor(t, 2*time.Second, "subagent to become idle", func() bool {
		return mgr.Agent(id).State() == agent.StateIdle
	})

	select {
	case <-mgr.Roster():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no roster notification for the running→idle transition")
	}
}

func TestCancelAllKeepsAgentsReusableAndDropsQueuedSubagentWork(t *testing.T) {
	top := agent.New(agent.Spec{
		ID: "main", Type: "main", AllowSubagents: true, Client: blockingClient{},
	})
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 8, Builder: builderBlocking()})
	defer mgr.Shutdown()
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}

	subID, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "first")
	if err != nil {
		t.Fatal(err)
	}
	topDone := make(chan error, 1)
	go func() { topDone <- top.Turn(context.Background(), "top") }()
	waitFor(t, 2*time.Second, "top and subagent to start", func() bool {
		return top.State() == agent.StateRunning && mgr.Agent(subID).State() == agent.StateRunning
	})
	if !mgr.Agent(subID).Send("queued") {
		t.Fatal("failed to queue second subagent task")
	}

	n, err := mgr.CancelAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cancelled agents = %d, want 2", n)
	}
	if err := <-topDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("top turn error = %v, want cancellation", err)
	}
	waitFor(t, 2*time.Second, "subagent to return idle", func() bool {
		return mgr.Agent(subID).State() == agent.StateIdle
	})
	if top.State() == agent.StateClosed || mgr.Agent(subID).State() == agent.StateClosed {
		t.Fatal("CancelAll closed an agent")
	}

	// The queued task was discarded, but a task sent after the interrupt runs.
	time.Sleep(20 * time.Millisecond)
	if got := mgr.Agent(subID).State(); got != agent.StateIdle {
		t.Fatalf("queued task survived cancellation: state = %v", got)
	}
	if !mgr.Agent(subID).Send("after") {
		t.Fatal("subagent was not reusable after cancellation")
	}
	waitFor(t, 2*time.Second, "reused subagent to start", func() bool {
		return mgr.Agent(subID).State() == agent.StateRunning
	})
	if n, err := mgr.CancelAll(); err != nil || n != 1 {
		t.Fatalf("second CancelAll = (%d, %v), want (1, nil)", n, err)
	}
}

func TestShutdownIdempotentNoLeaks(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 16, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}

	var ids []string
	for i := 0; i < 3; i++ {
		id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	waitFor(t, 2*time.Second, "subagents to finish", func() bool {
		for _, id := range ids {
			if mgr.Agent(id).State() != agent.StateIdle {
				return false
			}
		}
		return true
	})

	run := func() {
		done := make(chan struct{})
		go func() { _ = mgr.Shutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Shutdown hung: run loop leaked")
		}
	}
	run()
	run() // idempotent

	if mgr.Agent("main").State() != agent.StateClosed {
		t.Fatalf("top-level state = %v, want closed", mgr.Agent("main").State())
	}
	for _, id := range ids {
		if mgr.Agent(id).State() != agent.StateClosed {
			t.Fatalf("subagent %q state = %v, want closed", id, mgr.Agent(id).State())
		}
	}
}

// builderDroppingName returns a Builder that deliberately ignores spec.Name, so
// a display name requested by the caller never reaches the built agent. It
// exercises the post-build spawn-spec mismatch guard.
func builderDroppingName() agent.Builder {
	return func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{
			ID:             spec.ID,
			Type:           spec.Type,
			Depth:          spec.Depth,
			CallerID:       spec.CallerID,
			AllowSubagents: true,
			Client:         &testllm.FakeClient{Script: [][]llm.Event{finalTurn("done")}},
		}), nil
	}
}

// TestSpawnSubagentCustomName proves a caller-supplied display name threads all
// the way through: the runtime's Agent.Name(), CheckSubagent status, and
// ListSubagents status all report it.
func TestSpawnSubagentCustomName(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const name = "researcher"

	id, err := mgr.SpawnSubagent(ctx, "main", "coder", name, "task")
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.Agent(id).Name(); got != name {
		t.Fatalf("mgr.Agent(%q).Name() = %q, want %q", id, got, name)
	}

	status, err := mgr.CheckSubagent(ctx, "main", id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != name {
		t.Fatalf("CheckSubagent status.Name = %q, want %q", status.Name, name)
	}

	list, err := mgr.ListSubagents(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != name {
		t.Fatalf("ListSubagents = %+v, want one entry with Name %q", list, name)
	}
}

// TestSpawnSubagentNameDefaultsToID proves an empty or whitespace-only name is
// not an error and defaults to the generated "coder-N" id.
func TestSpawnSubagentNameDefaultsToID(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderWithScript([][]llm.Event{finalTurn("done")})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i, req := range []string{"", "   "} {
		wantID := fmt.Sprintf("coder-%d", i+1)
		id, err := mgr.SpawnSubagent(ctx, "main", "coder", req, "task")
		if err != nil {
			t.Fatalf("SpawnSubagent(name=%q): %v", req, err)
		}
		if id != wantID {
			t.Fatalf("SpawnSubagent(name=%q) id = %q, want %q", req, id, wantID)
		}
		if got := mgr.Agent(id).Name(); got != id {
			t.Fatalf("Name() for request %q = %q, want defaulted id %q", req, got, id)
		}
		status, err := mgr.CheckSubagent(ctx, "main", id)
		if err != nil {
			t.Fatal(err)
		}
		if status.Name != id {
			t.Fatalf("status.Name for request %q = %q, want %q", req, status.Name, id)
		}
	}
}

// TestSpawnSubagentNameMismatchRejected proves the post-build guard rejects a
// builder that does not propagate spec.Name when a custom name was requested,
// while an empty name still succeeds via the id fallback.
func TestSpawnSubagentNameMismatchRejected(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderDroppingName()})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := mgr.SpawnSubagent(ctx, "main", "coder", "researcher", "task"); err == nil ||
		!strings.Contains(err.Error(), "does not match its spawn spec") {
		t.Fatalf("SpawnSubagent with dropped custom name err = %v, want spawn-spec mismatch", err)
	}
	list, err := mgr.ListSubagents(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("mismatched subagent was registered: %+v", list)
	}

	id, err := mgr.SpawnSubagent(ctx, "main", "coder", "", "task")
	if err != nil {
		t.Fatalf("SpawnSubagent with empty name err = %v, want success (id fallback)", err)
	}
	if got := mgr.Agent(id).Name(); got != id {
		t.Fatalf("Name() = %q, want id %q", got, id)
	}
}
