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

// maxInputHeight caps the input box at one quarter of the screen height, but
// never below one line.
func (m *Model) maxInputHeight() int {
	h := m.height / 4
	if h < 1 {
		h = 1
	}
	return h
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
// overflow once the quarter-screen cap is reached.
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
// width, height grown to fit the content up to a quarter of the screen.
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
