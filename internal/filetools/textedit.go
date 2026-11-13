package filetools

import "strings"

// applyEdit replaces complete lines [start, end) of doc with supplied content
// and returns the resulting file bytes. Line endings follow the file's existing
// style; inserted content is normalized to that style. Unaffected bytes,
// including the final newline, are preserved; the supplied content controls the
// final newline when the edit reaches EOF.
func applyEdit(doc document, start, end int, supplied string) (string, error) {
	if doc.style == styleMixed {
		return "", Errorf(CodeMixedNewlines, "file mixes CRLF and LF line endings", hintFor(CodeMixedNewlines))
	}
	if start < 0 || end < start || end > doc.lineCount() {
		return "", invalidRange("", start, end, doc.lineCount())
	}
	sep := doc.separator()
	ins := parseDocument(normalizeNewlines(supplied, sep))

	lines := make([]string, 0, start+ins.lineCount()+doc.lineCount()-end)
	delims := make([]string, 0, cap(lines))
	lines = append(lines, doc.lines[:start]...)
	delims = append(delims, doc.delims[:start]...)
	lines = append(lines, ins.lines...)
	delims = append(delims, ins.delims...)
	lines = append(lines, doc.lines[end:]...)
	delims = append(delims, doc.delims[end:]...)

	// Every line except the last must be terminated: an unterminated retained
	// line before inserted content, and an inserted line without a trailing
	// delimiter before retained lines, both need a separator.
	for i := 0; i < len(lines)-1; i++ {
		if delims[i] == "" {
			delims[i] = sep
		}
	}
	var b strings.Builder
	b.Grow(len(supplied) + (end-start)*8)
	for i := range lines {
		b.WriteString(lines[i])
		b.WriteString(delims[i])
	}
	return b.String(), nil
}
