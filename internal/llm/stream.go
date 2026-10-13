package llm

import "context"

// StreamBuffer bounds the number of buffered stream events. A full channel
// blocks the producer, giving the consumer backpressure.
const StreamBuffer = 16

// Emit sends e on out, or reports false if ctx was cancelled first.
func Emit(ctx context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// StartStream runs collect in a goroutine that closes out and calls closeFn
// (typically the SDK stream's Close) when collect returns. It returns the
// buffered event channel.
func StartStream(ctx context.Context, closeFn func() error, collect func(context.Context, chan<- Event)) <-chan Event {
	out := make(chan Event, StreamBuffer)
	go func() {
		defer close(out)
		defer func() { _ = closeFn() }()
		collect(ctx, out)
	}()
	return out
}
