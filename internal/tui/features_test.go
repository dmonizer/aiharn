package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

func TestApprovalNotInTranscript(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}})

	if m.pending == nil {
		t.Fatal("expected pending approval")
	}
	for _, ln := range m.lines {
		if strings.Contains(ln.text, "[approval]") {
			t.Fatalf("approval leaked into transcript: %+v", m.lines)
		}
	}
}

func TestShellCommandAccumulation(t *testing.T) {
	m := newTestModel(t)
	call := llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"ls -la"}`}
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: call}})

	if len(m.shellCmds) != 1 || m.shellCmds[0].command != "ls -la" || m.shellCmds[0].done {
		t.Fatalf("shellCmds = %+v, want one running ls command", m.shellCmds)
	}
	if len(m.lines) == 0 || m.lines[len(m.lines)-1].cmd != 0 || m.lines[len(m.lines)-1].text != "ls -la" {
		t.Fatalf("last transcript line = %+v, want clickable command preview", m.lines[len(m.lines)-1])
	}

	result := "exit_code: 0\nstdout:\nfoo"
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolResult, Call: call, Text: result}})
	if !m.shellCmds[0].done || m.shellCmds[0].output != result {
		t.Fatalf("shellCmds[0] = %+v, want done with output", m.shellCmds[0])
	}
	// header + 3 output lines
	if len(m.shellFlat) != 4 || m.shellFlat[0] != "$ ls -la" {
		t.Fatalf("shellFlat = %+v", m.shellFlat)
	}
}

func TestShellOutputFollowsNewestContent(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 14
	m.resizeInput()
	m.shellMode = shellOpen

	call := llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"seq 20"}`}
	m.appendToolCall(call)
	m.appendToolResult(call, strings.Join([]string{
		"line 01", "line 02", "line 03", "line 04", "line 05",
		"line 06", "line 07", "line 08", "line 09", "line 10",
		"line 11", "line 12", "line 13", "line 14", "line 15",
		"line 16", "line 17", "line 18", "line 19", "line 20",
	}, "\n"))

	if m.shellScroll != m.shellMaxScroll() || m.shellScroll == 0 {
		t.Fatalf("shellScroll = %d, max = %d; want output pinned to non-zero bottom", m.shellScroll, m.shellMaxScroll())
	}

	// Shrinking a followed pane increases its maximum offset. It must move to
	// that new maximum instead of retaining the now-stale pre-resize offset.
	m, _ = upd(t, m, tea.WindowSizeMsg{Width: 80, Height: 10})
	if m.shellScroll != m.shellMaxScroll() {
		t.Fatalf("after resize shellScroll = %d, max = %d", m.shellScroll, m.shellMaxScroll())
	}
}

func TestShellOutputDoesNotFollowAfterManualScroll(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 12
	m.resizeInput()
	m.shellMode = shellOpen

	first := llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"first"}`}
	m.appendToolCall(first)
	m.appendToolResult(first, strings.Repeat("output\n", 20))
	m.setShellScroll(0)
	if m.shellFollow {
		t.Fatal("manual scroll away from bottom should disable following")
	}

	second := llm.Item{CallID: "c2", Name: "execute_command", Args: `{"command":"second"}`}
	m.appendToolCall(second)
	if m.shellScroll != 0 {
		t.Fatalf("shellScroll = %d, want manual position preserved", m.shellScroll)
	}
}

func TestClickNewestShellCommandFollowsItsOutput(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 12
	m.resizeInput()
	m.shellMode = shellOpen

	call := llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"long-running"}`}
	m.appendToolCall(call)
	m.openShellFocus(0)
	if !m.shellFollow {
		t.Fatal("newest command focus should follow live output")
	}
	m.appendToolResult(call, strings.Repeat("output\n", 20))
	if m.shellScroll != m.shellMaxScroll() {
		t.Fatalf("shellScroll = %d, max = %d", m.shellScroll, m.shellMaxScroll())
	}
}

func TestShellPaneRendersScrollbarForOverflow(t *testing.T) {
	m := newTestModel(t)
	m.width = 50
	m.height = 12
	m.resizeInput()
	m.shellMode = shellOpen
	call := llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"seq 30"}`}
	m.appendToolCall(call)
	m.appendToolResult(call, strings.Repeat("output\n", 30))

	view := m.shellPane(m.shellOpenRows(), 1)
	if !strings.Contains(view, "█") {
		t.Fatalf("overflowing shell pane has no scrollbar thumb:\n%s", view)
	}
	if !strings.Contains(view, "│") {
		t.Fatalf("overflowing shell pane has no scrollbar track:\n%s", view)
	}
	start, size := m.shellScrollbar(m.shellContentRows())
	if size < 1 || start+size != m.shellContentRows() {
		t.Fatalf("bottom-pinned thumb = start %d size %d, content rows %d", start, size, m.shellContentRows())
	}
}

func TestShellCommandTruncatesPreview(t *testing.T) {
	m := newTestModel(t)
	long := strings.Repeat("x", 200)
	call := llm.Item{CallID: "c1", Name: "execute_command", Args: fmt.Sprintf(`{"command":%q}`, long)}
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: call}})

	if m.shellCmds[0].command != long {
		t.Fatalf("full command should be preserved in the shell view")
	}
	got := m.lines[len(m.lines)-1].text
	if !strings.HasPrefix(got, strings.Repeat("x", commandPreviewLen)) {
		t.Fatalf("preview should keep the first %d chars, got %q", commandPreviewLen, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("preview should end with an ellipsis, got %q", got)
	}
}

func TestPromptHistoryUpDown(t *testing.T) {
	m := newTestModel(t)
	// Submit two prompts.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Half-finished prompt, then up saves it as draft and shows the newest.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("half")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.textarea.Value() != "second" || m.draft != "half" {
		t.Fatalf("value=%q draft=%q, want second/half", m.textarea.Value(), m.draft)
	}
	// Up again to oldest.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.textarea.Value() != "first" {
		t.Fatalf("value=%q, want first", m.textarea.Value())
	}
	// Down back to newest, then down again restores the draft.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.textarea.Value() != "second" {
		t.Fatalf("value=%q, want second", m.textarea.Value())
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.textarea.Value() != "half" {
		t.Fatalf("value=%q, want restored draft", m.textarea.Value())
	}
}

func TestPromptHistoryDownDiscardsDraftOnSubmit(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("unsent")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.textarea.Value() != "" || m.draft != "unsent" {
		t.Fatalf("value=%q draft=%q, want empty/unsent", m.textarea.Value(), m.draft)
	}
	// Typing a new prompt and submitting discards the half-finished draft.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.draft != "" {
		t.Fatalf("draft=%q, want cleared after submit", m.draft)
	}
	if len(m.history) != 1 || m.history[0] != "new" {
		t.Fatalf("history=%v, want [new]", m.history)
	}
}

func TestTranscriptAutoscrollsToNewest(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 10
	m.resizeInput()
	for i := 0; i < 100; i++ {
		m.appendLine(kindPlain, fmt.Sprintf("line %d", i))
	}

	rows := m.rows()
	visual, _ := m.transcriptRows(m.width-32-1, rows)
	if len(visual) != rows {
		t.Fatalf("visual rows = %d, want %d", len(visual), rows)
	}
	if !strings.Contains(visual[len(visual)-1], "line 99") {
		t.Fatalf("newest line not at bottom: %q", visual[len(visual)-1])
	}
	for _, v := range visual {
		if strings.Contains(v, "line 0") && !strings.Contains(v, "line 90") {
			t.Fatalf("oldest line visible, expected pinned to bottom: %v", visual)
		}
	}
}

func TestShellScrollbarClickAndDrag(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 80, 24
	m.resizeInput()
	m.shellMode = shellMaximized
	call := llm.Item{CallID: "long", Name: "execute_command", Args: `{"command":"seq 100"}`}
	m.appendToolCall(call)
	m.appendToolResult(call, strings.Repeat("row\n", 100))
	m.View()
	if m.shellMaxScroll() == 0 {
		t.Fatal("test setup did not overflow pane")
	}
	x := m.width - 2
	first := 3 // maximized shell: top border y=1, header y=2
	m, _ = upd(t, m, tea.MouseMsg{X: x, Y: first, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.shellScroll != 0 {
		t.Fatalf("track top scroll=%d, want 0", m.shellScroll)
	}
	last := first + m.shellContentRows() - 1
	m, _ = upd(t, m, tea.MouseMsg{X: x, Y: last, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if m.shellScroll != m.shellMaxScroll() || !m.shellFollow {
		t.Fatalf("track bottom scroll=%d max=%d follow=%v", m.shellScroll, m.shellMaxScroll(), m.shellFollow)
	}
}

func TestClickOpensShellFocused(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"cmd1"}`}}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: llm.Item{CallID: "c2", Name: "execute_command", Args: `{"command":"cmd2"}`}}})

	m.clickRows = []int{0, 1}
	m.clickWidth = 80
	m.clickTranscript(5, 2) // body row 1 → command 1

	if m.shellMode != shellOpen {
		t.Fatalf("shellMode = %v, want open", m.shellMode)
	}
	if m.shellFocus != 1 {
		t.Fatalf("shellFocus = %d, want 1", m.shellFocus)
	}
}
