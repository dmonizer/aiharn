// Package chatcompletions adapts OpenAI-compatible Chat Completions APIs to
// Aiharn's provider-neutral llm.Client contract.
package chatcompletions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
	"aiharn/internal/llm/oaiclient"
)

// Adapter implements llm.Client over an OpenAI-compatible Chat Completions API.
type Adapter struct {
	client *openai.Client
}

// NewAdapter returns an Adapter whose base URL is the API root. The SDK appends
// /chat/completions to this URL.
func NewAdapter(baseURL, apiKey string) *Adapter {
	return &Adapter{client: oaiclient.NewClient(baseURL, apiKey)}
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

	return llm.StartStream(ctx, func() { _ = stream.Close() }, func(ctx context.Context, out chan<- llm.Event) {
		collectStream(ctx, stream, out)
	}), nil
}

type callBuilder struct {
	id   string
	name string
	args strings.Builder
}

// streamChunk is a chat-completions stream event decoded leniently. The
// go-openai SDK types delta content as a plain string, but some providers (for
// example Mistral) send content as an array of content parts. Decoding into
// json.RawMessage lets us accept both shapes without losing the SDK's
// request-side types.
type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *openai.Usage  `json:"usage"`
}

type streamChoice struct {
	Index        int             `json:"index"`
	Delta        streamDelta     `json:"delta"`
	FinishReason json.RawMessage `json:"finish_reason"`
}

type streamDelta struct {
	Content          json.RawMessage   `json:"content"`
	ReasoningContent json.RawMessage   `json:"reasoning_content"`
	Refusal          json.RawMessage   `json:"refusal"`
	ToolCalls        []openai.ToolCall `json:"tool_calls"`
}

// streamText decodes a delta text field. Providers either send a plain JSON
// string or an array of content parts (for example
// [{"type":"text","text":"..."}]). Absent and null decode to the empty string;
// any other shape is an error.
func streamText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range parts {
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

func streamFinishReason(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var reason string
	if err := json.Unmarshal(raw, &reason); err != nil {
		return ""
	}
	return reason
}

func collectStream(ctx context.Context, stream *openai.ChatCompletionStream, out chan<- llm.Event) {
	var text strings.Builder
	calls := make(map[int]*callBuilder)
	finish := ""
	sawChoice := false
	usage := llm.Usage{}

	for {
		raw, err := stream.RecvRaw()
		if err != nil {
			handleStreamError(ctx, out, err, text.String(), calls, sawChoice, finish, usage)
			return
		}

		var chunk streamChunk
		if err := json.Unmarshal(raw, &chunk); err != nil {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: stream: %w", err)})
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
			if !processChoice(ctx, out, choice, &text, calls, &finish, &sawChoice) {
				return
			}
		}
	}
}

// handleStreamError emits the terminal event for a stream receive error. It
// only reads accumulated state; collectStream always returns immediately after
// calling it.
func handleStreamError(ctx context.Context, out chan<- llm.Event, err error, text string, calls map[int]*callBuilder, sawChoice bool, finish string, usage llm.Usage) {
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, io.EOF):
		if !sawChoice || finish == "" {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: errors.New("chat completions: stream ended before a finish reason")})
			return
		}
		items, itemErr := completedItems(text, calls)
		if itemErr != nil {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: itemErr})
			return
		}
		llm.Emit(ctx, out, llm.Event{Type: llm.EventCompleted, Items: items, Usage: usage, FinishReason: normalizeFinishReason(finish)})
	default:
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: stream: %w", err)})
	}
}

// processChoice folds one streamed choice into the accumulated reply state.
// It reports false when the stream must stop (cancellation or fatal error).
func processChoice(ctx context.Context, out chan<- llm.Event, choice streamChoice, text *strings.Builder, calls map[int]*callBuilder, finish *string, sawChoice *bool) bool {
	if choice.Index != 0 {
		return true
	}
	*sawChoice = true

	if !streamReasoningDelta(ctx, out, choice.Delta.ReasoningContent) {
		return false
	}
	content, refusal, err := decodeContentAndRefusal(choice.Delta.Content, choice.Delta.Refusal)
	if err != nil {
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: stream: %w", err)})
		return false
	}
	if !emitTextDelta(ctx, out, content, text) {
		return false
	}
	if !emitTextDelta(ctx, out, refusal, text) {
		return false
	}
	if !streamToolCalls(ctx, out, choice.Delta.ToolCalls, calls) {
		return false
	}

	if reason := streamFinishReason(choice.FinishReason); reason != "" && reason != string(openai.FinishReasonNull) {
		*finish = reason
	}
	return true
}

// streamReasoningDelta decodes and emits a reasoning delta (not accumulated).
func streamReasoningDelta(ctx context.Context, out chan<- llm.Event, raw json.RawMessage) bool {
	delta, err := streamText(raw)
	if err != nil {
		llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: stream: %w", err)})
		return false
	}
	if delta == "" {
		return true
	}
	return llm.Emit(ctx, out, llm.Event{Type: llm.EventReasoningDelta, Text: delta})
}

// decodeContentAndRefusal decodes the content and refusal deltas together,
// mirroring the original ordering where both are validated before either is
// emitted.
func decodeContentAndRefusal(contentRaw, refusalRaw json.RawMessage) (string, string, error) {
	content, err := streamText(contentRaw)
	if err != nil {
		return "", "", err
	}
	refusal, err := streamText(refusalRaw)
	if err != nil {
		return "", "", err
	}
	return content, refusal, nil
}

// emitTextDelta appends a non-empty text delta to the reply and emits it.
func emitTextDelta(ctx context.Context, out chan<- llm.Event, delta string, text *strings.Builder) bool {
	if delta == "" {
		return true
	}
	text.WriteString(delta)
	return llm.Emit(ctx, out, llm.Event{Type: llm.EventTextDelta, Text: delta})
}

// streamToolCalls folds a delta's tool-call fragments into stable per-index
// call builders, emitting a response-started event for each call seen.
func streamToolCalls(ctx context.Context, out chan<- llm.Event, toolCalls []openai.ToolCall, calls map[int]*callBuilder) bool {
	for position, call := range toolCalls {
		if !llm.Emit(ctx, out, llm.Event{Type: llm.EventResponseStarted}) {
			return false
		}
		index := position
		if call.Index != nil {
			index = *call.Index
		}
		if index < 0 {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: fmt.Errorf("chat completions: negative tool-call index %d", index)})
			return false
		}
		builder := calls[index]
		if builder == nil {
			builder = &callBuilder{}
			calls[index] = builder
		}
		if err := mergeStableField(&builder.id, call.ID, "id", index); err != nil {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: err})
			return false
		}
		if err := mergeStableField(&builder.name, call.Function.Name, "name", index); err != nil {
			llm.Emit(ctx, out, llm.Event{Type: llm.EventFailed, Err: err})
			return false
		}
		builder.args.WriteString(call.Function.Arguments)
	}
	return true
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

func buildRequest(req llm.Request) (openai.ChatCompletionRequest, error) {
	messages, err := buildMessages(req)
	if err != nil {
		return openai.ChatCompletionRequest{}, err
	}
	tools, err := buildTools(req.Tools)
	if err != nil {
		return openai.ChatCompletionRequest{}, err
	}
	return openai.ChatCompletionRequest{
		Model: req.Model, Messages: messages, Tools: tools, Stream: true, ReasoningEffort: req.ReasoningEffort,
	}, nil
}

// buildMessages converts the normalized input (plus the system prompt) into
// SDK messages, grouping consecutive function calls into assistant messages.
func buildMessages(req llm.Request) ([]openai.ChatCompletionMessage, error) {
	messages := make([]openai.ChatCompletionMessage, 0, len(req.Input)+1)
	if req.System != "" {
		messages = append(messages, openai.ChatCompletionMessage{Role: "system", Content: req.System})
	}
	for i := 0; i < len(req.Input); {
		message, next, err := buildInputMessage(req.Input, i)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
		i = next
	}
	return messages, nil
}

// buildInputMessage converts the input item at index i into one SDK message and
// returns the index of the next unconsumed input item.
func buildInputMessage(input []llm.Item, i int) (openai.ChatCompletionMessage, int, error) {
	item := input[i]
	switch item.Type {
	case llm.ItemMessage:
		return buildMessageItem(input, i, item)
	case llm.ItemFunctionCall:
		return buildFunctionCallMessage(input, i)
	case llm.ItemFunctionCallOutput:
		return buildFunctionCallOutputMessage(i, item)
	default:
		return openai.ChatCompletionMessage{}, i, fmt.Errorf("input[%d]: unsupported item type %q", i, item.Type)
	}
}

func buildMessageItem(input []llm.Item, i int, item llm.Item) (openai.ChatCompletionMessage, int, error) {
	if item.Role != llm.RoleUser && item.Role != llm.RoleAssistant {
		return openai.ChatCompletionMessage{}, i, fmt.Errorf("input[%d]: invalid message role %q", i, item.Role)
	}
	message := openai.ChatCompletionMessage{Role: string(item.Role), Content: item.Content}
	i++
	if item.Role != llm.RoleAssistant {
		return message, i, nil
	}
	var err error
	message.ToolCalls, i, err = collectInputCalls(input, i)
	if err != nil {
		return openai.ChatCompletionMessage{}, i, err
	}
	return message, i, nil
}

func buildFunctionCallMessage(input []llm.Item, i int) (openai.ChatCompletionMessage, int, error) {
	calls, next, err := collectInputCalls(input, i)
	if err != nil {
		return openai.ChatCompletionMessage{}, i, err
	}
	return openai.ChatCompletionMessage{Role: "assistant", ToolCalls: calls}, next, nil
}

func buildFunctionCallOutputMessage(i int, item llm.Item) (openai.ChatCompletionMessage, int, error) {
	if item.CallID == "" {
		return openai.ChatCompletionMessage{}, i, fmt.Errorf("input[%d]: function call output requires call id", i)
	}
	return openai.ChatCompletionMessage{Role: "tool", ToolCallID: item.CallID, Content: item.Content}, i + 1, nil
}

func buildTools(tools []llm.ToolDefinition) ([]openai.Tool, error) {
	definitions := make([]openai.Tool, 0, len(tools))
	for i, tool := range tools {
		if tool.Name == "" {
			return nil, fmt.Errorf("tool[%d]: name is required", i)
		}
		definitions = append(definitions, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
			},
		})
	}
	return definitions, nil
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
