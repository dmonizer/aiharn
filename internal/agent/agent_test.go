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

type activitySpy struct {
	mu     sync.Mutex
	events []agent.Event
}

func (*activitySpy) ObserveHistory(string, string, []llm.Item) {}
func (s *activitySpy) ObserveEvent(id, typ string, event agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "a1" && typ == "main" {
		s.events = append(s.events, event)
	}
}

func TestTurnRecordsLiveStreamEvenWithoutUIConsumer(t *testing.T) {
	spy := &activitySpy{}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", Observer: spy,
		Client: &testllm.FakeClient{Script: [][]llm.Event{{
			{Type: llm.EventReasoningDelta, Text: "consider"},
			{Type: llm.EventTextDelta, Text: "partial"},
			{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "partial"}}},
		}}},
	})
	if err := a.Turn(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	var user, reasoning, text bool
	for _, ev := range spy.events {
		if ev.Type == agent.EventUser && ev.Text == "question" {
			user = true
		}
		if ev.Type == agent.EventReasoningDelta && ev.Text == "consider" {
			reasoning = true
		}
		if ev.Type == agent.EventText && ev.Text == "partial" {
			text = true
		}
	}
	if !user || !reasoning || !text {
		t.Fatalf("observed events = %+v", spy.events)
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
	if err := reg.Register(tools.ExecuteCommand(ex, gate, 0, "", 0, "main", "main")); err != nil {
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

func TestTurnRecordsHumanOrigin(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	if err := a.Turn(context.Background(), "typed by a person"); err != nil {
		t.Fatal(err)
	}
	hist := a.History()
	if len(hist) == 0 || hist[0].Type != llm.ItemMessage || hist[0].Role != llm.RoleUser {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].Origin != llm.OriginHuman {
		t.Fatalf("turn input origin = %q, want %q", hist[0].Origin, llm.OriginHuman)
	}
}

func TestDrainedInboxPreservesOrigin(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	if !a.Send("queued by a person") {
		t.Fatal("Send failed")
	}
	if !a.SendAgent("[subagent coder (7)] done", &llm.Delivery{
		From: "coder-7", To: "a1", Direction: llm.DirectionUp, Kind: llm.KindReport,
	}) {
		t.Fatal("SendAgent failed")
	}
	if err := a.Turn(context.Background(), "typed by a person"); err != nil {
		t.Fatal(err)
	}
	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(hist), hist)
	}
	if hist[0].Content != "queued by a person" || hist[0].Origin != llm.OriginHuman {
		t.Fatalf("drained human message = %+v", hist[0])
	}
	if hist[1].Content != "[subagent coder (7)] done" || hist[1].Origin != llm.OriginAgent {
		t.Fatalf("drained agent message = %+v", hist[1])
	}
	if got := hist[1].Delivery; got == nil || got.From != "coder-7" || got.To != "a1" ||
		got.Direction != llm.DirectionUp || got.Kind != llm.KindReport {
		t.Fatalf("drained agent message delivery = %+v", got)
	}
	if hist[2].Content != "typed by a person" || hist[2].Origin != llm.OriginHuman {
		t.Fatalf("turn input = %+v", hist[2])
	}
	if hist[2].Delivery != nil {
		t.Fatalf("human turn input must carry no delivery: %+v", hist[2].Delivery)
	}
	// The drained report is reported delivered exactly once, at the point it is
	// appended to history; the human turn input stays a plain EventUser.
	events := drainEvents(a)
	if got := countAgentMessages(events, "[subagent coder (7)] done", false); got != 1 {
		t.Fatalf("drained report delivered events = %d, want exactly 1: %+v", got, events)
	}
}

// TestSendAgentEmitsPendingThenDelivered proves the two EventAgentMessage
// emissions a live UI relies on: Pending true when the message is queued, and
// Pending false once it reaches history.
func TestSendAgentEmitsPendingThenDelivered(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	d := &llm.Delivery{From: "sub-1", To: "a1", Direction: llm.DirectionUp, Kind: llm.KindReport}
	if !a.SendAgent("done", d) {
		t.Fatal("SendAgent failed")
	}
	queued := drainEvents(a)
	var pending bool
	for _, e := range queued {
		if e.Type == agent.EventAgentMessage {
			if !e.Pending || e.Text != "done" || e.Delivery == nil || e.Delivery.From != "sub-1" {
				t.Fatalf("queued event = %+v", e)
			}
			pending = true
		}
	}
	if !pending {
		t.Fatalf("no Pending EventAgentMessage emitted: %+v", queued)
	}

	if err := a.Turn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	delivered := drainEvents(a)
	count := 0
	for _, e := range delivered {
		if e.Type == agent.EventAgentMessage {
			if e.Pending || e.Text != "done" || e.Delivery == nil || e.Delivery.Kind != llm.KindReport {
				t.Fatalf("delivered event = %+v", e)
			}
			count++
		}
		if e.Type == agent.EventUser && e.Text == "done" {
			t.Fatalf("delivered report also rendered as a raw user turn: %+v", e)
		}
	}
	if count != 1 {
		t.Fatalf("delivered EventAgentMessage count = %d, want exactly 1: %+v", count, delivered)
	}
}

// TestHumanSendEmitsNoAgentMessage asserts a plain Send is still a human
// message: no delivery and no agent-message event.
func TestHumanSendEmitsNoAgentMessage(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	if !a.Send("hi") {
		t.Fatal("Send failed")
	}
	for _, e := range drainEvents(a) {
		if e.Type == agent.EventAgentMessage {
			t.Fatalf("human Send emitted an agent-message event: %+v", e)
		}
	}
	if err := a.Turn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	hist := a.History()
	if hist[0].Content != "hi" || hist[0].Origin != llm.OriginHuman || hist[0].Delivery != nil {
		t.Fatalf("human message = %+v", hist[0])
	}
	var users int
	for _, e := range drainEvents(a) {
		if e.Type == agent.EventAgentMessage {
			t.Fatalf("human turn emitted an agent-message event: %+v", e)
		}
		if e.Type == agent.EventUser && e.Text == "go" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("human turn EventUser count = %d, want exactly 1", users)
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

// historyCount returns how many history items carry the given content.
func historyCount(hist []llm.Item, content string) int {
	n := 0
	for _, it := range hist {
		if it.Content == content {
			n++
		}
	}
	return n
}

// countAgentMessages counts EventAgentMessage events for text with the given
// Pending flag. A delivered event is Pending false; an enqueue event is Pending
// true.
func countAgentMessages(events []agent.Event, text string, pending bool) int {
	n := 0
	for _, e := range events {
		if e.Type == agent.EventAgentMessage && e.Text == text && e.Pending == pending {
			n++
		}
	}
	return n
}

// TestEnqueueEmitsPendingTrueUnchanged pins the enqueue-time event: a queued
// agent message reports Pending true once, and nothing is delivered until the
// text reaches a history.
func TestEnqueueEmitsPendingTrueUnchanged(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	d := &llm.Delivery{From: "sub-1", To: "a1", Direction: llm.DirectionUp, Kind: llm.KindReport}
	if !a.SendAgent("queued report", d) {
		t.Fatal("SendAgent failed")
	}
	events := drainEvents(a)
	if got := countAgentMessages(events, "queued report", true); got != 1 {
		t.Fatalf("Pending-true events = %d, want exactly 1: %+v", got, events)
	}
	if got := countAgentMessages(events, "queued report", false); got != 0 {
		t.Fatalf("enqueue emitted %d delivered events before any turn: %+v", got, events)
	}
}

// TestDeliveryCarryingTurnInputEmitsDeliveredNotUser proves a subagent task
// prompt -- a delivery-carrying turn input -- produces exactly one delivered
// EventAgentMessage and no raw EventUser, so a view cannot render it twice (once
// structured, once as "> task").
func TestDeliveryCarryingTurnInputEmitsDeliveredNotUser(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderWithScript([][]llm.Event{finalTurn("ack")})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	const prompt = "TASK implement the parser"
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", prompt)
	if err != nil {
		t.Fatal(err)
	}
	sub := mgr.Agent(id)
	waitFor(t, 2*time.Second, "subagent to finish the task", func() bool {
		return sub.State() == agent.StateIdle
	})

	events := drainEvents(sub)
	if got := countAgentMessages(events, prompt, false); got != 1 {
		t.Fatalf("delivered turn-input events = %d, want exactly 1: %+v", got, events)
	}
	for _, e := range events {
		if e.Type == agent.EventUser && e.Text == prompt {
			t.Fatalf("delivery-carrying turn input also emitted EventUser: %+v", e)
		}
		if e.Type == agent.EventAgentMessage && e.Text == prompt && !e.Pending {
			if e.Delivery == nil || e.Delivery.Direction != llm.DirectionDown || e.Delivery.Kind != llm.KindTask {
				t.Fatalf("delivered task event lost its delivery metadata: %+v", e)
			}
		}
	}
	if n := historyCount(sub.History(), prompt); n != 1 {
		t.Fatalf("task in history %d times, want exactly 1: %+v", n, sub.History())
	}
}

// TestHumanTurnInputEmitsUserOnly proves a person-authored turn input still
// emits exactly one EventUser and no agent-message event.
func TestHumanTurnInputEmitsUserOnly(t *testing.T) {
	a := agent.New(agent.Spec{
		ID: "a1", Type: "main", Model: "m",
		Client: &testllm.FakeClient{Script: [][]llm.Event{finalTurn("ok")}},
	})
	if err := a.Turn(context.Background(), "typed by a person"); err != nil {
		t.Fatal(err)
	}
	var users int
	for _, e := range drainEvents(a) {
		if e.Type == agent.EventAgentMessage {
			t.Fatalf("human turn emitted an agent-message event: %+v", e)
		}
		if e.Type == agent.EventUser {
			users++
			if e.Text != "typed by a person" {
				t.Fatalf("EventUser text = %q", e.Text)
			}
		}
	}
	if users != 1 {
		t.Fatalf("human turn EventUser events = %d, want exactly 1", users)
	}
}

// TestTaskTakenWhilePausedDeliversOnlyOnResume reproduces the ghost delivered
// event: a task taken from the inbox while the subagent is paused must not be
// reported as delivered (it is not in history); the delivered event fires only
// when the resumed turn appends it to history.
func TestTaskTakenWhilePausedDeliversOnlyOnResume(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderWithScript([][]llm.Event{
		finalTurn("first done"), finalTurn("second done"),
	})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "first task")
	if err != nil {
		t.Fatal(err)
	}
	sub := mgr.Agent(id)
	waitFor(t, 2*time.Second, "first task to complete", func() bool { return sub.State() == agent.StateIdle })
	// Let the run loop park in its inbox receive: a task sent next is received
	// even though the agent is paused (the receive is already past the gate).
	time.Sleep(50 * time.Millisecond)
	drainEvents(sub)

	const second = "second task body"
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, second, llm.OriginAgent); err != nil {
		t.Fatal(err)
	}
	// The task is taken from the inbox: it leaves the pending list...
	waitFor(t, 2*time.Second, "task to leave the pending list", func() bool {
		for _, pm := range mgr.PendingMessages() {
			if pm.Text == second {
				return false
			}
		}
		return true
	})
	// ...but it is neither delivered nor in history while held.
	held := drainEvents(sub)
	if got := countAgentMessages(held, second, false); got != 0 {
		t.Fatalf("taken-but-unrun task emitted %d delivered events: %+v", got, held)
	}
	if historyCount(sub.History(), second) != 0 {
		t.Fatalf("taken-but-unrun task is already in history: %+v", sub.History())
	}

	// On resume the task runs: exactly one delivered event and it is in history.
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "resumed task to reach history", func() bool {
		return historyCount(sub.History(), second) == 1
	})
	after := drainEvents(sub)
	if got := countAgentMessages(after, second, false); got != 1 {
		t.Fatalf("resumed task delivered events = %d, want exactly 1: %+v", got, after)
	}
}

// TestTaskTakenThenShutdownDeliversNothing proves the ghost is gone at shutdown:
// a task taken from the inbox but dropped before its turn (here, held by a pause
// and then cancelled by shutdown) emits no delivered event and never appears in
// history.
func TestTaskTakenThenShutdownDeliversNothing(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{MaxAgents: 4, Builder: builderWithScript([][]llm.Event{
		finalTurn("first done"), finalTurn("never run"),
	})})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	if err := mgr.RegisterTop(newTop(t, true)); err != nil {
		t.Fatal(err)
	}
	id, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "first task")
	if err != nil {
		t.Fatal(err)
	}
	sub := mgr.Agent(id)
	waitFor(t, 2*time.Second, "first task to complete", func() bool { return sub.State() == agent.StateIdle })
	time.Sleep(50 * time.Millisecond)
	drainEvents(sub)

	// Hold the subagent so a task sent now is taken from the inbox but held
	// before its turn; shutting down then drops it before it reaches history.
	if err := mgr.SetSubagentPaused(context.Background(), "main", id, true); err != nil {
		t.Fatal(err)
	}
	const queued = "queued then dropped at shutdown"
	if err := mgr.SendSubagentMessage(context.Background(), "main", id, queued, llm.OriginAgent); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "task to leave the pending list", func() bool {
		for _, pm := range mgr.PendingMessages() {
			if pm.Text == queued {
				return false
			}
		}
		return true
	})
	if got := countAgentMessages(drainEvents(sub), queued, false); got != 0 {
		t.Fatalf("taken-but-unrun task emitted %d delivered events: %+v", got, queued)
	}

	if err := mgr.Shutdown(); err != nil {
		t.Fatal(err)
	}
	events := drainEvents(sub)
	if got := countAgentMessages(events, queued, false); got != 0 {
		t.Fatalf("dropped task emitted %d delivered events: %+v", got, events)
	}
	if historyCount(sub.History(), queued) != 0 {
		t.Fatalf("dropped task appears in history: %+v", sub.History())
	}
}

// TestAgentName proves Name() reports the configured spec name and falls back
// to the id (so a top-level agent built without a name still has a non-empty
// display name).
func TestAgentName(t *testing.T) {
	if got := agent.New(agent.Spec{ID: "x"}).Name(); got != "x" {
		t.Fatalf("Name() with no explicit name = %q, want fallback id %q", got, "x")
	}
	if got := agent.New(agent.Spec{ID: "x", Name: "researcher"}).Name(); got != "researcher" {
		t.Fatalf("Name() = %q, want %q", got, "researcher")
	}
}
