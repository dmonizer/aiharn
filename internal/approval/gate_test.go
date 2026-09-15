package approval

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAllowAllImmediate(t *testing.T) {
	g := NewGate(ModeAllowAll)
	d, err := g.Check(context.Background(), Request{ToolName: "execute_command", Command: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if d != DecisionApproved {
		t.Fatalf("got %v", d)
	}
	select {
	case <-g.Pending():
		t.Fatal("unexpected pending request")
	default:
	}
}

func TestAskBlocksUntilApproved(t *testing.T) {
	g := NewGate(ModeAsk)

	done := make(chan struct{})
	var d Decision
	var err error
	go func() {
		defer close(done)
		d, err = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "rm -rf /"})
	}()

	var req Request
	select {
	case req = <-g.Pending():
	case <-time.After(time.Second):
		t.Fatal("no pending request surfaced")
	}
	if req.ID == "" {
		t.Fatal("empty request ID")
	}
	if req.Command != "rm -rf /" {
		t.Fatalf("command = %q", req.Command)
	}

	if err := g.Decide(req.ID, DecisionApproved); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Check did not return")
	}
	if err != nil {
		t.Fatal(err)
	}
	if d != DecisionApproved {
		t.Fatalf("got %v", d)
	}
}

func TestAskBlocksUntilDenied(t *testing.T) {
	g := NewGate(ModeAsk)

	done := make(chan struct{})
	var d Decision
	go func() {
		defer close(done)
		d, _ = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "shutdown"})
	}()

	var req Request
	select {
	case req = <-g.Pending():
	case <-time.After(time.Second):
		t.Fatal("no pending request surfaced")
	}
	if err := g.Decide(req.ID, DecisionDenied); err != nil {
		t.Fatal(err)
	}
	<-done
	if d != DecisionDenied {
		t.Fatalf("got %v", d)
	}
}

func TestAskCancel(t *testing.T) {
	g := NewGate(ModeAsk)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Check(ctx, Request{ToolName: "execute_command", Command: "ls"})
	}()

	var req Request
	select {
	case req = <-g.Pending():
	case <-time.After(time.Second):
		t.Fatal("no pending request surfaced")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Check did not unblock on cancel")
	}

	if err := g.Decide(req.ID, DecisionApproved); err == nil {
		t.Fatal("expected error deciding a cancelled request")
	}
}

func TestOneWayRule(t *testing.T) {
	g := NewGate(ModeAllowAll)
	if err := g.ApplyModelMode(ModeAsk); err != nil {
		t.Fatalf("tighten should succeed: %v", err)
	}
	if g.Mode() != ModeAsk {
		t.Fatal("mode not tightened")
	}
	if err := g.ApplyModelMode(ModeAllowAll); !errors.Is(err, ErrLoosen) {
		t.Fatalf("loosen should fail with ErrLoosen, got %v", err)
	}

	// The user may loosen or tighten freely.
	g.SetMode(ModeAllowAll)
	if g.Mode() != ModeAllowAll {
		t.Fatal("user loosen failed")
	}
	g.SetMode(ModeAsk)
	if g.Mode() != ModeAsk {
		t.Fatal("user tighten failed")
	}
}

func TestDecideUnknownID(t *testing.T) {
	g := NewGate(ModeAsk)
	if err := g.Decide("nope", DecisionApproved); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecideIsOneShot(t *testing.T) {
	g := NewGate(ModeAsk)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "ls"})
	}()

	req := <-g.Pending()
	if err := g.Decide(req.ID, DecisionApproved); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := g.Decide(req.ID, DecisionApproved); err == nil {
		t.Fatal("expected a completed request to be removed")
	}
}

func TestPendingRequestsIsNonDestructive(t *testing.T) {
	g := NewGate(ModeAsk)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "ls"})
	}()

	req := <-g.Pending()
	snapshot := g.PendingRequests()
	if len(snapshot) != 1 || snapshot[0] != req {
		t.Fatalf("PendingRequests() = %#v, want %#v", snapshot, req)
	}
	if err := g.Decide(req.ID, DecisionDenied); err != nil {
		t.Fatal(err)
	}
	<-done
	if got := g.PendingRequests(); len(got) != 0 {
		t.Fatalf("PendingRequests() after decision = %#v", got)
	}
}

func TestApproveAllRequiresPendingRequest(t *testing.T) {
	g := NewGate(ModeAsk)
	if err := g.ApproveAll("missing"); err == nil {
		t.Fatal("expected unknown request error")
	}
	if g.Mode() != ModeAsk {
		t.Fatal("unknown request loosened approval mode")
	}

	done := make(chan Decision, 1)
	go func() {
		d, _ := g.Check(context.Background(), Request{ToolName: "execute_command", Command: "ls"})
		done <- d
	}()
	req := <-g.Pending()
	if err := g.ApproveAll(req.ID); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got != DecisionApproved {
		t.Fatalf("decision = %v", got)
	}
	if g.Mode() != ModeAllowAll {
		t.Fatal("mode was not changed to allow-all")
	}
}

func TestHeadlessApprovalsDoNotFillNotificationChannel(t *testing.T) {
	g := NewGate(ModeAsk)
	defer g.Close()

	for i := 0; i < 24; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "true"})
		}()

		deadline := time.Now().Add(time.Second)
		var requests []Request
		for len(requests) == 0 && time.Now().Before(deadline) {
			requests = g.PendingRequests()
			if len(requests) == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		if len(requests) != 1 {
			t.Fatalf("iteration %d: pending = %#v", i, requests)
		}
		if err := g.Decide(requests[0].ID, DecisionApproved); err != nil {
			t.Fatal(err)
		}
		<-done
	}
}

func TestCloseUnblocksPending(t *testing.T) {
	g := NewGate(ModeAsk)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Check(context.Background(), Request{ToolName: "execute_command", Command: "ls"})
	}()

	select {
	case <-g.Pending():
	case <-time.After(time.Second):
		t.Fatal("no pending request surfaced")
	}

	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Check did not unblock on Close")
	}

	if _, err := g.Check(context.Background(), Request{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestModeString(t *testing.T) {
	if ModeAsk.String() != "ask" {
		t.Fatalf("ask = %q", ModeAsk.String())
	}
	if ModeAllowAll.String() != "allow-all" {
		t.Fatalf("allow-all = %q", ModeAllowAll.String())
	}
}
