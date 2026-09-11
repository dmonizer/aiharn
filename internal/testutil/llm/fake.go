// Package llm provides reusable test doubles for the llm.Client contract.
package llm

import (
	"context"
	"sync"

	"aiharn/internal/llm"
)

// FakeClient is a scripted llm.Client. Each Stream call consumes the next entry
// of Script (a sequence of events) and records the request. SetupErr, when set,
// is returned synchronously. An empty or exhausted Script emits no events.
type FakeClient struct {
	Script   [][]llm.Event
	SetupErr error

	mu       sync.Mutex
	requests []llm.Request
}

// Stream implements llm.Client.
func (f *FakeClient) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if f.SetupErr != nil {
		return nil, f.SetupErr
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	var events []llm.Event
	if len(f.Script) > 0 {
		events = f.Script[0]
		f.Script = f.Script[1:]
	}
	f.mu.Unlock()

	out := make(chan llm.Event, len(events)+1)
	go func() {
		defer close(out)
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Requests returns a copy of the recorded requests, in call order.
func (f *FakeClient) Requests() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.requests...)
}
