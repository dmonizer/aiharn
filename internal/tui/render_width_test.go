package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

// rwWidths and rwHeights are the terminal sizes every table-driven render-width
// test sweeps. No width may be silently dropped: the narrow panes hit the
// truncation paths that wide panes never reach.
var (
	rwWidths  = []int{20, 40, 60, 72, 80, 120, 200}
	rwHeights = []int{8, 14, 24, 30}
)

// rwPlainColors forces lipgloss to emit unstyled text so every row's measured
// width equals its display width. uniseg.StringWidth does not strip ANSI escape
// sequences, so a styled row would be miscounted; the previous profile is
// restored when the test ends.
func rwPlainColors(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// rwAssertRowsWithin locks the core invariant: View() returns exactly m.height
// rows and no row exceeds m.width by either the lipgloss or uniseg measure. It
// also rejects a raw tab byte, which would render wider than it measures.
func rwAssertRowsWithin(t *testing.T, m *Model, width, height int) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("View rows = %d, want %d (width=%d height=%d)", len(lines), m.height, width, height)
	}
	for i, row := range lines {
		if strings.ContainsRune(row, '\t') {
			t.Fatalf("raw tab in row: width=%d height=%d row=%d content=%q", width, height, i, row)
		}
		if lw := lipgloss.Width(row); lw > m.width {
			t.Fatalf("row overflows (lipgloss): width=%d height=%d row=%d measured=%d content=%q", width, height, i, lw, row)
		}
		if uw := uniseg.StringWidth(row); uw > m.width {
			t.Fatalf("row overflows (uniseg): width=%d height=%d row=%d measured=%d content=%q", width, height, i, uw, row)
		}
	}
}

// rwCase is one corpus entry fed through the real ingestion paths.
type rwCase struct {
	name string
	text string
}

// rwCorpus returns the text corpus that stresses wrapping, truncation and width
// measurement: prose, tab-indented code, oversized tokens, URLs, emoji (ZWJ and
// regional indicators), CJK and combining marks.
func rwCorpus() []rwCase {
	token := strings.Repeat("x", 300)
	url := "https://example.com/very/long/path/" + strings.Repeat("segment/", 20) + "?q=" + strings.Repeat("z", 60)
	mix := "Prose 👨‍👩‍👧‍👦 漢字 e\u0301.\n\tcode with tab 🇺🇸 and " + token + "\n" + url + "\ncombining a\u0301 tail"
	return []rwCase{
		{"prose", "First paragraph about a topic that keeps going and going until it wraps.\n\nSecond paragraph with more words so the wrap path runs again.\n\nThird and final paragraph."},
		{"tab-1", "\tfmt.Println(\"one tab\")"},
		{"tab-2", "\t\tfmt.Println(\"two tabs\")"},
		{"tab-4", "\t\t\t\tfmt.Println(\"four tabs\")"},
		{"tabs-mixed-prose", "Intro before the block.\n\tif ok {\n\t\tdo()\n\t}\nOutro after the block."},
		{"long-token", token},
		{"long-url", url},
		{"emoji", "family 👨‍👩‍👧‍👦 flag 🇺🇸 party 🎉 rockets 🚀🚀 and plain text"},
		{"cjk", strings.Repeat("漢字テストの文章", 12)},
		{"combining", "cafe\u0301 nai\u0308ve " + strings.Repeat("e\u0301", 40)},
		{"mix", mix},
	}
}

// TestRWTranscriptCorpusWithinWidth feeds every corpus item through the static
// ingestion paths at every size and re-checks the invariant after each append.
func TestRWTranscriptCorpusWithinWidth(t *testing.T) {
	rwPlainColors(t)
	for _, width := range rwWidths {
		for _, height := range rwHeights {
			m := newTestModel(t)
			m.width, m.height = width, height
			m.resizeInput()
			rwAssertRowsWithin(t, m, width, height)
			for _, tc := range rwCorpus() {
				m.appendLine(kindAssistant, tc.text)
				rwAssertRowsWithin(t, m, width, height)
				m.appendLine(kindAgentMessage, tc.text)
				rwAssertRowsWithin(t, m, width, height)
				m.appendLine(kindTool, tc.text)
				rwAssertRowsWithin(t, m, width, height)
				m.appendLine(kindPlain, tc.text)
				rwAssertRowsWithin(t, m, width, height)
			}
		}
	}
}

// TestRWStreamedDeltasWithinWidth feeds the assistant stream one rune at a time
// and checks every intermediate frame, including while curText is non-empty.
func TestRWStreamedDeltasWithinWidth(t *testing.T) {
	rwPlainColors(t)
	text := "\tstreamed 👨‍👩‍👧‍👦 漢字 " + strings.Repeat("q", 60) + " e\u0301 tail"
	for _, width := range rwWidths {
		for _, height := range rwHeights {
			m := newTestModel(t)
			m.width, m.height = width, height
			m.resizeInput()
			for _, r := range text {
				m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: string(r)}})
				if len(m.curText) > 0 {
					rwAssertRowsWithin(t, m, width, height)
				}
			}
			rwAssertRowsWithin(t, m, width, height)
		}
	}
}

// TestRWShellPaneWithinWidth checks the open and maximized shell panes with
// tab-containing, multi-line command output fed through rebuildShellFlat.
func TestRWShellPaneWithinWidth(t *testing.T) {
	rwPlainColors(t)
	output := "stdout:\n\tline one\n\t\tline two 👨‍👩‍👧‍👦\n" + strings.Repeat("wide ", 40) + "\n" + strings.Repeat("z", 200)
	for _, mode := range []shellViewMode{shellOpen, shellMaximized} {
		for _, width := range rwWidths {
			for _, height := range rwHeights {
				m := newTestModel(t)
				m.width, m.height = width, height
				m.resizeInput()
				call := llm.Item{CallID: "rw-c1", Name: "execute_command", Args: `{"command":"echo hi"}`}
				m.appendToolCall(call)
				m.appendToolResult(call, output)
				if len(m.shellCmds) == 0 {
					t.Fatalf("shell command not recorded (width=%d height=%d)", width, height)
				}
				m.shellCmds[0].output = output + "\n\tmore\t" + strings.Repeat("y", 120)
				m.rebuildShellFlat()
				m.shellMode = mode
				m.followShell()
				rwAssertRowsWithin(t, m, width, height)
			}
		}
	}
}

// TestRWApprovalWithinWidth checks both the inline approval prompt and the full
// command popover, whose commands may contain tabs and newlines.
func TestRWApprovalWithinWidth(t *testing.T) {
	rwPlainColors(t)
	command := "rm -rf /tmp/" + strings.Repeat("x", 200) + "\n\tyou should not\tsee tabs"
	for _, popover := range []bool{false, true} {
		for _, width := range rwWidths {
			for _, height := range rwHeights {
				m := newTestModel(t)
				m.width, m.height = width, height
				m.resizeInput()
				m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "rw-a1", ToolName: "execute_command", Command: command}})
				if m.pending == nil {
					t.Fatalf("approval not pending (width=%d height=%d)", width, height)
				}
				m.approvalPopover = popover
				rwAssertRowsWithin(t, m, width, height)
			}
		}
	}
}

// TestRWToolOutputWithinWidth feeds tool output through both the plain tool
// line and the execute_command path (whose result lands in the shell pane).
func TestRWToolOutputWithinWidth(t *testing.T) {
	rwPlainColors(t)
	text := "tool says:\n\tindented 👨‍👩‍👧‍👦 " + strings.Repeat("t", 150)
	for _, width := range rwWidths {
		for _, height := range rwHeights {
			m := newTestModel(t)
			m.width, m.height = width, height
			m.resizeInput()
			m.appendToolCall(llm.Item{CallID: "rw-t1", Name: "read_file", Args: text})
			rwAssertRowsWithin(t, m, width, height)
			call := llm.Item{CallID: "rw-t2", Name: "execute_command", Args: `{"command":"cat file"}`}
			m.appendToolCall(call)
			m.appendToolResult(call, text)
			rwAssertRowsWithin(t, m, width, height)
		}
	}
}

// TestRWRosterWithinWidth renders the subagent roster at every size with a
// deliberately long subagent id so the roster row truncation is exercised.
func TestRWRosterWithinWidth(t *testing.T) {
	rwPlainColors(t)
	longType := strings.Repeat("t", 58) // the generated id is "<type>-<seq>", about 60 columns
	for _, width := range rwWidths {
		for _, height := range rwHeights {
			m := newRosterModel(t)
			id, err := m.manager.SpawnSubagent(context.Background(), "main", longType, "", "task")
			if err != nil {
				t.Fatalf("spawn subagent: %v (width=%d height=%d)", err, width, height)
			}
			if len(id) < 55 {
				t.Fatalf("subagent id %q too short to exercise roster truncation", id)
			}
			m.refreshSubagents()
			m.width, m.height = width, height
			m.resizeInput()
			rwAssertRowsWithin(t, m, width, height)
		}
	}
}

// TestRWRosterLongIDWithinWidth asserts the long roster row is actually
// truncated with an ellipsis instead of overflowing or losing the id entirely.
func TestRWRosterLongIDWithinWidth(t *testing.T) {
	rwPlainColors(t)
	m := newRosterModel(t)
	longType := strings.Repeat("t", 58)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", longType, "", "task")
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}
	m.refreshSubagents()
	m.width, m.height = 40, 24
	m.resizeInput()
	rwAssertRowsWithin(t, m, 40, 24)
	content := m.renderRoster(4, 1, 18, 1)
	if !strings.Contains(content, "…") {
		t.Fatalf("long subagent row not truncated: %q", content)
	}
	if strings.Contains(content, id) {
		t.Fatalf("long subagent id rendered untruncated: %q", content)
	}
}

// TestRWTabIndentationPreserved checks that a tab-indented line keeps its
// indentation as spaces in the rendered row and that no row of View() carries a
// raw tab byte that would render wider than it measures. It sweeps every width.
func TestRWTabIndentationPreserved(t *testing.T) {
	rwPlainColors(t)
	const marker = "INDENTMARK"
	const height = 24
	for _, width := range rwWidths {
		t.Run(fmt.Sprintf("w%d", width), func(t *testing.T) {
			m := newTestModel(t)
			m.width, m.height = width, height
			m.resizeInput()
			m.appendLine(kindAssistant, "\t\t"+marker)
			lines := strings.Split(m.View(), "\n")
			found := false
			for i, row := range lines {
				if strings.ContainsRune(row, '\t') {
					t.Fatalf("width=%d height=%d row %d contains a raw tab: %q", width, height, i, row)
				}
				at := strings.Index(row, marker)
				if at < 0 {
					continue
				}
				found = true
				if !strings.Contains(row[:at], "  ") {
					t.Fatalf("width=%d height=%d tab indentation stripped from row %d: %q", width, height, i, row)
				}
			}
			if !found {
				t.Fatalf("width=%d height=%d marker row missing from view: %q", width, height, m.View())
			}
		})
	}
	wrapped := wrapLine("\t\t"+marker, 40)
	if len(wrapped) == 0 || !strings.HasPrefix(wrapped[0], "        ") {
		t.Fatalf("wrapLine stripped tab indentation: %q", wrapped)
	}
}
