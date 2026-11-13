package filetools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Approval previews are bounded: the human sees what will change, and a huge
// write cannot flood the approval prompt.
const (
	maxPreviewLines = 40
	maxPreviewBytes = 4096
)

// creationPreview renders a write approval preview.
func creationPreview(filename, content string, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "create %q: %d bytes, %d lines\n", filename, len(content), lines)
	linesShown := parseDocument(content).lines
	appendPreviewLines(&b, "+", linesShown)
	return boundPreview(b.String())
}

// editPreview renders an edit approval preview describing the replaced range
// and a bounded diff.
func editPreview(filename string, req EditRequest, doc document, result string) string {
	sep := doc.separator()
	ins := parseDocument(normalizeNewlines(req.Content, sep))
	var b strings.Builder
	switch {
	case req.StartLine < req.EndLine && ins.lineCount() == 0:
		fmt.Fprintf(&b, "edit %q: delete lines [%d, %d)\n", filename, req.StartLine, req.EndLine)
	case req.StartLine < req.EndLine:
		fmt.Fprintf(&b, "edit %q: replace lines [%d, %d) with %d lines\n", filename, req.StartLine, req.EndLine, ins.lineCount())
	case ins.lineCount() == 0:
		fmt.Fprintf(&b, "edit %q: no change at line %d\n", filename, req.StartLine)
	default:
		fmt.Fprintf(&b, "edit %q: insert %d lines at line %d\n", filename, ins.lineCount(), req.StartLine)
	}
	if req.StartLine < req.EndLine {
		appendPreviewLines(&b, "-", doc.lines[req.StartLine:req.EndLine])
	}
	appendPreviewLines(&b, "+", ins.lines)
	fmt.Fprintf(&b, "result: %d bytes, %d lines\n", len(result), parseDocument(result).lineCount())
	return boundPreview(b.String())
}

// appendPreviewLines renders at most maxPreviewLines prefixed lines.
func appendPreviewLines(b *strings.Builder, prefix string, lines []string) {
	for i, line := range lines {
		if i == maxPreviewLines {
			fmt.Fprintf(b, "... (%d more lines)\n", len(lines)-i)
			return
		}
		b.WriteString(prefix)
		b.WriteByte(' ')
		b.WriteString(strings.ReplaceAll(line, "\r", "\\r"))
		b.WriteByte('\n')
	}
}

// boundPreview caps the preview at maxPreviewBytes without splitting a rune.
func boundPreview(s string) string {
	if len(s) <= maxPreviewBytes {
		return s
	}
	cut := maxPreviewBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "... (preview truncated)\n"
}
