package filetools

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentProposalsPublishOnce proves mutations of one target serialize:
// two proposals built from the same read cannot both publish, so an edit never
// silently overwrites the other one.
func TestConcurrentProposalsPublishOnce(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "a\nb\n")
	first := readID(t, s, "agent-1", "a.txt", 0, 1)
	second := readID(t, s, "agent-2", "a.txt", 0, 1)

	p1, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: first, StartLine: 0, EndLine: 1, Content: "one\n"})
	if err != nil {
		t.Fatalf("propose 1: %v", err)
	}
	p2, err := s.ProposeEdit(context.Background(), EditRequest{AgentID: "agent-2", Filename: "a.txt", ReadID: second, StartLine: 0, EndLine: 1, Content: "two\n"})
	if err != nil {
		t.Fatalf("propose 2: %v", err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, p := range []*EditProposal{p1, p2} {
		wg.Add(1)
		go func(proposal *EditProposal) {
			defer wg.Done()
			_, err := proposal.Commit(context.Background())
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	successes, stale := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case HasCode(err, CodeStaleRead):
			stale++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("successes = %d, stale = %d, want 1 and 1", successes, stale)
	}
	content, _ := f.fs.Content("a.txt")
	if content != "one\nb\n" && content != "two\nb\n" {
		t.Fatalf("content = %q, want exactly one published edit", content)
	}
}

// TestConcurrentReadsAndEditsKeepLedgerConsistent drives many agents against one
// file: every read/edit pair either succeeds or fails with a stable code, and
// the final content must be one of the sequential results (no interleaving).
func TestConcurrentReadsAndEditsKeepLedgerConsistent(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", "0\n")

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			agent := fmt.Sprintf("agent-%d", n)
			for attempt := 0; attempt < 5; attempt++ {
				end := 1
				res, err := s.Read(context.Background(), ReadRequest{AgentID: agent, Filename: "a.txt", StartLine: 0, EndLine: &end})
				if err != nil {
					if HasCode(err, CodeNotFound) || HasCode(err, CodeStaleRead) || HasCode(err, CodeReadRequired) {
						continue
					}
					t.Errorf("read: %v", err)
					return
				}
				_, err = editOnce(t, s, EditRequest{AgentID: agent, Filename: "a.txt", ReadID: res.ReadID, StartLine: 0, EndLine: 1, Content: fmt.Sprintf("%d\n", n)})
				if err != nil && !HasCode(err, CodeStaleRead) && !HasCode(err, CodeReadRequired) {
					t.Errorf("edit: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	content, ok := f.fs.Content("a.txt")
	if !ok {
		t.Fatal("file disappeared")
	}
	if CountLines(content) != 1 {
		t.Fatalf("content = %q, want a single line", content)
	}
}

// TestConcurrentCreatesPublishOnce proves atomic no-replace creation: of two
// concurrent writes to one new path, exactly one succeeds.
func TestConcurrentCreatesPublishOnce(t *testing.T) {
	f := newFixture(t, testLimits())
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := f.session("conv-1")
			proposal, err := s.ProposeWrite(context.Background(), WriteRequest{
				AgentID: "agent-1", Filename: "new.txt", Content: fmt.Sprintf("writer-%d\n", n),
			})
			if err != nil {
				results[n] = err
				return
			}
			_, results[n] = proposal.Commit(context.Background())
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case HasCode(err, CodeAlreadyExists):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != len(results)-1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
}
