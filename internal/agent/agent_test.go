package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/execution"
	"aiharn/internal/llm"
	testexec "aiharn/internal/testutil/execution"
	testllm "aiharn/internal/testutil/llm"
	"aiharn/internal/tools"
)

func drainEvents(a *agent.Agent) []agent.Event {
	var out []agent.Event
	for {
		select {
		case e := <-a.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestTurnSimple(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{
				{Type: llm.EventTextDelta, Text: "Hel"},
				{Type: llm.EventTextDelta, Text: "lo"},
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "Hello"},
				}},
			},
		},
	}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if a.State() != agent.StateIdle {
		t.Fatalf("state = %v", a.State())
	}

	hist := a.History()
	if len(hist) != 2 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if hist[0].Type != llm.ItemMessage || hist[0].Role != llm.RoleUser || hist[0].Content != "hi" {
		t.Fatalf("history[0] = %+v", hist[0])
	}
	if hist[1].Content != "Hello" {
		t.Fatalf("history[1] = %+v", hist[1])
	}

	events := drainEvents(a)
	if !hasText(events, "Hel") || !hasText(events, "lo") {
		t.Fatalf("missing streamed text: %+v", events)
	}

	reqs := client.Requests()
	if len(reqs) != 1 {
		t.Fatalf("stream calls = %d", len(reqs))
	}
	if reqs[0].System != "sys" || reqs[0].Model != "m" {
		t.Fatalf("request = %+v", reqs[0])
	}
}

func TestTurnForwardsReasoningConfigurationAndEvents(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{{
		{Type: llm.EventReasoningDelta, Text: "checking"},
		{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"}}},
	}}}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", Client: client,
		ReasoningEffort: "high", ReasoningSummary: "auto",
	})

	if err := a.Turn(context.Background(), "hi"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	reqs := client.Requests()
	if len(reqs) != 1 || reqs[0].ReasoningEffort != "high" || reqs[0].ReasoningSummary != "auto" {
		t.Fatalf("requests = %+v, want reasoning high/auto", reqs)
	}
	events := drainEvents(a)
	var start, delta bool
	for _, event := range events {
		start = start || event.Type == agent.EventReasoningStart
		delta = delta || event.Type == agent.EventReasoningDelta && event.Text == "checking"
	}
	if !start || !delta {
		t.Fatalf("reasoning events = %+v, want start and delta", events)
	}
}

func TestTurnToolCall(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemFunctionCall, CallID: "c1", Name: tools.NameExecuteCommand, Args: `{"command":"pwd"}`},
				}},
			},
			{
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"},
				}},
			},
		},
	}

	ex := testexec.NewSession(func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
		return execution.Result{Stdout: "/home\n", ExitCode: 0}, nil
	})
	gate := approval.NewGate(approval.ModeAllowAll)
	reg := tools.New()
	if err := reg.Register(tools.ExecuteCommand(ex, gate, 0, "", 0)); err != nil {
		t.Fatal(err)
	}

	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client, Tools: reg})
	if err := a.Turn(context.Background(), "run pwd"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if got := len(ex.Calls()); got != 1 {
		t.Fatalf("executor calls = %d", got)
	}
	if ex.Calls()[0].Cmd != "pwd" {
		t.Fatalf("cmd = %q", ex.Calls()[0].Cmd)
	}

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if hist[1].Type != llm.ItemFunctionCall {
		t.Fatalf("history[1] = %+v", hist[1])
	}
	if hist[2].Type != llm.ItemFunctionCallOutput || hist[2].CallID != "c1" {
		t.Fatalf("history[2] = %+v", hist[2])
	}
	if !strings.Contains(hist[2].Content, "/home") {
		t.Fatalf("tool output = %q", hist[2].Content)
	}
	if hist[3].Content != "done" {
		t.Fatalf("history[3] = %+v", hist[3])
	}

	if got := len(client.Requests()); got != 2 {
		t.Fatalf("stream calls = %d", got)
	}
	if len(client.Requests()[1].Tools) != 1 {
		t.Fatalf("second request tools = %+v", client.Requests()[1].Tools)
	}

	events := drainEvents(a)
	if !hasToolCall(events, tools.NameExecuteCommand) {
		t.Fatalf("missing tool-call event: %+v", events)
	}
}

func TestTurnSetupError(t *testing.T) {
	client := &testllm.FakeClient{SetupErr: errors.New("no auth")}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
	if a.State() != agent.StateErrored {
		t.Fatalf("state = %v", a.State())
	}
}

func TestTurnFailedEvent(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{{Type: llm.EventFailed, Err: errors.New("boom")}},
		},
	}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
	if a.State() != agent.StateErrored {
		t.Fatalf("state = %v", a.State())
	}
}

func TestTurnNoTerminalEvent(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{{{Type: llm.EventTextDelta, Text: "x"}}}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error for stream without terminal event")
	}
}

// stubTool returns a fixed string, for exercising the agent's tool-result cap.
type stubTool struct{ name, out string }

func (s stubTool) Definition() llm.ToolDefinition { return llm.ToolDefinition{Name: s.name} }
func (s stubTool) Run(context.Context, json.RawMessage) (string, error) {
	return s.out, nil
}

// toolCallScript runs one stub tool call then completes.
func toolCallScript(name string) [][]llm.Event {
	return [][]llm.Event{
		{{Type: llm.EventCompleted, Items: []llm.Item{
			{Type: llm.ItemFunctionCall, CallID: "c1", Name: name, Args: "{}"},
		}}},
		{{Type: llm.EventCompleted, Items: []llm.Item{
			{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"},
		}}},
	}
}

func TestTurnToolResultTruncated(t *testing.T) {
	reg := tools.New()
	if err := reg.Register(stubTool{name: "big", out: "abcdefghij"}); err != nil {
		t.Fatal(err)
	}
	client := &testllm.FakeClient{Script: toolCallScript("big")}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", System: "sys",
		Client: client, Tools: reg, ToolResultBytes: 5,
	})

	if err := a.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if got := hist[2].Content; got != "abcde\n[result truncated]" {
		t.Fatalf("truncated output = %q", got)
	}
}

func TestTurnToolResultTruncatedUTF8(t *testing.T) {
	reg := tools.New()
	if err := reg.Register(stubTool{name: "big", out: "ééééé"}); err != nil {
		t.Fatal(err)
	}
	client := &testllm.FakeClient{Script: toolCallScript("big")}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", System: "sys",
		Client: client, Tools: reg, ToolResultBytes: 5,
	})

	if err := a.Turn(context.Background(), "go"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	got := a.History()[2].Content
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if got != "éé\n[result truncated]" {
		t.Fatalf("truncated output = %q", got)
	}
}

func TestTurnRequestTimeout(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", System: "sys",
		Client:         blockingClient{},
		RequestTimeout: 50 * time.Millisecond,
	})

	err := a.Turn(context.Background(), "hi")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if a.State() != agent.StateErrored {
		t.Fatalf("state = %v, want errored", a.State())
	}
}

func TestTurnTrimsTranscriptAtCompletedBoundary(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "first"}}}},
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "second"}}}},
	}}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", Client: client,
		TranscriptItems: 2, TranscriptBytes: 8,
	})
	if err := a.Turn(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := a.Turn(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	hist := a.History()
	if len(hist) != 1 || hist[0].Content != "second" {
		t.Fatalf("history = %+v", hist)
	}
	if got := client.Requests(); len(got) != 2 || len(got[1].Input) != 2 {
		t.Fatalf("request history was not bounded before the second turn: %+v", got)
	}
}

func TestTurnTruncatesSingleOversizedTranscriptItemUTF8Safe(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{{
			Type:  llm.EventCompleted,
			Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "éééé"}},
		}},
	}}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", Client: client, TranscriptBytes: 5,
	})
	if err := a.Turn(context.Background(), "long input"); err != nil {
		t.Fatal(err)
	}
	hist := a.History()
	if len(hist) != 1 || hist[0].Content != "éé" || !utf8.ValidString(hist[0].Content) {
		t.Fatalf("history = %+v", hist)
	}
}

func TestTranscriptTrimmingDoesNotRetainOrphanedToolOutput(t *testing.T) {
	reg := tools.New()
	if err := reg.Register(stubTool{name: "tool", out: "result"}); err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", Client: &testllm.FakeClient{Script: toolCallScript("tool")},
		Tools: reg, TranscriptItems: 2,
	})
	if err := a.Turn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, it := range a.History() {
		if it.Type == llm.ItemFunctionCallOutput {
			t.Fatalf("orphaned tool output retained: %+v", a.History())
		}
	}
}

func TestCloseCancelsAndWaitsForActiveTurn(t *testing.T) {
	a := agent.New(agent.Spec{ID: "main", Type: "main", Client: blockingClient{}})
	mgr := agent.NewManager(agent.ManagerOptions{})
	if err := mgr.RegisterTop(a); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Turn(context.Background(), "wait") }()
	waitFor(t, time.Second, "turn to start", func() bool { return a.State() == agent.StateRunning })
	a.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("turn err = %v, want cancellation", err)
	}
	if err := a.Turn(context.Background(), "again"); !errors.Is(err, agent.ErrAgentClosed) {
		t.Fatalf("turn after close err = %v", err)
	}
}

func TestTurnAfterCloseDoesNotDrainInbox(t *testing.T) {
	a := agent.New(agent.Spec{ID: "main", Type: "main", Client: &testllm.FakeClient{}})
	if !a.Send("queued") {
		t.Fatal("failed to queue message before close")
	}
	a.Close()
	if a.Send("after close") {
		t.Fatal("Send succeeded after close")
	}
	if err := a.Turn(context.Background(), "after close"); !errors.Is(err, agent.ErrAgentClosed) {
		t.Fatalf("turn after close err = %v", err)
	}
	if got := a.History(); len(got) != 0 {
		t.Fatalf("closed turn mutated history: %+v", got)
	}
}

type recordingObserver struct {
	mu        sync.Mutex
	agentID   string
	agentType string
	items     []llm.Item
}

func (o *recordingObserver) ObserveHistory(agentID, agentType string, items []llm.Item) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.agentID = agentID
	o.agentType = agentType
	o.items = append(o.items, items...)
}

func TestObserverReceivesAppendedHistory(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "Hello"},
				}},
			},
		},
	}
	obs := &recordingObserver{}
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m", System: "sys", Client: client,
		Observer: obs,
	})

	if err := a.Turn(context.Background(), "hi"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.agentID != "a1" || obs.agentType != "main" {
		t.Fatalf("observer saw agent %q/%q, want a1/main", obs.agentID, obs.agentType)
	}
	if len(obs.items) != 2 {
		t.Fatalf("observer items = %+v, want user + assistant", obs.items)
	}
	if obs.items[0].Role != llm.RoleUser || obs.items[0].Content != "hi" {
		t.Fatalf("observer items[0] = %+v", obs.items[0])
	}
	if obs.items[1].Role != llm.RoleAssistant || obs.items[1].Content != "Hello" {
		t.Fatalf("observer items[1] = %+v", obs.items[1])
	}
}

func hasText(events []agent.Event, text string) bool {
	for _, e := range events {
		if e.Type == agent.EventText && e.Text == text {
			return true
		}
	}
	return false
}

func hasToolCall(events []agent.Event, name string) bool {
	for _, e := range events {
		if e.Type == agent.EventToolCall && e.Call.Name == name {
			return true
		}
	}
	return false
}
