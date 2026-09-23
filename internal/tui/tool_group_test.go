package tui

import (
	"strings"
	"testing"

	"aiharn/internal/llm"
)

func TestSingleToolCallRendersAsPlainToolLine(t *testing.T) {
	m := newTestModel(t)
	m.appendToolCall(llm.Item{CallID: "t1", Name: "read_file", Args: "file.txt"})

	visual, _ := m.transcriptRows(200, 50)
	text := strings.Join(visual, "\n")
	if !strings.Contains(text, "[tool] read_file") || !strings.Contains(text, "file.txt") {
		t.Fatalf("single tool call not rendered as plain tool line: %q", text)
	}
	if strings.Contains(text, "tool calls") {
		t.Fatalf("single tool call should not render a grouped count: %q", text)
	}
}

func TestConsecutiveToolCallsGroupAndCollapse(t *testing.T) {
	m := newTestModel(t)
	m.appendToolCall(llm.Item{CallID: "t1", Name: "read_file", Args: "arg-one"})
	m.appendToolCall(llm.Item{CallID: "t2", Name: "read_file", Args: "arg-two"})

	var groups []*line
	for i := range m.lines {
		if m.lines[i].kind == kindToolGroup {
			groups = append(groups, &m.lines[i])
		}
	}
	if len(groups) != 1 || len(groups[0].calls) != 2 {
		t.Fatalf("want exactly one tool group with two calls, got %d groups: %+v", len(groups), groups)
	}

	visual, _ := m.transcriptRows(200, 50)
	text := strings.Join(visual, "\n")
	if !strings.Contains(text, "[tool] tool calls (2)") {
		t.Fatalf("collapsed group header missing: %q", text)
	}
	if strings.Contains(text, "arg-one") || strings.Contains(text, "arg-two") {
		t.Fatalf("collapsed group should hide individual args: %q", text)
	}
}

func TestToggleToolGroupExpands(t *testing.T) {
	m := newTestModel(t)
	m.appendToolCall(llm.Item{CallID: "t1", Name: "read_file", Args: "arg-one"})
	m.appendToolCall(llm.Item{CallID: "t2", Name: "read_file", Args: "arg-two"})

	groupID := 0
	for i := range m.lines {
		if m.lines[i].kind == kindToolGroup {
			groupID = m.lines[i].groupID
			break
		}
	}
	if groupID == 0 {
		t.Fatalf("no tool group found in lines: %+v", m.lines)
	}

	m.toggleToolGroup(groupID)
	visual, _ := m.transcriptRows(200, 50)
	text := strings.Join(visual, "\n")
	if !strings.Contains(text, "  [tool] read_file arg-one") || !strings.Contains(text, "  [tool] read_file arg-two") {
		t.Fatalf("expanded group missing indented tool lines: %q", text)
	}
}

func TestInterveningLineBreaksToolGroup(t *testing.T) {
	m := newTestModel(t)
	m.appendToolCall(llm.Item{CallID: "t1", Name: "read_file", Args: "arg-one"})
	m.appendLine(kindPlain, "chat")
	m.appendToolCall(llm.Item{CallID: "t2", Name: "read_file", Args: "arg-two"})

	var groups []*line
	for i := range m.lines {
		if m.lines[i].kind == kindToolGroup {
			groups = append(groups, &m.lines[i])
		}
	}
	if len(groups) != 2 {
		t.Fatalf("want two tool groups after an intervening line, got %d: %+v", len(groups), m.lines)
	}
	for i, g := range groups {
		if len(g.calls) != 1 {
			t.Fatalf("group %d has %d calls, want 1: %+v", i, len(g.calls), g.calls)
		}
	}
}
