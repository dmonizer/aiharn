package filetools

import (
	"strings"
	"unicode/utf8"
)

// newlineStyle is the line-ending convention of a text file.
type newlineStyle int

const (
	// styleNone is a file without any line delimiter (empty or a single
	// unterminated line).
	styleNone newlineStyle = iota
	styleLF
	styleCRLF
	// styleMixed is a file that mixes LF and CRLF delimiters. Reads preserve it;
	// edits reject it.
	styleMixed
)

const (
	lfDelim   = "\n"
	crlfDelim = "\r\n"
)

// document is a parsed text file: every line's text and its own delimiter. Only
// the last line may have an empty delimiter.
type document struct {
	lines  []string
	delims []string
	style  newlineStyle
}

// parseDocument splits content into lines. LF and CRLF delimit a line; a lone CR
// is text. A trailing delimiter terminates the last line and creates no extra
// empty line, so "a\n" is one line, "a\nb" is two, "\n" is one empty line, and
// "" is zero lines.
func parseDocument(content string) document {
	var d document
	lf, crlf := 0, 0
	for i := 0; i < len(content); {
		rel := strings.IndexByte(content[i:], '\n')
		if rel < 0 {
			d.lines = append(d.lines, content[i:])
			d.delims = append(d.delims, "")
			break
		}
		lineEnd := i + rel
		text, delim := content[i:lineEnd], lfDelim
		if lineEnd > i && content[lineEnd-1] == '\r' {
			text, delim = content[i:lineEnd-1], crlfDelim
			crlf++
		} else {
			lf++
		}
		d.lines = append(d.lines, text)
		d.delims = append(d.delims, delim)
		i = lineEnd + 1
	}
	d.style = styleOf(lf, crlf)
	return d
}

func styleOf(lf, crlf int) newlineStyle {
	switch {
	case lf == 0 && crlf == 0:
		return styleNone
	case lf == 0:
		return styleCRLF
	case crlf == 0:
		return styleLF
	default:
		return styleMixed
	}
}

// lineCount returns the number of lines in the document.
func (d document) lineCount() int { return len(d.lines) }

// separator returns the delimiter to use for any boundary this edit introduces.
// A file without a delimiter gets LF.
func (d document) separator() string {
	if d.style == styleCRLF {
		return crlfDelim
	}
	return lfDelim
}

// segment returns the original bytes of lines [start, end).
func (d document) segment(start, end int) string {
	var b strings.Builder
	for i := start; i < end && i < len(d.lines); i++ {
		b.WriteString(d.lines[i])
		b.WriteString(d.delims[i])
	}
	return b.String()
}

// validateText rejects content that is not valid UTF-8 or contains NUL bytes.
func validateText(content string) error {
	if !utf8.ValidString(content) {
		return Errorf(CodeInvalidText, "content is not valid UTF-8", "supply UTF-8 text; binary files are not supported")
	}
	if strings.IndexByte(content, 0) >= 0 {
		return Errorf(CodeInvalidText, "content contains NUL bytes", "supply text without NUL bytes; binary files are not supported")
	}
	return nil
}

// normalizeNewlines rewrites every LF and CRLF separator in s to sep. A lone CR
// is left untouched.
func normalizeNewlines(s, sep string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				b.WriteString(sep)
				i++
				continue
			}
			b.WriteByte('\r')
		case '\n':
			b.WriteString(sep)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// CountLines reports the number of lines in content under the shared line
// model: LF and CRLF delimit lines, a trailing delimiter terminates the last
// line and creates no extra empty line, and an empty string has zero lines.
func CountLines(content string) int { return parseDocument(content).lineCount() }
