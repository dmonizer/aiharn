package filetools

import (
	"context"
	"strings"
	"testing"
)

func proposeWrite(t *testing.T, s *Session, agent, name, content string) (*WriteProposal, error) {
	t.Helper()
	return s.ProposeWrite(context.Background(), WriteRequest{AgentID: agent, Filename: name, Content: content})
}

func TestWriteCreatesFile(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	p, err := proposeWrite(t, s, "agent-1", "new.txt", "a\nb\nc\n")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !strings.Contains(p.Preview(), "create") || !strings.Contains(p.Preview(), "+ a") {
		t.Fatalf("preview = %q", p.Preview())
	}
	res, err := p.Commit(context.Background())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Filename != "new.txt" || res.Bytes != 6 || res.Lines != 3 {
		t.Fatalf("result = %+v", res)
	}
	if content, _ := f.fs.Content("new.txt"); content != "a\nb\nc\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestWriteEmptyContent(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	p, err := proposeWrite(t, s, "agent-1", "empty.txt", "")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	res, err := p.Commit(context.Background())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Bytes != 0 || res.Lines != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestWriteRejectsExisting(t *testing.T) {
	f := newFixture(t, testLimits())
	f.fs.Set("a.txt", "old")
	s := f.session("conv-1")
	if _, err := proposeWrite(t, s, "agent-1", "a.txt", "new"); !HasCode(err, CodeAlreadyExists) {
		t.Fatalf("existing destination: %v", err)
	}
	if content, _ := f.fs.Content("a.txt"); content != "old" {
		t.Fatalf("existing content changed: %q", content)
	}
}

func TestWriteRequiresParent(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	if _, err := proposeWrite(t, s, "agent-1", "missing/a.txt", "x"); !HasCode(err, CodeParentNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
}

func TestWriteLimits(t *testing.T) {
	limits := testLimits()
	limits.MaxWriteBytes = 4
	f := newFixture(t, limits)
	s := f.session("conv-1")
	if _, err := proposeWrite(t, s, "agent-1", "a.txt", "1234"); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	if _, err := proposeWrite(t, s, "agent-1", "b.txt", "12345"); !HasCode(err, CodeFileLimitExceeded) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestWriteInvalidText(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	if _, err := proposeWrite(t, s, "agent-1", "a.txt", "bad\xff"); !HasCode(err, CodeInvalidText) {
		t.Fatalf("invalid utf8: %v", err)
	}
	if _, err := proposeWrite(t, s, "agent-1", "a.txt", "bad\x00"); !HasCode(err, CodeInvalidText) {
		t.Fatalf("nul byte: %v", err)
	}
}

func TestWriteChannelSwitchWhileApproving(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	p, err := proposeWrite(t, s, "agent-1", "a.txt", "x")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	f.switchChannel("other")
	if _, err := p.Commit(context.Background()); !HasCode(err, CodeChannelChanged) {
		t.Fatalf("switch: %v", err)
	}
	if _, ok := f.fs.Content("a.txt"); ok {
		t.Fatal("file was published after a channel switch")
	}
	f.switchChannel("local")
	if _, err := p.Commit(context.Background()); !HasCode(err, CodeChannelChanged) {
		t.Fatalf("switch back: %v", err)
	}
}

func TestWritePublishFailureLeavesNothing(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	p, err := proposeWrite(t, s, "agent-1", "a.txt", "x")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	f.fs.FailNext("io_error")
	if _, err := p.Commit(context.Background()); !HasCode(err, CodeIOError) {
		t.Fatalf("publish failure: %v", err)
	}
	if _, ok := f.fs.Content("a.txt"); ok {
		t.Fatal("failed publish left a file behind")
	}
}

func TestWriteDoesNotSatisfyReadBeforeEdit(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	p, err := proposeWrite(t, s, "agent-1", "a.txt", "a\n")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := p.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	_, err = editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: "", StartLine: 0, EndLine: 1, Content: "b\n"})
	if !HasCode(err, CodeReadRequired) {
		t.Fatalf("edit after write: %v", err)
	}
}

func TestWriteRootNotConfigured(t *testing.T) {
	f := newFixture(t, testLimits())
	f.root = ""
	s := f.session("conv-1")
	if _, err := proposeWrite(t, s, "agent-1", "a.txt", "x"); !HasCode(err, CodeRootNotConfigured) {
		t.Fatalf("root: %v", err)
	}
}
