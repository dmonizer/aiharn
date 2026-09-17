package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

// stripANSI drops CSI escapes so widths can be computed as a terminal would.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// terminalWidth is the width a real terminal gives a row: a tab advances the
// cursor to the next multiple of 8 columns.
func terminalWidth(row string) int {
	col := 0
	for _, r := range stripANSI(row) {
		if r == '\t' {
			col += 8 - col%8
			continue
		}
		col += uniseg.StringWidth(string(r))
	}
	return col
}

func TestZZProbeTabWidth(t *testing.T) {
	const line = "row\twith\ttabs\tand\tsome\tmore\twords\there\tto\tfill\tthe\tline\tout\tto\tthe\tedge\tand\tbeyond"
	fmt.Printf("PROBE uniseg.StringWidth(tab)=%d lipgloss.Width(a\\tb)=%d\n", uniseg.StringWidth("\t"), lipgloss.Width("a\tb"))
	fmt.Printf("PROBE source line width: lipgloss=%d terminal=%d\n", lipgloss.Width(line), terminalWidth(line))

	m := newTestModel(t)
	m.width, m.height = 80, 24
	m.resizeInput()
	m.appendLine(kindAssistant, line)
	rows := strings.Split(m.View(), "\n")
	fmt.Printf("PROBE view rows=%d (height=%d)\n", len(rows), m.height)
	over, worst, worstRow := 0, 0, -1
	for i, r := range rows {
		tw := terminalWidth(r)
		if tw > m.width {
			over++
		}
		if tw > worst {
			worst, worstRow = tw, i
		}
	}
	fmt.Printf("PROBE rows whose TERMINAL width exceeds %d: %d (worst %d at row %d)\n", m.width, over, worst, worstRow)
	for i, r := range rows {
		if tw := terminalWidth(r); tw > m.width {
			fmt.Printf("PROBE OVER row %d: app-width(lipgloss)=%d terminal-width=%d content=%q\n", i, lipgloss.Width(r), tw, r)
		}
	}
	for i, r := range rows {
		if strings.Contains(r, "\t") {
			fmt.Printf("PROBE tab row %2d lipgloss=%d terminal=%d %q\n", i, lipgloss.Width(r), terminalWidth(r), r)
		}
	}
	// plain prose control
	m2 := newTestModel(t)
	m2.width, m2.height = 80, 24
	m2.resizeInput()
	m2.appendLine(kindAssistant, strings.Repeat("plain prose without tabs ", 8))
	over2 := 0
	for _, r := range strings.Split(m2.View(), "\n") {
		if terminalWidth(r) > m2.width {
			over2++
		}
	}
	fmt.Printf("PROBE control (no tabs) rows over width: %d\n", over2)
}

func TestZZProbeNarrowRoster(t *testing.T) {
	m := newRosterModel(t)
	if _, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "", "task with a rather long name here"); err != nil {
		t.Fatal(err)
	}
	m.refreshSubagents()
	m.width, m.height = 20, 24
	m.resizeInput()
	rows := strings.Split(m.View(), string(rune(10)))
	worst, worstRow, over := 0, "", 0
	for i, r := range rows {
		if w := lipgloss.Width(r); w > worst {
			worst, worstRow = w, r
		}
		if lipgloss.Width(r) > m.width || terminalWidth(r) > m.width {
			over++
			fmt.Printf("PROBE OVER narrow row %d app=%d terminal=%d %q\n", i, lipgloss.Width(r), terminalWidth(r), r)
		}
	}
	fmt.Printf("PROBE narrow 20x24: rows=%d worst=%d over=%d worstrow=%q\n", len(rows), worst, over, worstRow)
}

// TestZZProbeMatrix sweeps widths x heights x states and reports every state
// where View() is not exactly m.height rows or some row exceeds m.width.
func TestZZProbeMatrix(t *testing.T) {
	corpus := []string{
		"Paragraph one has ordinary prose that wraps naturally across the pane width without any special characters at all.\n\nParagraph two is the same but longer, so that several visual rows are produced and the bottom-pinned transcript has to drop rows.\n\nThird paragraph ends here.",
		"Code block:\n\tif err != nil {\n\t\treturn err\n\t}\n\t\treturn nil",
		"longtoken:" + strings.Repeat("abcdefghij", 30),
		"url: https://example.com/" + strings.Repeat("verylongpathsegment/", 20) + "?q=" + strings.Repeat("x", 100),
		"emoji 👨‍👩‍👧‍👦 family and flag 🇺🇸 and a sword ⚔️ ok",
		"CJK: 日本語のテキストはここにあります。これは長い行です。",
		"combining: e\u0301 a\u0300 o\u0302 plus 漢字 mixed with 👨‍👩‍👧‍👦 and \ttabs\tinside\ttext",
	}
	widths := []int{20, 40, 60, 72, 80, 120, 200}
	heights := []int{8, 14, 24, 30}
	states := []string{"plain", "streaming", "tool", "approval", "popover", "shellOpen", "shellMax", "roster"}
	bad, checked := 0, 0
	for _, w := range widths {
		for _, h := range heights {
			for _, state := range states {
				m := newTestModel(t)
				if state == "roster" {
					m = newRosterModel(t)
					if _, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "", "describe the whole rendering pipeline in detail and keep going for a while"); err != nil {
						t.Fatal(err)
					}
					m.refreshSubagents()
				}
				m.width, m.height = w, h
				m.resizeInput()
				for _, c := range corpus {
					m.appendLine(kindAssistant, c)
				}
				switch state {
				case "streaming":
					m.appendStreamText(kindAssistant, "streaming "+corpus[0]+"\tstill\tstreaming")
				case "tool":
					m.appendToolCall(llm.Item{CallID: "c1", Name: "execute_command", Args: `{"command":"printf 'a\tb\nc\td\n'"}`})
					m.appendToolResult(llm.Item{CallID: "c1", Name: "execute_command"}, "a\tb\n\tindented\toutput\n"+strings.Repeat("z", 200))
					m.shellMode = shellOpen
				case "approval":
					m.pending = &approval.Request{ID: "1", Command: "run\tthis\twith\ttabs and " + strings.Repeat("y", 90)}
				case "popover":
					m.pending = &approval.Request{ID: "1", Command: "run\tthis\twith\ttabs and " + strings.Repeat("y", 90)}
					m.approvalPopover = true
				case "shellOpen":
					m.appendToolCall(llm.Item{CallID: "c2", Name: "execute_command", Args: `{"command":"ls"}`})
					m.appendToolResult(llm.Item{CallID: "c2", Name: "execute_command"}, "\tcol1\tcol2\n"+strings.Repeat("wide-line-of-output ", 20))
					m.shellMode = shellOpen
				case "shellMax":
					m.appendToolCall(llm.Item{CallID: "c3", Name: "execute_command", Args: `{"command":"ls"}`})
					m.appendToolResult(llm.Item{CallID: "c3", Name: "execute_command"}, "\tcol1\tcol2\n"+strings.Repeat("wide-line-of-output ", 20))
					m.shellMode = shellMaximized
				}
				rows := strings.Split(m.View(), string(rune(10)))
				checked++
				fail := ""
				if len(rows) != m.height {
					fail = fmt.Sprintf("rows=%d want %d", len(rows), m.height)
				}
				for i, r := range rows {
					if lw, tw := lipgloss.Width(r), terminalWidth(r); lw > m.width || tw > m.width {
						fail = fmt.Sprintf("row %d lipgloss=%d terminal=%d > %d: %q", i, lw, tw, m.width, r)
						break
					}
				}
				if fail != "" {
					bad++
					if bad <= 25 {
						fmt.Printf("PROBE FAIL w=%-3d h=%-2d %-10s %s\n", w, h, state, fail)
					}
				}
			}
		}
	}
	fmt.Printf("PROBE MATRIX: %d/%d states clean, %d violations\n", checked-bad, checked, bad)
}
