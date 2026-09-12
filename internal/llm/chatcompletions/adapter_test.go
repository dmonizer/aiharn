package chatcompletions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiharn/internal/llm"
)

func writeSSE(w io.Writer, payload string) {
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

func drain(t *testing.T, events <-chan llm.Event) []llm.Event {
	t.Helper()
	done := make(chan []llm.Event, 1)
	go func() {
		var all []llm.Event
		for event := range events {
			all = append(all, event)
		}
		done <- all
	}()
	select {
	case all := <-done:
		return all
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close")
		return nil
	}
}

func terminal(t *testing.T, events []llm.Event) llm.Event {
	t.Helper()
	for _, event := range events {
		if event.Type == llm.EventCompleted || event.Type == llm.EventFailed {
			return event
		}
	}
	t.Fatalf("missing terminal event: %+v", events)
	return llm.Event{}
}

func TestStreamTextAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	events, err := NewAdapter(server.URL+"/", "key").Stream(context.Background(), llm.Request{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	all := drain(t, events)
	if len(all) != 3 || all[0].Text != "Hel" || all[1].Text != "lo" {
		t.Fatalf("events = %+v", all)
	}
	got := terminal(t, all)
	if got.Type != llm.EventCompleted || got.FinishReason != "stop" || len(got.Items) != 1 || got.Items[0].Content != "Hello" {
		t.Fatalf("terminal = %+v", got)
	}
	if got.Usage != (llm.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}) {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestStreamFragmentedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"execute_command","arguments":"{\"command\":"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	events, err := NewAdapter(server.URL, "key").Stream(context.Background(), llm.Request{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	got := terminal(t, drain(t, events))
	if got.Type != llm.EventCompleted || got.FinishReason != "stop" || len(got.Items) != 1 {
		t.Fatalf("terminal = %+v", got)
	}
	call := got.Items[0]
	if call.Type != llm.ItemFunctionCall || call.CallID != "call_1" || call.Name != "execute_command" || call.Args != `{"command":"pwd"}` {
		t.Fatalf("call = %+v", call)
	}
}

func TestBuildRequestRoundTrip(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	req := llm.Request{
		Model: "glm", System: "system",
		Input: []llm.Item{
			{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "run"},
			{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "calling"},
			{Type: llm.ItemFunctionCall, CallID: "call_1", Name: "execute_command", Args: `{"command":"pwd"}`},
			{Type: llm.ItemFunctionCallOutput, CallID: "call_1", Content: "/work"},
		},
		Tools: []llm.ToolDefinition{{Name: "execute_command", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	events, err := NewAdapter(server.URL, "key").Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := terminal(t, drain(t, events)); got.Type != llm.EventCompleted {
		t.Fatalf("terminal = %+v", got)
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	assistant := messages[2].(map[string]any)
	if assistant["content"] != "calling" || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant message = %#v", assistant)
	}
	tool := messages[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "/work" {
		t.Fatalf("tool message = %#v", tool)
	}
}
