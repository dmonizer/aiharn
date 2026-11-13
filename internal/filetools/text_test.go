package filetools

import (
	"strings"
	"testing"
)

func TestParseDocument(t *testing.T) {
	cases := []struct {
		name    string
		content string
		lines   []string
		delims  []string
		style   newlineStyle
	}{
		{name: "empty", content: "", lines: nil, style: styleNone},
		{name: "unterminated", content: "a", lines: []string{"a"}, delims: []string{""}, style: styleNone},
		{name: "trailing lf", content: "a\n", lines: []string{"a"}, delims: []string{"\n"}, style: styleLF},
		{name: "two lines", content: "a\nb", lines: []string{"a", "b"}, delims: []string{"\n", ""}, style: styleLF},
		{name: "single empty line", content: "\n", lines: []string{""}, delims: []string{"\n"}, style: styleLF},
		{name: "two empty lines", content: "\n\n", lines: []string{"", ""}, delims: []string{"\n", "\n"}, style: styleLF},
		{name: "crlf", content: "a\r\nb\r\n", lines: []string{"a", "b"}, delims: []string{"\r\n", "\r\n"}, style: styleCRLF},
		{name: "lone cr is text", content: "a\rb", lines: []string{"a\rb"}, delims: []string{""}, style: styleNone},
		{name: "mixed", content: "a\nb\r\n", style: styleMixed},
		{name: "mixed trailing", content: "a\r\n\n", style: styleMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := parseDocument(tc.content)
			if d.style != tc.style {
				t.Fatalf("style = %d, want %d", d.style, tc.style)
			}
			if tc.style == styleMixed {
				return
			}
			if strings.Join(d.lines, "|") != strings.Join(tc.lines, "|") {
				t.Fatalf("lines = %q, want %q", d.lines, tc.lines)
			}
			if strings.Join(d.delims, "|") != strings.Join(tc.delims, "|") {
				t.Fatalf("delims = %q, want %q", d.delims, tc.delims)
			}
			if d.lineCount() != len(tc.lines) {
				t.Fatalf("lineCount = %d, want %d", d.lineCount(), len(tc.lines))
			}
		})
	}
}

func TestParseDocumentSegmentPreservesBytes(t *testing.T) {
	d := parseDocument("a\r\nb\nc")
	if got := d.segment(0, 2); got != "a\r\nb\n" {
		t.Fatalf("segment = %q", got)
	}
	if got := d.segment(2, 3); got != "c" {
		t.Fatalf("segment = %q", got)
	}
}

func TestNormalizeNewlines(t *testing.T) {
	cases := []struct{ in, sep, want string }{
		{"a\nb", "\n", "a\nb"},
		{"a\nb", "\r\n", "a\r\nb"},
		{"a\r\nb", "\r\n", "a\r\nb"},
		{"a\r\nb", "\n", "a\nb"},
		{"a\rb", "\n", "a\rb"},
		{"a\r\n\r\nb", "\n", "a\n\nb"},
		{"no newline", "\r\n", "no newline"},
	}
	for _, tc := range cases {
		if got := normalizeNewlines(tc.in, tc.sep); got != tc.want {
			t.Fatalf("normalizeNewlines(%q, %q) = %q, want %q", tc.in, tc.sep, got, tc.want)
		}
	}
}

func TestApplyEdit(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		start    int
		end      int
		supplied string
		want     string
	}{
		{name: "replace at start", content: "a\nb\n", start: 0, end: 1, supplied: "x", want: "x\nb\n"},
		{name: "replace at start with newline", content: "a\nb\n", start: 0, end: 1, supplied: "x\n", want: "x\nb\n"},
		{name: "replace at eof", content: "a\nb\n", start: 1, end: 2, supplied: "x", want: "a\nx"},
		{name: "replace interior", content: "a\nb\nc\n", start: 1, end: 2, supplied: "x\ny", want: "a\nx\ny\nc\n"},
		{name: "delete first", content: "a\nb\n", start: 0, end: 1, supplied: "", want: "b\n"},
		{name: "delete last", content: "a\nb\n", start: 1, end: 2, supplied: "", want: "a\n"},
		{name: "delete all", content: "a\nb\n", start: 0, end: 2, supplied: "", want: ""},
		{name: "insert at start", content: "a\nb\n", start: 0, end: 0, supplied: "x", want: "x\na\nb\n"},
		{name: "insert interior", content: "a\nb\n", start: 1, end: 1, supplied: "x", want: "a\nx\nb\n"},
		{name: "append without final newline", content: "a\nb\n", start: 2, end: 2, supplied: "x", want: "a\nb\nx"},
		{name: "append with final newline", content: "a\nb\n", start: 2, end: 2, supplied: "x\n", want: "a\nb\nx\n"},
		{name: "append after unterminated line", content: "a\nb", start: 2, end: 2, supplied: "x", want: "a\nb\nx"},
		{name: "prepend before unterminated line", content: "a", start: 0, end: 0, supplied: "x", want: "x\na"},
		{name: "replace unterminated last line", content: "a", start: 0, end: 1, supplied: "x\n", want: "x\n"},
		{name: "crlf replace keeps style", content: "a\r\nb\r\n", start: 1, end: 2, supplied: "x", want: "a\r\nx"},
		{name: "crlf supplied lf normalized", content: "a\r\nb\r\n", start: 2, end: 2, supplied: "x\ny\n", want: "a\r\nb\r\nx\r\ny\r\n"},
		{name: "crlf supplied crlf", content: "a\r\n", start: 1, end: 1, supplied: "x\r\n", want: "a\r\nx\r\n"},
		{name: "single unterminated line deleted", content: "a", start: 0, end: 1, supplied: "", want: ""},
		{name: "empty file insert", content: "", start: 0, end: 0, supplied: "x", want: "x"},
		{name: "empty file insert empty", content: "", start: 0, end: 0, supplied: "", want: ""},
		{name: "lone cr preserved", content: "a\rb\nc\n", start: 0, end: 1, supplied: "z", want: "z\nc\n"},
		{name: "insert empty at eof keeps final newline", content: "a\n", start: 1, end: 1, supplied: "", want: "a\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyEdit(parseDocument(tc.content), tc.start, tc.end, tc.supplied)
			if err != nil {
				t.Fatalf("applyEdit: %v", err)
			}
			if got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyEditRejectsMixedNewlines(t *testing.T) {
	_, err := applyEdit(parseDocument("a\nb\r\n"), 0, 1, "x")
	if !HasCode(err, CodeMixedNewlines) {
		t.Fatalf("err = %v, want mixed_newlines", err)
	}
}

func TestApplyEditRejectsOutOfRange(t *testing.T) {
	if _, err := applyEdit(parseDocument("a\n"), 0, 2, "x"); !HasCode(err, CodeInvalidRange) {
		t.Fatalf("err = %v, want invalid_range", err)
	}
}

func TestValidateText(t *testing.T) {
	if err := validateText("ok \u00e9\n"); err != nil {
		t.Fatalf("valid text rejected: %v", err)
	}
	if err := validateText("bad \xff"); !HasCode(err, CodeInvalidText) {
		t.Fatalf("invalid utf8 accepted: %v", err)
	}
	if err := validateText("a\x00b"); !HasCode(err, CodeInvalidText) {
		t.Fatalf("nul accepted: %v", err)
	}
}
