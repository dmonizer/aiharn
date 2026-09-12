package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiharn/internal/llm"
)

// sseFrame wraps an event's JSON into a single SSE data frame.
func sseFrame(eventJSON string) string {
	return "data: " + eventJSON + "\n\n"
}

// drainWithTimeout reads every event until the channel closes, failing if the
// stream does not close within timeout.
func drainWithTimeout(t *testing.T, ch <-chan llm.Event, timeout time.Duration) []llm.Event {
	t.Helper()
	collected := make(chan llm.Event, 64)
	go func() {
		for e := range ch {
			collected <- e
		}
		close(collected)
	}()
	var evts []llm.Event
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-collected:
			if !ok {
				return evts
			}
			evts = append(evts, e)
		case <-deadline:
			t.Fatalf("stream did not close within %v", timeout)
		}
	}
}

// terminalOf returns the single terminal event (Completed or Failed) in evts.
func terminalOf(t *testing.T, evts []llm.Event) llm.Event {
	t.Helper()
	for _, e := range evts {
		if e.Type == llm.EventCompleted || e.Type == llm.EventFailed {
			return e
		}
	}
	t.Fatalf("no terminal event in %+v", evts)
	return llm.Event{}
}

func TestStreamTextDeltasAndCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseFrame(`{"type":"response.output_text.delta","delta":"Hel"}`))
		io.WriteString(w, sseFrame(`{"type":"response.output_text.delta","delta":"lo"}`))
		io.WriteString(w, sseFrame(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`))
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := NewAdapter(srv.URL, "sk-test").Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evts := drainWithTimeout(t, ch, 5*time.Second)

	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3 (2 deltas + completed): %+v", len(evts), evts)
	}
	if evts[0].Type != llm.EventTextDelta || evts[0].Text != "Hel" {
		t.Errorf("evt[0] = %+v, want delta Hel", evts[0])
	}
	if evts[1].Type != llm.EventTextDelta || evts[1].Text != "lo" {
		t.Errorf("evt[1] = %+v, want delta lo", evts[1])
	}
	term := evts[2]
	if term.Type != llm.EventCompleted {
		t.Fatalf("terminal = %+v, want Completed", term)
	}
	if term.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", term.FinishReason)
	}
	if len(term.Items) != 1 || term.Items[0].Type != llm.ItemMessage || term.Items[0].Content != "Hello" {
		t.Errorf("Items = %+v, want one assistant message Hello", term.Items)
	}
	if term.Usage.InputTokens != 10 || term.Usage.OutputTokens != 5 || term.Usage.TotalTokens != 15 {
		t.Errorf("Usage = %+v, want 10/5/15", term.Usage)
	}
}

func TestStreamFunctionCallExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseFrame(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"execute_command","arguments":"{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := NewAdapter(srv.URL, "sk-test").Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	term := terminalOf(t, drainWithTimeout(t, ch, 5*time.Second))

	if len(term.Items) != 1 {
		t.Fatalf("Items = %+v, want one function_call", term.Items)
	}
	fc := term.Items[0]
	if fc.Type != llm.ItemFunctionCall || fc.CallID != "call_1" || fc.Name != "execute_command" || fc.Args != `{"cmd":"ls"}` {
		t.Errorf("function_call = %+v", fc)
	}
}

func TestBuildRequestRoundTrip(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseFrame(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	req := llm.Request{
		Model:  "m",
		System: "be helpful",
		Stream: true,
		Input: []llm.Item{
			{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "run ls"},
			{Type: llm.ItemFunctionCall, CallID: "call_1", Name: "execute_command", Args: `{"cmd":"ls"}`},
			{Type: llm.ItemFunctionCallOutput, CallID: "call_1", Content: "file1\nfile2"},
		},
		Tools: []llm.ToolDefinition{{Name: "execute_command", Description: "run a command", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	ch, err := NewAdapter(srv.URL, "sk-test").Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainWithTimeout(t, ch, 5*time.Second)

	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, got)
	}
	if body["model"] != "m" {
		t.Errorf("model = %v, want m", body["model"])
	}
	if body["instructions"] != "be helpful" {
		t.Errorf("instructions = %v, want 'be helpful'", body["instructions"])
	}

	input, ok := body["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %#v, want 3 items", body["input"])
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "run ls" {
		t.Errorf("input[0] = %#v", msg)
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "execute_command" || call["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("input[1] = %#v", call)
	}
	out := input[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || out["output"] != "file1\nfile2" {
		t.Errorf("input[2] = %#v", out)
	}

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1 tool", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	if tool["name"] != "execute_command" || tool["description"] != "run a command" {
		t.Errorf("tool = %#v, want inline Responses function definition", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Errorf("tool uses Chat Completions nesting: %#v", tool)
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok || parameters["type"] != "object" {
		t.Errorf("tool parameters = %#v", tool["parameters"])
	}
}

func TestStreamSetupError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewAdapter(srv.URL, "sk-test").Stream(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Stream returned nil error for non-2xx response")
	}
}

func TestStreamFailedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseFrame(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`))
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := NewAdapter(srv.URL, "sk-test").Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	term := terminalOf(t, drainWithTimeout(t, ch, 5*time.Second))

	if term.Type != llm.EventFailed {
		t.Fatalf("terminal = %+v, want Failed", term)
	}
	if term.Err == nil || !strings.Contains(term.Err.Error(), "boom") {
		t.Errorf("Err = %v, want it to mention boom", term.Err)
	}
}

func TestStreamCancel(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseFrame(`{"type":"response.output_text.delta","delta":"x"}`))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := NewAdapter(srv.URL, "sk-test").Stream(ctx, llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	<-started
	first := <-ch
	if first.Type != llm.EventTextDelta {
		t.Fatalf("first event = %+v, want TextDelta", first)
	}
	cancel()

	for e := range ch {
		if e.Type == llm.EventCompleted || e.Type == llm.EventFailed {
			t.Fatalf("cancellation emitted a terminal event: %+v", e)
		}
	}
}
