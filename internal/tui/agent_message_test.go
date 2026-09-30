package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"aiharn/internal/agent"
	"aiharn/internal/llm"
)

// lastLine returns the newest flushed transcript line.
func lastLine(t *testing.T, m *Model) line {
	t.Helper()
	if len(m.lines) == 0 {
		t.Fatal("transcript is empty")
	}
	return m.lines[len(m.lines)-1]
}

func TestAgentMessageDeliveredUp(t *testing.T) {
	m := newTestModel(t)
	m.appendEvent(agent.Event{
		AgentID:  "a1",
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "coder-1", To: "main", Direction: llm.DirectionUp, Kind: llm.KindReport},
		Text:     "all tests pass",
	})
	ln := lastLine(t, m)
	if want := "↑ coder-1 → main · report  all tests pass"; ln.text != want {
		t.Fatalf("line text = %q, want %q", ln.text, want)
	}
	if ln.kind != kindAgentMessage {
		t.Fatalf("line kind = %d, want kindAgentMessage", ln.kind)
	}
}

func TestAgentMessageDeliveredDown(t *testing.T) {
	m := newTestModel(t)
	m.appendEvent(agent.Event{
		AgentID:  "a1",
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "main", To: "coder-1", Direction: llm.DirectionDown, Kind: llm.KindTask},
		Text:     "implement the parser",
	})
	ln := lastLine(t, m)
	if want := "↓ main → coder-1 · task  implement the parser"; ln.text != want {
		t.Fatalf("line text = %q, want %q", ln.text, want)
	}
	if ln.kind != kindAgentMessage {
		t.Fatalf("line kind = %d, want kindAgentMessage", ln.kind)
	}
}

// TestAgentMessageSeparableFromUserAndAssistant asserts an agent message cannot
// be confused with human input or assistant output, by kind and by text.
func TestAgentMessageSeparableFromUserAndAssistant(t *testing.T) {
	m := newTestModel(t)
	m.appendLine(kindUser, "> human typed this")
	m.appendText("assistant streamed this")
	m.appendEvent(agent.Event{
		AgentID:  "a1",
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "coder-1", To: "main", Direction: llm.DirectionUp, Kind: llm.KindReport},
		Text:     "agent body",
	})

	var agentLine, userLine, assistantLine *line
	for i := range m.lines {
		switch m.lines[i].kind {
		case kindAgentMessage:
			agentLine = &m.lines[i]
		case kindUser:
			userLine = &m.lines[i]
		case kindAssistant:
			assistantLine = &m.lines[i]
		}
	}
	if agentLine == nil || userLine == nil || assistantLine == nil {
		t.Fatalf("missing kinds: agent=%v user=%v assistant=%v lines=%+v", agentLine, userLine, assistantLine, m.lines)
	}
	if agentLine.kind == kindUser || agentLine.kind == kindAssistant {
		t.Fatalf("agent-message kind collides: %d", agentLine.kind)
	}
	if strings.HasPrefix(agentLine.text, "> ") {
		t.Fatalf("agent line looks like human input: %q", agentLine.text)
	}
	if agentLine.text == userLine.text || agentLine.text == assistantLine.text {
		t.Fatalf("agent line text collides: %q", agentLine.text)
	}
	if !strings.Contains(agentLine.text, "↑ coder-1 → main · report") {
		t.Fatalf("agent line missing provenance header: %q", agentLine.text)
	}
}

func TestAgentMessageMultilineBodyPreserved(t *testing.T) {
	m := newTestModel(t)
	m.appendEvent(agent.Event{
		AgentID:  "a1",
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "coder-1", To: "main", Direction: llm.DirectionUp, Kind: llm.KindReport},
		Text:     "line one\nline two",
	})
	if len(m.lines) < 2 {
		t.Fatalf("expected two lines, got %+v", m.lines)
	}
	first := m.lines[len(m.lines)-2]
	second := m.lines[len(m.lines)-1]
	if want := "↑ coder-1 → main · report  line one"; first.text != want {
		t.Fatalf("first line = %q, want %q", first.text, want)
	}
	if second.text != "line two" {
		t.Fatalf("second line = %q, want %q", second.text, "line two")
	}
	if first.kind != kindAgentMessage || second.kind != kindAgentMessage {
		t.Fatalf("kinds = %d,%d want kindAgentMessage", first.kind, second.kind)
	}
}

func TestAgentMessageNilDeliveryDoesNotPanic(t *testing.T) {
	m := newTestModel(t)
	// Must not panic on a malformed/stale event with no Delivery.
	m.appendEvent(agent.Event{AgentID: "a1", Type: agent.EventAgentMessage, Text: "orphan text"})
	ln := lastLine(t, m)
	if !strings.HasPrefix(ln.text, "(agent message)") {
		t.Fatalf("nil-delivery line = %q, want (agent message) header", ln.text)
	}
	if ln.kind != kindAgentMessage {
		t.Fatalf("line kind = %d, want kindAgentMessage", ln.kind)
	}
}

func TestAgentMessageRenderedForNonFocusedAgent(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	if m.focusedID == id {
		t.Fatalf("subagent %s unexpectedly focused", id)
	}
	m.appendAgentEvent(agent.Event{
		AgentID:  id, // recipient is the non-focused subagent
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "main", To: id, Direction: llm.DirectionDown, Kind: llm.KindTask},
		Text:     "do the thing",
	})
	found := false
	for _, ln := range m.views[id].lines {
		if ln.kind == kindAgentMessage && strings.Contains(ln.text, "↓ main → "+id+" · task  do the thing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("message not rendered for non-focused agent %s: %+v", id, m.views[id].lines)
	}
	// It is also visible once that agent is focused, and never dropped.
	m.focusAgent(id)
	found = false
	for _, ln := range m.lines {
		if ln.kind == kindAgentMessage && strings.Contains(ln.text, "do the thing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("message missing after focusing %s: %+v", id, m.lines)
	}
}

func TestAgentMessageFlushesStreamedText(t *testing.T) {
	m := newTestModel(t)
	m.appendText("partial assistant")
	m.appendEvent(agent.Event{
		AgentID:  "a1",
		Type:     agent.EventAgentMessage,
		Delivery: &llm.Delivery{From: "coder-1", To: "main", Direction: llm.DirectionUp, Kind: llm.KindReport},
		Text:     "report body",
	})
	if len(m.curText) != 0 {
		t.Fatalf("streamed text not flushed: %q", m.curText)
	}
	msg := lastLine(t, m)
	prev := m.lines[len(m.lines)-2]
	if msg.kind != kindAgentMessage || !strings.Contains(msg.text, "report body") {
		t.Fatalf("last line = %+v, want agent message", msg)
	}
	if prev.kind != kindAssistant || prev.text != "partial assistant" {
		t.Fatalf("previous line = %+v, want flushed assistant text", prev)
	}
}

func TestAgentMessageStyleDistinct(t *testing.T) {
	// Force the 16-color ANSI profile so Render emits color codes regardless of
	// the test terminal, restoring the exact prior profile so later tests see
	// the same baseline they would without this test.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	agent := styleLine(kindAgentMessage, "msg")
	if !strings.Contains(agent, "35m") { // magenta
		t.Fatalf("agent-message style = %q, want magenta", agent)
	}
	if user := styleLine(kindUser, "msg"); agent == user {
		t.Fatalf("agent-message style identical to user style: %q", agent)
	}
	if assistant := styleLine(kindAssistant, "msg"); agent == assistant {
		t.Fatalf("agent-message style identical to assistant style: %q", agent)
	}
	if tool := styleLine(kindTool, "msg"); agent == tool {
		t.Fatalf("agent-message style identical to tool style: %q", agent)
	}
}
