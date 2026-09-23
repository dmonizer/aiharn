package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
)

func TestTranscriptScrollsWithMouseWheelAndRejoinsBottom(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 80, 14
	m.resizeInput()
	for i := 0; i < 60; i++ {
		m.appendLine(kindPlain, fmt.Sprintf("line %02d", i))
	}
	m.View() // compute the render-derived chatMaxScroll
	if m.chatMaxScroll == 0 {
		t.Fatal("test setup did not overflow the transcript")
	}

	m, _ = upd(t, m, tea.MouseMsg{Y: 0, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.chatFollow {
		t.Fatal("scrolling up should leave follow mode")
	}
	if m.chatScroll >= m.chatMaxScroll {
		t.Fatalf("chatScroll = %d, want below max %d", m.chatScroll, m.chatMaxScroll)
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	if m.chatScroll != m.chatMaxScroll || !m.chatFollow {
		t.Fatalf("End did not rejoin the bottom: scroll=%d max=%d follow=%v", m.chatScroll, m.chatMaxScroll, m.chatFollow)
	}
}

func TestApprovalResolvedClearsPrompt(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", Command: "ls"}})
	if m.pending == nil || m.pending.ID != "1" {
		t.Fatalf("pending = %+v, want id 1", m.pending)
	}
	m, _ = upd(t, m, approvalResolvedMsg{id: "1"})
	if m.pending != nil {
		t.Fatalf("resolved approval stayed active: %+v", m.pending)
	}
}

func TestApprovalResolvedPromotesQueued(t *testing.T) {
	m := newTestModel(t)
	m.pending = &approval.Request{ID: "1", Command: "one"}
	m.approvals = append(m.approvals, approval.Request{ID: "2", Command: "two"})
	m.removeApproval("1")
	if m.pending == nil || m.pending.ID != "2" {
		t.Fatalf("queued approval was not promoted: %+v", m.pending)
	}
}

func TestToolLimitResolvedClearsPrompt(t *testing.T) {
	m := newTestModel(t)
	req := &agent.ToolLimitRequest{ID: "a1-1", AgentID: "a1", Count: 1, Limit: 1}
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolLimit, ToolLimit: req}})
	if m.toolLimit == nil || m.toolLimit.ID != req.ID {
		t.Fatalf("tool limit = %+v, want %+v", m.toolLimit, req)
	}
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolLimitResolved, ToolLimit: req}})
	if m.toolLimit != nil {
		t.Fatalf("resolved tool limit stayed active: %+v", m.toolLimit)
	}
}
