package agent

import "aiharn/internal/llm"

// ReasoningStatus is the currently streamed thinking block. Completed blocks
// live in History as display-only reasoning items.
type ReasoningStatus struct {
	Text   string `json:"text"`
	Active bool   `json:"active"`
}

func (a *Agent) LiveReasoning() *ReasoningStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.thinkingActive && a.liveReasoning == "" {
		return nil
	}
	return &ReasoningStatus{Text: a.liveReasoning, Active: a.thinkingActive}
}

func (a *Agent) setThinkingActive(active bool) {
	a.mu.Lock()
	a.thinkingActive = active
	a.mu.Unlock()
}

func (a *Agent) addReasoning(delta string) {
	a.mu.Lock()
	a.liveReasoning += delta
	a.mu.Unlock()
}

func (a *Agent) finishReasoning() {
	a.mu.Lock()
	text := a.liveReasoning
	a.liveReasoning = ""
	a.thinkingActive = false
	a.mu.Unlock()
	if text != "" {
		a.append(llm.Item{Type: llm.ItemReasoning, Content: text})
	}
}
