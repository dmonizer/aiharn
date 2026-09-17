package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
	testllm "aiharn/internal/testutil/llm"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	client := &testllm.FakeClient{}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})
	mgr := agent.NewManager(agent.ManagerOptions{Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{ID: spec.ID, Type: spec.Type, Client: &testllm.FakeClient{}}), nil
	}})
	if err := mgr.RegisterTop(a); err != nil {
		t.Fatal(err)
	}
	g := approval.NewGate(approval.ModeAsk)
	return New(mgr, a, g, Status{Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask"})
}

// upd runs m.Update and returns the concrete model and command.
func upd(t *testing.T, m *Model, msg tea.Msg) (*Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(msg)
	return nm.(*Model), cmd
}

// runCmd runs a returned tea.Cmd synchronously and returns its message, or nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestUpdateTextDelta(t *testing.T) {
	m := newTestModel(t)

	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "Hel"}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "lo"}})

	if string(m.curText) != "Hello" {
		t.Fatalf("curText = %q, want Hello", m.curText)
	}
}

func TestTimeoutNoteFollowsPartialBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta agent.EventType
		phase string
	}{
		{"thinking", agent.EventReasoningDelta, "thinking"},
		{"response", agent.EventText, "response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: tc.delta, Text: "partial"}})
			m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventTimeout, TimeoutPhase: tc.phase, Text: "time budget exceeded"}})
			m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventState, State: agent.StateIdle}})
			var found bool
			for i := 1; i < len(m.lines); i++ {
				if m.lines[i-1].text == "partial" && m.lines[i].text == "time budget exceeded" &&
					m.lines[i-1].kind == m.lines[i].kind {
					found = true
				}
			}
			if !found {
				t.Fatalf("timeout note not beside partial block: %+v", m.lines)
			}
		})
	}
}

func TestClosedCallerCloseSubagentIsWarning(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, subagentClosedMsg{id: "sub-1", err: agent.ErrCallerUnavailable})
	if len(m.lines) == 0 || m.lines[len(m.lines)-1].kind != kindPlain ||
		!strings.Contains(m.lines[len(m.lines)-1].text, "warning: subagent was not closed") {
		t.Fatalf("lines = %+v", m.lines)
	}
}

func TestUpdateToolCallFlushesText(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "run"}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: llm.Item{Name: "execute_command", Args: `{"command":"ls"}`}}})

	if len(m.curText) != 0 {
		t.Fatalf("curText = %q, want flushed", m.curText)
	}
	if len(m.lines) == 0 || m.lines[len(m.lines)-1].text != "ls" {
		t.Fatalf("last line = %q, want command preview", m.lines[len(m.lines)-1].text)
	}
	if len(m.shellCmds) != 1 || m.shellCmds[0].command != "ls" {
		t.Fatalf("shellCmds = %+v, want one ls command", m.shellCmds)
	}
}

func TestUpdateApprovalReq(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "rm -rf /"}
	m, cmd := upd(t, m, approvalReqMsg{req: req})

	if m.pending == nil || m.pending.Command != "rm -rf /" {
		t.Fatalf("pending = %+v", m.pending)
	}
	if cmd == nil {
		t.Fatal("expected re-subscription command")
	}
}

func TestInputQueuedAtTurnBoundary(t *testing.T) {
	m := newTestModel(t)

	// First input starts a turn immediately.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("expected running after first enter")
	}
	if cmd == nil {
		t.Fatal("expected runTurn command")
	}

	// Second input is queued while running.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})
	m, cmd = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("still running")
	}
	if cmd != nil {
		t.Fatal("no new turn should start while running")
	}
	if len(m.queue) != 1 || m.queue[0] != "two" {
		t.Fatalf("queue = %v", m.queue)
	}

	// Turn completion drains the queue.
	m, cmd = upd(t, m, turnDoneMsg{})
	if !m.running || cmd == nil {
		t.Fatalf("queued turn should start: running=%v cmd=%v", m.running, cmd != nil)
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue = %v", m.queue)
	}

	// Second completion leaves the agent idle.
	m, cmd = upd(t, m, turnDoneMsg{})
	if m.running || cmd != nil {
		t.Fatalf("expected idle: running=%v cmd=%v", m.running, cmd != nil)
	}
}

func TestEscStopsCurrentAndQueuedRequests(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first")})
	m, turnCmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 {
		t.Fatalf("queue before stop = %v", m.queue)
	}

	m, stopCmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if stopCmd != nil {
		t.Fatal("stop key unexpectedly returned a command")
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue after stop = %v", m.queue)
	}
	if !m.stopping {
		t.Fatal("top-level turn was not marked as stopping")
	}

	msg := runCmd(turnCmd)
	done, ok := msg.(turnDoneMsg)
	if !ok || !errors.Is(done.err, context.Canceled) {
		t.Fatalf("turn result = %v, want context cancellation", msg)
	}
	m, next := upd(t, m, done)
	if m.running || next != nil {
		t.Fatalf("stopped turn restarted work: running=%v cmd=%v", m.running, next != nil)
	}
	for _, line := range m.lines {
		if strings.Contains(line.text, "error: context canceled") {
			t.Fatalf("expected user cancellation to be non-error: %q", line.text)
		}
	}
}

func TestEscDismissesPendingApproval(t *testing.T) {
	m := newTestModel(t)
	done := make(chan error, 1)
	go func() {
		_, err := m.gate.Check(context.Background(), approval.Request{Command: "sleep 10"})
		done <- err
	}()

	var req approval.Request
	select {
	case req = <-m.gate.Pending():
	case <-time.After(time.Second):
		t.Fatal("approval was not registered")
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.pending != nil || len(m.approvals) != 0 {
		t.Fatalf("approval UI survived stop: pending=%+v queued=%+v", m.pending, m.approvals)
	}

	// A notification already in Bubble Tea's queue must not resurrect it.
	m, _ = upd(t, m, approvalReqMsg{req: req})
	if m.pending != nil {
		t.Fatalf("late canceled approval reappeared: %+v", m.pending)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("approval cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval check did not unblock")
	}
}

func TestKeyHandling(t *testing.T) {
	m := newTestModel(t)

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.textarea.Value(); got != "a" {
		t.Fatalf("backspace: value = %q, want a", got)
	}
	// A single ESC arms the clear but leaves the input untouched.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.textarea.Value(); got != "a" {
		t.Fatalf("single esc: value = %q, want unchanged", got)
	}
	// A second ESC within the window clears.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("double esc: value = %q, want cleared", got)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if got := m.textarea.Value(); got != " " {
		t.Fatalf("space: value = %q, want space", got)
	}
}

func TestPasteMultilineInput(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("one\ntwo\nthree")})
	if got := m.textarea.Value(); got != "one\ntwo\nthree" {
		t.Fatalf("value = %q", got)
	}
	// Submitting sends the whole block verbatim to the agent.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 0 {
		t.Fatalf("expected immediate turn, queue = %v", m.queue)
	}
	if !m.running {
		t.Fatal("expected running after submit")
	}
}

func TestCtrlJInsertsNewline(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlJ})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cd")})
	if got := m.textarea.Value(); got != "ab\ncd" {
		t.Fatalf("value = %q, want ab\\ncd", got)
	}
	// Enter still submits rather than inserting a newline.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("enter should submit, not insert a newline")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("value after submit = %q, want empty", got)
	}
}

func TestMultilinePasteStartingWithSlashIsNotACommand(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("/home/user\nls -la")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("multi-line input starting with / must reach the agent, not be treated as a command")
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue = %v", m.queue)
	}
}

func TestInputBoxHeightCappedAtConfiguredMax(t *testing.T) {
	m := newTestModel(t)
	m.height = 20
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "x"
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(strings.Join(lines, "\n"))})

	if got := m.maxInputHeight(); got != defaultInputHeight {
		t.Fatalf("maxInputHeight = %d, want %d", got, defaultInputHeight)
	}
	if got := m.inputBoxHeight(); got != defaultInputHeight {
		t.Fatalf("inputBoxHeight = %d, want %d", got, defaultInputHeight)
	}
}

func TestInputBoxResizesWithCtrlUpDown(t *testing.T) {
	m := newTestModel(t)
	m.height = 20

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlUp})
	if got := m.maxInputHeight(); got != defaultInputHeight+1 {
		t.Fatalf("maxInputHeight after ctrl+up = %d, want %d", got, defaultInputHeight+1)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlDown})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlDown})
	if got := m.maxInputHeight(); got != defaultInputHeight-1 {
		t.Fatalf("maxInputHeight after two ctrl+down = %d, want %d", got, defaultInputHeight-1)
	}
	// Shrinking floors at one line, no matter how many times it is pressed.
	for i := 0; i < 10; i++ {
		m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlDown})
	}
	if got := m.maxInputHeight(); got != minInputHeight {
		t.Fatalf("maxInputHeight after many ctrl+down = %d, want %d", got, minInputHeight)
	}
}

func TestInputAutoScrollsToBottom(t *testing.T) {
	m := newTestModel(t)
	m.width = 120
	m.height = 16 // defaultInputHeight = 4
	m.resizeInput()

	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%02d", i)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(strings.Join(lines, "\n"))})

	view := m.textarea.View()
	if !strings.Contains(view, "L19") {
		t.Fatalf("input box is not scrolled to the newest line; got:\n%s", view)
	}
	if strings.Contains(view, "L00") {
		t.Fatalf("input box should be scrolled to the bottom, not showing the first line; got:\n%s", view)
	}
}

func TestInputCappedAndScrollable(t *testing.T) {
	m := newTestModel(t)
	m.height = 16 // defaultInputHeight = 4
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(strings.Join(lines, "\n"))})

	// The box is capped at the configured max height but the value is intact.
	if got := m.inputBoxHeight(); got != defaultInputHeight {
		t.Fatalf("inputBoxHeight = %d, want %d", got, defaultInputHeight)
	}
	if got := m.textarea.Value(); got != strings.Join(lines, "\n") {
		t.Fatalf("value = %q", got)
	}
	// Up/down now navigate prompt history rather than move the cursor.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if got := m.textarea.Value(); got != strings.Join(lines, "\n") {
		t.Fatalf("value after up = %q, want the draft preserved", got)
	}
}

func TestInputScrollsWithPageKeysAndMouseWheel(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 16
	m.resizeInput()
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(strings.Join(lines, "\n"))})

	bottom := m.textarea.Line()
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	pageUp := m.textarea.Line()
	if pageUp >= bottom {
		t.Fatalf("line after pgup = %d, want before bottom %d", pageUp, bottom)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if got := m.textarea.Line(); got <= pageUp {
		t.Fatalf("line after pgdn = %d, want after %d", got, pageUp)
	}

	inputY := m.rows()
	m, _ = upd(t, m, tea.MouseMsg{Y: inputY, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	wheelUp := m.textarea.Line()
	if wheelUp >= bottom {
		t.Fatalf("line after wheel up = %d, want before bottom %d", wheelUp, bottom)
	}
	m, _ = upd(t, m, tea.MouseMsg{Y: inputY, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if got := m.textarea.Line(); got <= wheelUp {
		t.Fatalf("line after wheel down = %d, want after %d", got, wheelUp)
	}
}

func TestReasoningDisplayToggleAndElapsedStatus(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.beginThinking(started)
	m.appendReasoning("private summary")
	m.finishThinking(started.Add(65 * time.Second))

	hidden, _ := m.transcriptRows(80, 20)
	hiddenText := strings.Join(hidden, "\n")
	if strings.Contains(hiddenText, "private summary") {
		t.Fatalf("hidden reasoning was rendered: %q", hiddenText)
	}
	if !strings.Contains(hiddenText, "thinking ... (01:05)") {
		t.Fatalf("elapsed status missing from %q", hiddenText)
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyF10})
	if !m.showReasoning || !strings.Contains(m.statusLine(), "thinking shown") {
		t.Fatalf("F10 did not enable reasoning display: show=%v status=%q", m.showReasoning, m.statusLine())
	}
	shown, _ := m.transcriptRows(80, 20)
	shownText := strings.Join(shown, "\n")
	if !strings.Contains(shownText, "private summary") {
		t.Fatalf("shown reasoning missing from %q", shownText)
	}
	if strings.Contains(shownText, "thinking ...") {
		t.Fatalf("elapsed placeholder should be replaced by shown reasoning: %q", shownText)
	}
}

func TestReasoningStyleIsLighterThanCommandOutput(t *testing.T) {
	if got := styleReasoning.GetForeground(); got != lipgloss.Color("245") {
		t.Fatalf("reasoning color = %q, want 245", got)
	}
	if got := styleCommand.GetForeground(); got != lipgloss.Color("240") {
		t.Fatalf("command color = %q, want 240", got)
	}
}

func TestQuitOnCtrlC(t *testing.T) {
	m := newTestModel(t)
	_, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit command")
	}
}

func TestQuitCommand(t *testing.T) {
	m := New(nil, nil, nil, Status{})
	cmd := m.handleCommand("/quit")
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("cmd() = %T, want tea.QuitMsg", msg)
	}
	if m.ctx.Err() == nil {
		t.Fatal("expected context cancelled on quit")
	}

	// /exit is an alias.
	m = New(nil, nil, nil, Status{})
	if cmd := m.handleCommand("/exit"); cmd == nil {
		t.Fatal("expected /exit to quit too")
	}
}

func TestStyleLineColors(t *testing.T) {
	// Force the 16-color ANSI profile so Render emits color codes regardless of
	// the terminal the test runs under (CI and headless runs detect "no color").
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })

	cases := []struct {
		kind lineKind
		want string // ANSI SGR color code, "" means unstyled
	}{
		{kindPlain, ""},
		{kindAssistant, ""},
		{kindUser, "36"},
		{kindTool, "33"},
		{kindError, "31"},
	}
	for _, c := range cases {
		got := styleLine(c.kind, "hello")
		if c.want == "" {
			if strings.Contains(got, "\x1b[") {
				t.Fatalf("kind=%d got %q, want no ANSI escape", c.kind, got)
			}
			continue
		}
		if !strings.Contains(got, c.want+"m") {
			t.Fatalf("kind=%d got %q, want color %s", c.kind, got, c.want)
		}
	}
}

func TestApprovalKeys(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}
	m, _ = upd(t, m, approvalReqMsg{req: req})

	_, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.pending != nil {
		t.Fatal("expected approval cleared")
	}
	if cmd != nil {
		t.Fatal("approval bridge was already re-subscribed when the request arrived")
	}
}

func TestApprovalRequestsAreQueued(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", Command: "one"}})
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "2", Command: "two"}})
	if m.pending == nil || m.pending.ID != "1" || len(m.approvals) != 1 {
		t.Fatalf("pending=%+v queue=%+v", m.pending, m.approvals)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.pending == nil || m.pending.ID != "2" || len(m.approvals) != 0 {
		t.Fatalf("pending=%+v queue=%+v", m.pending, m.approvals)
	}
}

func TestTerminalControlSequencesAreRemoved(t *testing.T) {
	m := newTestModel(t)
	m.appendLine(kindPlain, "safe\x1b]52;c;clipboard\a text")
	got := m.lines[len(m.lines)-1].text
	if strings.ContainsAny(got, "\x1b\a") || got != "safe]52;c;clipboard text" {
		t.Fatalf("sanitized line = %q", got)
	}
}

func TestApprovalAcceptAndAllowAll(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}
	m, _ = upd(t, m, approvalReqMsg{req: req})

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.gate.Mode() != approval.ModeAllowAll {
		t.Fatalf("gate mode = %v", m.gate.Mode())
	}
	if m.pending != nil {
		t.Fatal("expected approval cleared")
	}
	if !strings.Contains(m.statusLine(), "actions allowed all") {
		t.Fatalf("status line = %q", m.statusLine())
	}
}

func TestF9TogglesActionsAllowed(t *testing.T) {
	m := newTestModel(t)
	if !strings.Contains(m.statusLine(), "actions allowed ask") {
		t.Fatalf("initial status = %q", m.statusLine())
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyF9})
	if m.gate.Mode() != approval.ModeAllowAll || !strings.Contains(m.statusLine(), "actions allowed all") {
		t.Fatalf("after first F9: mode=%v status=%q", m.gate.Mode(), m.statusLine())
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyF9})
	if m.gate.Mode() != approval.ModeAsk || !strings.Contains(m.statusLine(), "actions allowed ask") {
		t.Fatalf("after second F9: mode=%v status=%q", m.gate.Mode(), m.statusLine())
	}
}

func TestF9LeavesExistingApprovalPending(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}
	m, _ = upd(t, m, approvalReqMsg{req: req})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyF9})
	if m.gate.Mode() != approval.ModeAllowAll || m.pending == nil || m.pending.ID != req.ID {
		t.Fatalf("mode=%v pending=%+v", m.gate.Mode(), m.pending)
	}
}

func TestStatusBarItemsFitWidth(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	line := m.statusLine()
	if !strings.Contains(line, "actions allowed ask │ thinking hidden") || uniseg.StringWidth(line) > m.width {
		t.Fatalf("status line = %q", line)
	}
	m.width = 10
	line = m.statusLine()
	if uniseg.StringWidth(line) > m.width || line == "" {
		t.Fatalf("narrow status line = %q", line)
	}
}

func TestWaitAgentEventDelivers(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{
			{Type: llm.EventTextDelta, Text: "x"},
			{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "x"}}},
		},
	}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})

	go func() { _ = a.Turn(context.Background(), "hi") }()

	// The bridge delivers whatever event the agent emits; the first is the
	// running state transition, not a text delta.
	msg := runCmd(waitAgentEventContext(context.Background(), a))
	if _, ok := msg.(agentEventMsg); !ok {
		t.Fatalf("msg = %+v, want agentEventMsg", msg)
	}
}

func TestInputPinnedToBottom(t *testing.T) {
	m := newTestModel(t)
	m.width = 120
	m.height = 10
	m.resizeInput()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != 10 {
		t.Fatalf("view has %d lines, want 10", len(lines))
	}
	if !strings.Contains(lines[len(lines)-2], "> ") || !strings.Contains(lines[len(lines)-1], "actions allowed ask") {
		t.Fatalf("bottom rows = %q, %q; want input then status", lines[len(lines)-2], lines[len(lines)-1])
	}
}

func TestRunTurnReportsCompletion(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "ok"}}}},
	}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})

	msg := runCmd(runTurn(a, context.Background(), "hi"))
	if done, ok := msg.(turnDoneMsg); !ok || done.err != nil {
		t.Fatalf("msg = %+v", msg)
	}
}
