package filetools

import (
	"context"
	"strings"
	"testing"
)

// readID returns the receipt id of an exact read range.
func readID(t *testing.T, s *Session, agent, name string, start, end int) string {
	t.Helper()
	return mustReadRange(t, s, agent, name, start, end).ReadID
}

func TestEditReplacement(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\nc\n")
	id := readID(t, s, "agent-1", "a.txt", 1, 2)
	res := mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 1, EndLine: 2, Content: "B\n"})
	if content, _ := f.fs.Content("a.txt"); content != "a\nB\nc\n" {
		t.Fatalf("content = %q", content)
	}
	if res.StartLine != 1 || res.EndLine != 2 || res.Bytes != len("a\nB\nc\n") || res.LineCount != 3 {
		t.Fatalf("result = %+v", res)
	}
}

func TestEditDeletion(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\nc\n")
	id := readID(t, s, "agent-1", "a.txt", 1, 2)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 1, EndLine: 2, Content: ""})
	if content, _ := f.fs.Content("a.txt"); content != "a\nc\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditInsertionContexts(t *testing.T) {
	t.Run("prepend", func(t *testing.T) {
		f := newFixture(t, testLimits())
		s := f.session("conv-1")
		f.fs.Set("a.txt", "a\nb\n")
		id := readID(t, s, "agent-1", "a.txt", 0, 1)
		mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 0, Content: "z\n"})
		if content, _ := f.fs.Content("a.txt"); content != "z\na\nb\n" {
			t.Fatalf("content = %q", content)
		}
	})
	t.Run("interior", func(t *testing.T) {
		f := newFixture(t, testLimits())
		s := f.session("conv-1")
		f.fs.Set("a.txt", "a\nb\n")
		id := readID(t, s, "agent-1", "a.txt", 0, 2)
		mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 1, EndLine: 1, Content: "z\n"})
		if content, _ := f.fs.Content("a.txt"); content != "a\nz\nb\n" {
			t.Fatalf("content = %q", content)
		}
	})
	t.Run("append", func(t *testing.T) {
		f := newFixture(t, testLimits())
		s := f.session("conv-1")
		f.fs.Set("a.txt", "a\nb\n")
		id := readID(t, s, "agent-1", "a.txt", 1, 2)
		mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 2, EndLine: 2, Content: "z"})
		if content, _ := f.fs.Content("a.txt"); content != "a\nb\nz" {
			t.Fatalf("content = %q", content)
		}
	})
	t.Run("append after unterminated line", func(t *testing.T) {
		f := newFixture(t, testLimits())
		s := f.session("conv-1")
		f.fs.Set("a.txt", "a\nb")
		id := readID(t, s, "agent-1", "a.txt", 1, 2)
		mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 2, EndLine: 2, Content: "z"})
		if content, _ := f.fs.Content("a.txt"); content != "a\nb\nz" {
			t.Fatalf("content = %q", content)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		f := newFixture(t, testLimits())
		s := f.session("conv-1")
		f.fs.Set("empty.txt", "")
		id := readID(t, s, "agent-1", "empty.txt", 0, 0)
		mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "empty.txt", ReadID: id, StartLine: 0, EndLine: 0, Content: "hello\n"})
		if content, _ := f.fs.Content("empty.txt"); content != "hello\n" {
			t.Fatalf("content = %q", content)
		}
	})
}

func TestEditInsertionNeedsBothAdjacentLines(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\nc\n")
	first := readID(t, s, "agent-1", "a.txt", 0, 1)
	second := readID(t, s, "agent-1", "a.txt", 1, 2)
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: first, StartLine: 1, EndLine: 1, Content: "z\n"})
	if !HasCode(err, CodeReadScopeMismatch) {
		t.Fatalf("single adjacent read: %v", err)
	}
	_, err = editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: second, StartLine: 1, EndLine: 1, Content: "z\n"})
	if !HasCode(err, CodeReadScopeMismatch) {
		t.Fatalf("receipts must not combine: %v", err)
	}
}

func TestEditRequiresReceipt(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: "forged", StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeReadRequired) {
		t.Fatalf("forged receipt: %v", err)
	}
}

func TestEditReceiptScope(t *testing.T) {
	f := newFixture(t, testLimits())
	f.fs.Set("a.txt", "a\nb\n")
	first := f.session("conv-1")
	id := readID(t, first, "agent-1", "a.txt", 0, 1)

	_, err := editOnce(t, first, EditRequest{AgentID: "agent-2", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeReadScopeMismatch) {
		t.Fatalf("cross-agent: %v", err)
	}
	other := f.session("conv-2")
	_, err = editOnce(t, other, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeReadScopeMismatch) {
		t.Fatalf("cross-conversation: %v", err)
	}
	f.switchChannel("other")
	_, err = editOnce(t, first, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeReadScopeMismatch) {
		t.Fatalf("cross-channel: %v", err)
	}
}

func TestEditStaleVersion(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	p, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	f.fs.Set("a.txt", "changed\nb\n")
	if _, err := p.Commit(context.Background()); !HasCode(err, CodeStaleRead) {
		t.Fatalf("stale commit: %v", err)
	}
	if content, _ := f.fs.Content("a.txt"); content != "changed\nb\n" {
		t.Fatalf("stale commit changed content: %q", content)
	}
	// The stale observation invalidates receipts: the old id is gone.
	if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
		t.Fatalf("receipt survived a stale read: %v", err)
	}
}

func TestEditChannelSwitchWhileApproving(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	p, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	f.switchChannel("other")
	if _, err := p.Commit(context.Background()); !HasCode(err, CodeChannelChanged) {
		t.Fatalf("commit after switch: %v", err)
	}
	if content, _ := f.fs.Content("a.txt"); content != "a\nb\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditNewlineStyles(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")

	f.fs.Set("crlf.txt", "a\r\nb\r\n")
	id := readID(t, s, "agent-1", "crlf.txt", 1, 2)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "crlf.txt", ReadID: id, StartLine: 1, EndLine: 2, Content: "x\ny\n"})
	if content, _ := f.fs.Content("crlf.txt"); content != "a\r\nx\r\ny\r\n" {
		t.Fatalf("crlf content = %q", content)
	}

	f.fs.Set("mixed.txt", "a\nb\r\n")
	if _, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "mixed.txt", StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeMixedNewlines) {
		t.Fatalf("mixed: %v", err)
	}
}

func TestEditResultLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxEditResultBytes = 9
	f := newFixture(t, limits)
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	if _, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "abcdefgh\n"}); !HasCode(err, CodeFileLimitExceeded) {
		t.Fatalf("over result limit: %v", err)
	}
	// Exactly at the limit is allowed.
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "abcdef\n"})
	if content, _ := f.fs.Content("a.txt"); content != "abcdef\nb\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditHardLinkRejected(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\n")
	f.fs.SetLinkCount("a.txt", 2)
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "b\n"})
	if !HasCode(err, CodeHardLinksNotAllowed) {
		t.Fatalf("hard link: %v", err)
	}
}

func TestEditOutcomeUnknown(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	f.fs.FailNext("outcome_unknown")
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeOutcomeUnknown) {
		t.Fatalf("outcome unknown: %v", err)
	}
	if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
		t.Fatalf("receipt survived an unknown outcome: %v", err)
	}
}

func TestEditPreviewShowsDiff(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "old\nkeep\n")
	id := readID(t, s, "agent-1", "a.txt", 0, 1)
	p, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "new\n"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	preview := p.Preview()
	for _, want := range []string{"edit", "- old", "+ new", "result:"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview %q missing %q", preview, want)
		}
	}
}

func TestPreviewIsBounded(t *testing.T) {
	big := strings.Repeat("x", maxPreviewBytes*2)
	preview := creationPreview("big.txt", big, 1)
	if len(preview) > maxPreviewBytes+64 {
		t.Fatalf("preview length = %d", len(preview))
	}
}
