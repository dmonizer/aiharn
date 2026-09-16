package tui

import (
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/rivo/uniseg"
)

// escClearWindow is the longest gap between the two presses of a "double ESC"
// that clears the input line.
const escClearWindow = 500 * time.Millisecond

const (
	// defaultInputHeight is the input box's starting max height: a few lines.
	defaultInputHeight = 4
	// minInputHeight is the smallest the input box can be shrunk to.
	minInputHeight = 1
)

// newTextarea builds the multi-line input widget. Enter is reserved for
// submitting to the agent, so a literal newline is inserted with Ctrl+J.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "> "
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxWidth = 0
	ta.MaxHeight = 0 // no line/height cap: display height is set via SetHeight
	ta.EndOfBufferCharacter = ' '
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j"),
		key.WithHelp("ctrl+j", "insert newline"),
	)
	return ta
}

// sanitizeTerminalText removes terminal control sequences from untrusted model,
// tool, and remote-command text while preserving normal whitespace. In
// particular ESC and C1 controls must never reach the user's terminal.
func sanitizeTerminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

func sanitizeTerminalLine(s string) string {
	s = sanitizeTerminalText(s)
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// maxInputHeight returns the user-adjustable cap for the input box, clamped so
// it can never grow beyond the screen minus a status line and one transcript
// row. It never returns below one line.
func (m *Model) maxInputHeight() int {
	h := m.inputMax
	if h < minInputHeight {
		h = minInputHeight
	}
	if limit := m.height - 2; limit > 0 && h > limit {
		h = limit
	}
	return h
}

// resizeInputBy grows (delta > 0) or shrinks (delta < 0) the input box's max
// height and re-applies it.
func (m *Model) resizeInputBy(delta int) {
	m.inputMax += delta
	if m.inputMax < minInputHeight {
		m.inputMax = minInputHeight
	}
	m.resizeInput()
}

// inputBoxHeight is the number of rows the input widget currently renders.
func (m *Model) inputBoxHeight() int {
	if h := m.textarea.Height(); h >= 1 {
		return h
	}
	return 1
}

// inputContentWidth is the number of columns available for text after the
// textarea reserves the "> " prompt at the start of each line.
func (m *Model) inputContentWidth() int {
	w := m.width - 2
	if w < 1 {
		w = 40
	}
	return w
}

// inputDisplayHeight is the number of rows the input occupies after soft
// wrapping. It over-estimates by treating every line as character-wrapped so
// the box never clips; the textarea's own viewport absorbs any residual
// overflow once the max-height cap is reached.
func (m *Model) inputDisplayHeight() int {
	w := m.inputContentWidth()
	n := 0
	for _, ln := range strings.Split(m.textarea.Value(), "\n") {
		lw := uniseg.StringWidth(ln)
		if lw == 0 {
			n++
			continue
		}
		n += (lw + w - 1) / w
	}
	return n
}

// resizeInput applies the current terminal size to the input widget: full
// width, height grown to fit the content up to the configured max height.
func (m *Model) resizeInput() {
	if m.width > 0 {
		m.textarea.SetWidth(m.width)
	}
	h := m.inputDisplayHeight()
	if max := m.maxInputHeight(); h > max {
		h = max
	}
	if h < 1 {
		h = 1
	}
	m.textarea.SetHeight(h)
}

// scrollInput moves the textarea cursor by visual rows. The textarea keeps its
// viewport pinned to the cursor, which provides reliable scrolling even though
// its embedded viewport is not exposed publicly.
func (m *Model) scrollInput(delta int) {
	if delta < 0 {
		for ; delta < 0; delta++ {
			m.textarea.CursorUp()
		}
	} else {
		for ; delta > 0; delta-- {
			m.textarea.CursorDown()
		}
	}
	// Force the textarea's viewport-repositioning pass after direct cursor moves.
	m.textarea, _ = m.textarea.Update(nil)
}

func (m *Model) pageInput(direction int) {
	rows := m.inputBoxHeight()
	if rows < 1 {
		rows = 1
	}
	m.scrollInput(direction * rows)
}

// pressEsc implements double-ESC-to-clear: the first press arms the clear and a
// second press within escClearWindow clears the input.
func (m *Model) pressEsc(now time.Time) {
	if !m.lastEsc.IsZero() && now.Sub(m.lastEsc) <= escClearWindow {
		m.textarea.Reset()
		m.resizeInput()
		m.lastEsc = time.Time{}
		return
	}
	m.lastEsc = now
}
