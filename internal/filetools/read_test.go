package filetools

import (
	"context"
	"strings"
	"testing"
)

func TestReadEmptyFile(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("empty.txt", "")
	res := mustReadRange(t, s, "agent-1", "empty.txt", 0, 0)
	if res.FileLineCount != 0 || res.Content != "" || res.HasMore || res.NextStartLine != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.ReadID == "" {
		t.Fatal("empty read did not mint a receipt")
	}
}

func TestReadDefaultsToLineCap(t *testing.T) {
	limits := testLimits()
	limits.MaxReadLines = 2
	f := newFixture(t, limits)
	s := f.session("conv-1")
	f.fs.Set("a.txt", lines(5))

	first, err := openEndedRead(t, s, "agent-1", "a.txt", 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if first.StartLine != 0 || first.EndLine != 2 || !first.HasMore || first.NextStartLine == nil || *first.NextStartLine != 2 {
		t.Fatalf("first chunk = %+v", first)
	}
	if first.FileLineCount != 5 {
		t.Fatalf("file_line_count = %d", first.FileLineCount)
	}
	last, err := openEndedRead(t, s, "agent-1", "a.txt", *first.NextStartLine)
	if err != nil {
		t.Fatalf("read next: %v", err)
	}
	if last.StartLine != 2 || last.EndLine != 4 || !last.HasMore {
		t.Fatalf("second chunk = %+v", last)
	}
	final, err := openEndedRead(t, s, "agent-1", "a.txt", 4)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if final.HasMore || final.NextStartLine != nil || final.EndLine != 5 {
		t.Fatalf("final chunk = %+v", final)
	}
}

func TestReadExplicitRangeLimits(t *testing.T) {
	limits := testLimits()
	limits.MaxReadLines = 2
	f := newFixture(t, limits)
	s := f.session("conv-1")
	f.fs.Set("a.txt", lines(10))

	if _, err := readRange(t, s, "agent-1", "a.txt", 0, 3); !HasCode(err, CodeReadLimitExceeded) {
		t.Fatalf("oversized explicit range: %v", err)
	}
	// An explicit end is not clipped to EOF.
	if _, err := readRange(t, s, "agent-1", "a.txt", 0, 11); !HasCode(err, CodeInvalidRange) {
		t.Fatalf("end past EOF: %v", err)
	}
	// An explicit end equal to the file length is fine when it fits the cap.
	roomy := testLimits()
	roomy.MaxReadLines = 20
	f2 := newFixture(t, roomy)
	s2 := f2.session("conv-1")
	f2.fs.Set("a.txt", lines(10))
	if res, err := readRange(t, s2, "agent-1", "a.txt", 0, 10); err != nil || res.HasMore {
		t.Fatalf("explicit EOF end: %+v %v", res, err)
	}
}

func TestReadCoordinateValidation(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", lines(3))
	cases := []struct{ start, end int }{
		{-1, 1}, {0, -1}, {2, 1}, {4, 4}, {0, 5},
	}
	for _, tc := range cases {
		if _, err := readRange(t, s, "agent-1", "a.txt", tc.start, tc.end); !HasCode(err, CodeInvalidRange) {
			t.Fatalf("range [%d,%d): %v", tc.start, tc.end, err)
		}
	}
}

func TestReadByteLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxReadBytes = 5
	f := newFixture(t, limits)
	s := f.session("conv-1")
	f.fs.Set("long.txt", "abcdef\n")
	if _, err := readRange(t, s, "agent-1", "long.txt", 0, 1); !HasCode(err, CodeReadLimitExceeded) {
		t.Fatalf("long line: %v", err)
	}
	// Multibyte characters count as encoded bytes: "éé\n" is five bytes.
	f.fs.Set("multi.txt", "éé\n")
	if _, err := readRange(t, s, "agent-1", "multi.txt", 0, 1); err != nil {
		t.Fatalf("five-byte multibyte line rejected: %v", err)
	}
	limits.MaxReadBytes = 4
	f2 := newFixture(t, limits)
	s2 := f2.session("conv-1")
	f2.fs.Set("multi.txt", "éé\n")
	if _, err := readRange(t, s2, "agent-1", "multi.txt", 0, 1); !HasCode(err, CodeReadLimitExceeded) {
		t.Fatalf("multibyte accounting: %v", err)
	}
}

func TestReadValidatesWholeFile(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("bad.txt", "ok\nnul\x00\n")
	if _, err := readRange(t, s, "agent-1", "bad.txt", 0, 1); !HasCode(err, CodeInvalidText) {
		t.Fatalf("invalid text beyond the requested range: %v", err)
	}
	f.fs.Set("utf8.txt", "ok\n\xff\n")
	if _, err := readRange(t, s, "agent-1", "utf8.txt", 0, 1); !HasCode(err, CodeInvalidText) {
		t.Fatalf("invalid utf8 beyond the requested range: %v", err)
	}
}

func TestReadFileSizeLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxFileBytes = 8
	f := newFixture(t, limits)
	s := f.session("conv-1")
	f.fs.Set("big.txt", strings.Repeat("x", 9))
	if _, err := readRange(t, s, "agent-1", "big.txt", 0, 1); !HasCode(err, CodeFileLimitExceeded) {
		t.Fatalf("oversized file: %v", err)
	}
}

func TestReadPreservesBytes(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("crlf.txt", "a\r\nb\r\n")
	res := mustReadRange(t, s, "agent-1", "crlf.txt", 0, 2)
	if res.Content != "a\r\nb\r\n" {
		t.Fatalf("content = %q", res.Content)
	}
	if strings.Contains(res.Content, "1:") {
		t.Fatal("content carries line-number prefixes")
	}
}

func TestReadErrors(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	if _, err := readRange(t, s, "agent-1", "missing.txt", 0, 0); !HasCode(err, CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	f.root = ""
	if _, err := readRange(t, s, "agent-1", "a.txt", 0, 0); !HasCode(err, CodeRootNotConfigured) {
		t.Fatalf("unconfigured root: %v", err)
	}
	f.root = "/root"
	f.fs.Close()
	if _, err := s.Read(context.Background(), ReadRequest{AgentID: "agent-1", Filename: "a.txt"}); err != nil {
		// memfs does not fail after Close; the call must still behave.
		t.Logf("read after close: %v", err)
	}
}
