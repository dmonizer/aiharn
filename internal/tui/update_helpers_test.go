package tui

import (
	"testing"

	"aiharn/internal/agent"
)

func TestApplyToolLimitNilAndQueue(t *testing.T) {
	m := newTestModel(t)
	m.applyToolLimit(nil)
	if m.toolLimit != nil || len(m.toolLimitQueue) != 0 {
		t.Fatalf("nil tool limit changed state: %+v", m.toolLimit)
	}

	m.applyToolLimit(&agent.ToolLimitRequest{ID: "1", Count: 2, Limit: 5})
	if m.toolLimit == nil || m.toolLimit.Count != 2 {
		t.Fatalf("first tool limit not recorded: %+v", m.toolLimit)
	}

	m.applyToolLimit(&agent.ToolLimitRequest{ID: "2", Count: 3, Limit: 5})
	if len(m.toolLimitQueue) != 1 || m.toolLimitQueue[0].Count != 3 {
		t.Fatalf("queued tool limit = %+v", m.toolLimitQueue)
	}
}
