// Package chatcompletions adapts OpenAI-compatible Chat Completions APIs to
// Aiharn's provider-neutral llm.Client contract.
package chatcompletions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
)

const streamBuffer = 16

// Adapter implements llm.Client over an OpenAI-compatible Chat Completions API.
type Adapter struct {
	client *openai.Client
}

// NewAdapter returns an Adapter whose base URL is the API root. The SDK appends
// /chat/completions to this URL.
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
		return nil, fmt.Errorf("chat completions: build request: %w", err)
	}
	stream, err := a.client.CreateChatCompletionStream(ctx, sdkReq)
	if err != nil {
		return nil, fmt.Errorf("chat completions: create stream: %w", err)
	}

	out := make(chan llm.Event, streamBuffer)
	go func() {
		defer close(out)
		defer stream.Close()
		collectStream(ctx, stream, out)
	}()
	return out, nil
}

type callBuilder struct {
	id   string
	name string
	args strings.Builder
}

func collectStream(ctx context.Context, stream *openai.ChatCompletionStream, out chan<- llm.Event) {
	var text strings.Builder
	calls := make(map[int]*callBuilder)
	finish := ""
	sawChoice := false
	usage := llm.Usage{}

	for {
		chunk, err := stream.Recv()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, io.EOF):
				if !sawChoice || finish == "" {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("chat completions: stream ended before a finish reason")})
					return
				}
				items, itemErr := completedItems(text.String(), calls)
				if itemErr != nil {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: itemErr})
					return
				}
				emit(ctx, out, llm.Event{Type: llm.EventCompleted, Items: items, Usage: usage, FinishReason: normalizeFinishReason(finish)})
			default:
				emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: stream: %w", err)})
			}
			return
		}

		if chunk.Usage != nil {
			usage = llm.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			sawChoice = true
			if choice.Delta.ReasoningContent != "" {
				if !emit(ctx, out, llm.Event{Type: llm.EventReasoningDelta, Text: choice.Delta.ReasoningContent}) {
					return
				}
			}
			for _, delta := range []string{choice.Delta.Content, choice.Delta.Refusal} {
				if delta != "" {
					text.WriteString(delta)
					if !emit(ctx, out, llm.Event{Type: llm.EventTextDelta, Text: delta}) {
						return
					}
				}
			}
			for position, call := range choice.Delta.ToolCalls {
				index := position
				if call.Index != nil {
					index = *call.Index
				}
				if index < 0 {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: negative tool-call index %d", index)})
					return
				}
				builder := calls[index]
				if builder == nil {
					builder = &callBuilder{}
					calls[index] = builder
				}
				if err := mergeStableField(&builder.id, call.ID, "id", index); err != nil {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: err})
					return
				}
				if err := mergeStableField(&builder.name, call.Function.Name, "name", index); err != nil {
					emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: err})
					return
				}
				builder.args.WriteString(call.Function.Arguments)
			}
			if choice.FinishReason != "" && choice.FinishReason != openai.FinishReasonNull {
				finish = string(choice.FinishReason)
			}
		}
	}
}

func mergeStableField(dst *string, next, field string, index int) error {
	if next == "" {
		return nil
	}
	if *dst != "" && *dst != next {
		return fmt.Errorf("chat completions: tool call %d changed %s during stream", index, field)
	}
	*dst = next
	return nil
}

func completedItems(content string, calls map[int]*callBuilder) ([]llm.Item, error) {
	items := make([]llm.Item, 0, 1+len(calls))
	if content != "" {
		items = append(items, llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: content})
	}
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := calls[index]
		if call.id == "" || call.name == "" {
			return nil, fmt.Errorf("chat completions: tool call %d requires id and name", index)
		}
		items = append(items, llm.Item{
			Type: llm.ItemFunctionCall, CallID: call.id, Name: call.name, Args: call.args.String(),
		})
	}
	return items, nil
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case string(openai.FinishReasonToolCalls), string(openai.FinishReasonFunctionCall):
		return "stop"
	default:
		return reason
	}
}

func emit(ctx context.Context, out chan<- llm.Event, event llm.Event) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func buildRequest(req llm.Request) (openai.ChatCompletionRequest, error) {
	messages := make([]openai.ChatCompletionMessage, 0, len(req.Input)+1)
	if req.System != "" {
		messages = append(messages, openai.ChatCompletionMessage{Role: "system", Content: req.System})
	}

	for i := 0; i < len(req.Input); {
		item := req.Input[i]
		switch item.Type {
		case llm.ItemMessage:
			if item.Role != llm.RoleUser && item.Role != llm.RoleAssistant {
				return openai.ChatCompletionRequest{}, fmt.Errorf("input[%d]: invalid message role %q", i, item.Role)
			}
			message := openai.ChatCompletionMessage{Role: string(item.Role), Content: item.Content}
			i++
			if item.Role == llm.RoleAssistant {
				var err error
				message.ToolCalls, i, err = collectInputCalls(req.Input, i)
				if err != nil {
					return openai.ChatCompletionRequest{}, err
				}
			}
			messages = append(messages, message)
		case llm.ItemFunctionCall:
			calls, next, err := collectInputCalls(req.Input, i)
			if err != nil {
				return openai.ChatCompletionRequest{}, err
			}
			messages = append(messages, openai.ChatCompletionMessage{Role: "assistant", ToolCalls: calls})
			i = next
		case llm.ItemFunctionCallOutput:
			if item.CallID == "" {
				return openai.ChatCompletionRequest{}, fmt.Errorf("input[%d]: function call output requires call id", i)
			}
			messages = append(messages, openai.ChatCompletionMessage{Role: "tool", ToolCallID: item.CallID, Content: item.Content})
			i++
		default:
			return openai.ChatCompletionRequest{}, fmt.Errorf("input[%d]: unsupported item type %q", i, item.Type)
		}
	}

	definitions := make([]openai.Tool, 0, len(req.Tools))
	for i, tool := range req.Tools {
		if tool.Name == "" {
			return openai.ChatCompletionRequest{}, fmt.Errorf("tool[%d]: name is required", i)
		}
		definitions = append(definitions, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
			},
		})
	}

	return openai.ChatCompletionRequest{
		Model: req.Model, Messages: messages, Tools: definitions, Stream: true, ReasoningEffort: req.ReasoningEffort,
	}, nil
}

func collectInputCalls(input []llm.Item, start int) ([]openai.ToolCall, int, error) {
	calls := make([]openai.ToolCall, 0)
	i := start
	for i < len(input) && input[i].Type == llm.ItemFunctionCall {
		item := input[i]
		if item.CallID == "" || item.Name == "" {
			return nil, i, fmt.Errorf("input[%d]: function call requires call id and name", i)
		}
		calls = append(calls, openai.ToolCall{
			ID: item.CallID, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: item.Name, Arguments: item.Args},
		})
		i++
	}
	return calls, i, nil
}
