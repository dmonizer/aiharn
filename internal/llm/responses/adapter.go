// Package responses adapts go-openai's Responses API to Aiharn's
// provider-neutral llm.Client. It is the only package in the project that
// imports the SDK.
package responses

import (
	"context"
	"errors"
	"fmt"
	"io"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
	"aiharn/internal/llm/oaiclient"
)

// Adapter implements llm.Client over the OpenAI Responses API.
type Adapter struct {
	client *openai.Client
}

// NewAdapter returns an Adapter talking to baseURL (an OpenAI-compatible
// Responses endpoint, e.g. https://api.openai.com/v1 or OpenRouter's
// /api/v1). apiKey is used verbatim.
func NewAdapter(baseURL, apiKey string) *Adapter {
	return &Adapter{client: oaiclient.NewClient(baseURL, apiKey)}
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

	return llm.StartStream(ctx, stream.Close, func(ctx context.Context, out chan<- llm.Event) {
		collectStream(ctx, stream, out)
	}), nil
}

// collectStream drains the Responses stream, translating each event into the
// provider-neutral event model until a terminal event or cancellation.
func collectStream(ctx context.Context, stream *openai.ResponseStream, out chan<- llm.Event) {
	for {
		evt, err := stream.Recv()
		if err != nil {
			handleStreamError(ctx, out, err)
			return
		}
		if !handleStreamEvent(ctx, out, evt) {
			return
		}
	}
}

// handleStreamError emits the terminal event for a stream receive error. A
// context cancellation terminates without emitting a terminal event.
func handleStreamError(ctx context.Context, out chan<- llm.Event, err error) {
	switch {
	case errors.Is(err, io.EOF):
		// Stream ended without a terminal event: a protocol error.
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("responses: stream ended without a terminal event")})
	case ctx.Err() != nil:
		// Cancellation: close without a terminal event.
		return
	default:
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("responses: stream: %w", err)})
	}
}

// handleStreamEvent translates one stream event, reporting false when the
// stream must stop (a terminal event or cancellation).
func handleStreamEvent(ctx context.Context, out chan<- llm.Event, evt openai.ResponseStreamEvent) bool {
	switch evt.Type {
	case openai.ResponseStreamEventReasoningSummaryTextDelta, openai.ResponseStreamEventReasoningTextDelta:
		return llm.Emit(ctx, out, llm.Event{Type: llm.EventReasoningDelta, Text: evt.Delta})
	case openai.ResponseStreamEventOutputTextDelta:
		return llm.Emit(ctx, out, llm.Event{Type: llm.EventTextDelta, Text: evt.Delta})
	case openai.ResponseStreamEventFunctionArgumentsDelta,
		openai.ResponseStreamEventCustomToolInputDelta:
		return llm.Emit(ctx, out, llm.Event{Type: llm.EventResponseStarted})
	case openai.ResponseStreamEventCompleted:
		return handleCompletedEvent(ctx, out, evt)
	case openai.ResponseStreamEventFailed:
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: responseError(evt)})
		return false
	case openai.ResponseStreamEventError:
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: responseError(evt)})
		return false
	}
	return true
}

// handleCompletedEvent emits the completed event for a streamed response. It
// always terminates the stream.
func handleCompletedEvent(ctx context.Context, out chan<- llm.Event, evt openai.ResponseStreamEvent) bool {
	if evt.Response == nil {
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("responses: completed event has no response")})
		return false
	}
	items, err := translateOutputItems(responseOutput(evt))
	if err != nil {
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("responses: translate output: %w", err)})
		return false
	}
	llm.Emit(ctx, out, llm.Event{
		Type:         llm.EventCompleted,
		Items:        items,
		Usage:        usage(responseUsage(evt)),
		FinishReason: finishReason(evt.Response),
	})
	return false
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
