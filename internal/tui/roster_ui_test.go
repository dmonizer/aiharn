package tui

import "testing"

func TestRosterHitAtNoMatch(t *testing.T) {
	m := newTestModel(t)
	if _, ok := m.rosterHitAt(0, 0); ok {
		t.Fatal("rosterHitAt reported a hit with no roster hits")
	}
}

func TestPauseSubagentMissing(t *testing.T) {
	m := newTestModel(t)
	// A missing subagent must not panic and must not change the roster.
	m.pauseSubagent("missing")
	if len(m.subagents) != 0 {
		t.Fatalf("pauseSubagent changed subagents: %+v", m.subagents)
	}
}
