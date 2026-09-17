package agent

import (
	"context"
	"sync"
	"time"
)

// responseBudget applies consecutive limits to one provider stream. The first
// covers request setup and thinking; the second starts at the first answer or
// tool-call delta. A generation check prevents a stopped timer from cancelling
// the next phase when its callback was already queued.
type responseBudget struct {
	mu         sync.Mutex
	cancel     context.CancelFunc
	timer      *time.Timer
	generation uint64
	phase      string
	duration   time.Duration
	expired    string
	expiredFor time.Duration
}

func newResponseBudget(cancel context.CancelFunc, thinking time.Duration) *responseBudget {
	b := &responseBudget{cancel: cancel}
	b.start("thinking", thinking)
	return b
}

func (b *responseBudget) start(phase string, duration time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.generation++
	b.phase, b.duration = phase, duration
	if duration <= 0 {
		b.timer = nil
		return
	}
	gen := b.generation
	b.timer = time.AfterFunc(duration, func() {
		b.mu.Lock()
		if b.generation == gen && b.expired == "" {
			b.expired, b.expiredFor = phase, duration
			b.cancel()
		}
		b.mu.Unlock()
	})
}

func (b *responseBudget) response(duration time.Duration) {
	b.mu.Lock()
	phase := b.phase
	b.mu.Unlock()
	if phase == "thinking" {
		b.start("response", duration)
	}
}

func (b *responseBudget) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
}

func (b *responseBudget) timeout() (string, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expired, b.expiredFor
}

func (b *responseBudget) current() (string, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.phase, b.duration
}
