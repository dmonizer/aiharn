package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rivo/uniseg"

	"aiharn/internal/approval"
)

func TestApprovalCommandPreviewAndClickablePopover(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 120, 18
	m.resizeInput()
	command := strings.Repeat("x", 75) + " UNIQUE_END"
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", Command: command}})
	view := m.View()
	if strings.Contains(view, "UNIQUE_END") || m.approvalLinkHit.width != 3 {
		t.Fatalf("full command appeared before opening preview, or link missing: hit=%+v", m.approvalLinkHit)
	}
	if got := strings.Split(view, "\n")[m.height-3]; !strings.Contains(got, strings.Repeat("x", 67)) || !strings.Contains(got, "...") {
		t.Fatalf("approval prompt was not shortened: %q", got)
	}
	hit := m.approvalLinkHit
	m, _ = upd(t, m, tea.MouseMsg{X: hit.x + 1, Y: hit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.approvalPopover {
		t.Fatal("clicking approval ellipsis did not open the full command")
	}
	view = m.View()
	if !strings.Contains(view, "UNIQUE_END") || m.pending == nil {
		t.Fatalf("popover did not show full command or changed approval state: %q", view)
	}
	closeHit := m.approvalCloseHit
	m, _ = upd(t, m, tea.MouseMsg{X: closeHit.x + 1, Y: closeHit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.approvalPopover || m.pending == nil {
		t.Fatal("close button did not dismiss only the popover")
	}
}

func TestApprovalPopoverScrollAndEsc(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 45, 12
	m.resizeInput()
	command := strings.Repeat("0123456789", 40) + "TAIL"
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", Command: command}})
	m.View()
	hit := m.approvalLinkHit
	m, _ = upd(t, m, tea.MouseMsg{X: hit.x, Y: hit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.approvalMaxScroll() == 0 {
		t.Fatal("long full command should scroll")
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(m.View(), "TAIL") {
		t.Fatal("End did not reveal the end of the full command")
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.approvalPopover || m.pending == nil {
		t.Fatal("Esc should close only the popover")
	}
}

func TestApprovalPreviewLimitsAndUnicode(t *testing.T) {
	for _, command := range []string{strings.Repeat("x", 70), strings.Repeat("x", 71), strings.Repeat("界", 40)} {
		short, truncated := previewCommand(command, commandPreviewLen)
		if truncated {
			short += "..."
		}
		if uniseg.StringWidth(short) > commandPreviewLen {
			t.Fatalf("preview width %d exceeds %d: %q", uniseg.StringWidth(short), commandPreviewLen, short)
		}
	}
}
