package tui

// pushHistory records a submitted prompt, deduplicating consecutive repeats.
func (m *Model) pushHistory(line string) {
	if n := len(m.history); n > 0 && m.history[n-1] == line {
		return
	}
	m.history = append(m.history, line)
}

// historyUp moves toward older prompts. From the draft/empty area it first saves
// the current half-finished text as the draft, then shows the newest prompt.
func (m *Model) historyUp() {
	if m.histIdx == -1 {
		if v := m.textarea.Value(); v != "" {
			m.draft = v
		}
		if len(m.history) == 0 {
			return
		}
		m.histIdx = len(m.history) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	} else {
		return
	}
	m.textarea.SetValue(m.history[m.histIdx])
	m.textarea.CursorEnd()
}

// historyDown moves toward newer prompts. At the newest prompt it steps back into
// the draft; from the draft/empty area it saves the half-finished text and shows
// an empty box (the half-finished text is discarded only when a new prompt is
// sent).
func (m *Model) historyDown() {
	if m.histIdx == -1 {
		if v := m.textarea.Value(); v != "" {
			m.draft = v
			m.textarea.SetValue("")
		}
		m.textarea.CursorEnd()
		return
	}
	if m.histIdx < len(m.history)-1 {
		m.histIdx++
		m.textarea.SetValue(m.history[m.histIdx])
	} else {
		m.histIdx = -1
		m.textarea.SetValue(m.draft)
	}
	m.textarea.CursorEnd()
}
