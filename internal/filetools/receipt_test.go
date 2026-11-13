package filetools

import (
	"context"
	"testing"
)

// bigFile installs a 200-line file and returns a session for it.
func bigFile(t *testing.T) (*fixture, *Session) {
	t.Helper()
	limits := testLimits()
	limits.MaxFileBytes = 1 << 20
	limits.MaxEditResultBytes = 1 << 20
	f := newFixture(t, limits)
	f.fs.Set("big.txt", lines(200))
	return f, f.session("conv-1")
}

func TestReceiptSurvivesLaterMutation(t *testing.T) {
	f, s := bigFile(t)
	early := readID(t, s, "agent-1", "big.txt", 100, 120)
	late := readID(t, s, "agent-1", "big.txt", 150, 151)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: late, StartLine: 150, EndLine: 151, Content: "changed\n"})
	// The receipt covering [100, 120) is before the mutation at 150: still valid.
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: early, StartLine: 100, EndLine: 101, Content: "early\n"})
	_ = f
}

func TestMutationAtReceiptEndInvalidates(t *testing.T) {
	_, s := bigFile(t)
	early := readID(t, s, "agent-1", "big.txt", 100, 120)
	boundary := readID(t, s, "agent-1", "big.txt", 120, 121)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: boundary, StartLine: 120, EndLine: 121, Content: "boundary\n"})
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: early, StartLine: 100, EndLine: 101, Content: "x\n"})
	if !HasCode(err, CodeReadRequired) {
		t.Fatalf("receipt ending at the mutation boundary survived: %v", err)
	}
}

func TestMutationBeforeReceiptInvalidates(t *testing.T) {
	_, s := bigFile(t)
	r := readID(t, s, "agent-1", "big.txt", 100, 120)
	before := readID(t, s, "agent-1", "big.txt", 50, 51)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: before, StartLine: 50, EndLine: 51, Content: "early\n"})
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: r, StartLine: 100, EndLine: 101, Content: "x\n"})
	if !HasCode(err, CodeReadRequired) {
		t.Fatalf("receipt after a mutation survived: %v", err)
	}
}

func TestMultipleSurvivingReceipts(t *testing.T) {
	_, s := bigFile(t)
	first := readID(t, s, "agent-1", "big.txt", 0, 2)
	second := readID(t, s, "agent-1", "big.txt", 2, 4)
	far := readID(t, s, "agent-1", "big.txt", 10, 12)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: far, StartLine: 10, EndLine: 12, Content: "far\n"})
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: second, StartLine: 3, EndLine: 4, Content: "second\n"})
	// first ends at 2, strictly before the mutation at 3: still valid.
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: first, StartLine: 0, EndLine: 1, Content: "first\n"})
	// second was invalidated by its own mutation.
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: second, StartLine: 3, EndLine: 4, Content: "again\n"})
	if !HasCode(err, CodeReadRequired) {
		t.Fatalf("authorizing receipt survived its own edit: %v", err)
	}
}

func TestExternalChangeInvalidatesEveryReceipt(t *testing.T) {
	f, s := bigFile(t)
	first := readID(t, s, "agent-1", "big.txt", 0, 2)
	second := readID(t, s, "agent-1", "big.txt", 100, 102)
	f.fs.Set("big.txt", lines(200)+"external\n")
	_, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: first, StartLine: 0, EndLine: 1, Content: "x\n"})
	if !HasCode(err, CodeStaleRead) {
		t.Fatalf("untracked change: %v", err)
	}
	for _, id := range []string{first, second} {
		if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: id, StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
			t.Fatalf("receipt %s survived an untracked change: %v", id[:6], err)
		}
	}
}

func TestClearAgentDropsOnlyThatAgentsReceipts(t *testing.T) {
	_, s := bigFile(t)
	mine := readID(t, s, "agent-1", "big.txt", 0, 2)
	other := readID(t, s, "agent-2", "big.txt", 0, 2)
	s.ClearAgent("agent-1")
	if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: mine, StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
		t.Fatalf("cleared agent still authorized: %v", err)
	}
	mustEdit(t, s, EditRequest{AgentID: "agent-2", Filename: "big.txt", ReadID: other, StartLine: 0, EndLine: 1, Content: "x\n"})
}

func TestSessionCloseDropsRuntimeReceipts(t *testing.T) {
	f, s := bigFile(t)
	other := f.session("conv-2")
	keep := readID(t, other, "agent-1", "big.txt", 0, 2)
	r := readID(t, s, "agent-1", "big.txt", 0, 2)
	s.Close()
	if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: r, StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
		t.Fatalf("closed runtime still authorized: %v", err)
	}
	mustEdit(t, other, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: keep, StartLine: 0, EndLine: 1, Content: "x\n"})
}

func TestReceiptsSurviveOrdinaryProgress(t *testing.T) {
	f, s := bigFile(t)
	r := readID(t, s, "agent-1", "big.txt", 0, 2)
	// An ordinary turn does not end the conversation; unrelated work must not
	// clear receipts.
	f.fs.Set("other.txt", "x\n")
	mustReadRange(t, s, "agent-1", "other.txt", 0, 1)
	f.fs.Set("third.txt", "y\n")
	mustReadRange(t, s, "agent-1", "third.txt", 0, 1)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: r, StartLine: 0, EndLine: 1, Content: "x\n"})
}

func TestReadAfterTrackedEditRemainsConsistent(t *testing.T) {
	_, s := bigFile(t)
	r1 := readID(t, s, "agent-1", "big.txt", 0, 2)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: r1, StartLine: 0, EndLine: 1, Content: "x\n"})
	r2 := readID(t, s, "agent-1", "big.txt", 0, 2)
	mustEdit(t, s, EditRequest{AgentID: "agent-1", Filename: "big.txt", ReadID: r2, StartLine: 0, EndLine: 1, Content: "y\n"})
}

func TestReadWithoutResolvedTarget(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.registry.NewSession("conv-1", f.limits, func(context.Context) (Target, error) {
		return Target{}, Errorf(CodeIOError, "no channel", "")
	})
	if _, err := s.Read(context.Background(), ReadRequest{AgentID: "a", Filename: "x"}); err == nil {
		t.Fatal("read succeeded without a target")
	}
}
