// Package responses adapts go-openai's Responses API to Aiharn's
// provider-neutral llm.Client. It is the only package in the project that
// imports the SDK.
package responses

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
)

// streamBuffer bounds the number of buffered stream events. A full channel
// blocks the producer, giving the consumer backpressure.
const streamBuffer = 16

// Adapter implements llm.Client over the OpenAI Responses API.
type Adapter struct {
	client *openai.Client
}

// NewAdapter returns an Adapter talking to baseURL (an OpenAI-compatible
// Responses endpoint, e.g. https://api.openai.com/v1 or OpenRouter's
// /api/v1). apiKey is used verbatim.
func NewAdapter(baseURL, apiKey string) *Adapter {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = strings.TrimRight(baseURL, "/")
	}
	return &Adapter{client: openai.NewClientWithConfig(cfg)}
}

// Stream implements llm.Client.
func (a *Adapter) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	sdkReq, err := buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("responses: build request: %w", err)
	}

	stream, err := a.client.CreateResponseStream(ctx, sdkReq)
	if err != nil {
		return nil, fmt.Errorf("responses: create stream: %w", err)
	}

	out := make(chan llm.Event, streamBuffer)
	go func() {
		defer close(out)
		defer stream.Close()

		for {
			evt, err := stream.Recv()
			if err != nil {
				switch {
				case errors.Is(err, io.EOF):
					// Stream ended without a terminal event: a protocol error.
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("responses: stream ended without a terminal event")})
				case ctx.Err() != nil:
					// Cancellation: close without a terminal event.
					return
				default:
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("responses: stream: %w", err)})
				}
				return
			}

			switch evt.Type {
			case openai.ResponseStreamEventOutputTextDelta:
				if !emit(ctx, out, llm.Event{Type: llm.EventTextDelta, Text: evt.Delta}) {
					return
				}
			case openai.ResponseStreamEventCompleted:
				if evt.Response == nil {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("responses: completed event has no response")})
					return
				}
				items, err := translateOutputItems(responseOutput(evt))
				if err != nil {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("responses: translate output: %w", err)})
					return
				}
				emit(ctx, out, llm.Event{
					Type:         llm.EventCompleted,
					Items:        items,
					Usage:        usage(responseUsage(evt)),
					FinishReason: finishReason(evt.Response),
				})
				return
			case openai.ResponseStreamEventFailed:
				emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: responseError(evt)})
				return
			case openai.ResponseStreamEventError:
				emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: responseError(evt)})
				return
			}
		}
	}()
	return out, nil
}

// responseOutput returns the response output for a completed event (nil-safe).
func responseOutput(evt openai.ResponseStreamEvent) []any {
	if evt.Response == nil {
		return nil
	}
	return evt.Response.Output
}

func responseUsage(evt openai.ResponseStreamEvent) *openai.ResponseUsage {
	if evt.Response == nil {
		return nil
	}
	return evt.Response.Usage
}

// responseError builds an error from a failed/error stream event. It never
// includes raw secret-bearing fields (provider error messages may contain
// request context; we surface only the code/message the provider returned).
func responseError(evt openai.ResponseStreamEvent) error {
	if evt.Response != nil && evt.Response.Error != nil {
		return fmt.Errorf("responses: %s: %s", evt.Response.Error.Code, evt.Response.Error.Message)
	}
	if evt.Error != nil {
		return fmt.Errorf("responses: %s: %s", evt.Error.Code, evt.Error.Message)
	}
	if evt.Message != "" {
		return fmt.Errorf("responses: %s", evt.Message)
	}
	return errors.New("responses: unknown failure")
}

// emit sends e on out, or reports false if ctx was cancelled first.
func emit(ctx context.Context, out chan<- llm.Event, e llm.Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}
