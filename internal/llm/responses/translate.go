package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
)

// buildRequest converts a normalized llm.Request into the SDK's Responses
// request. Complete stateless history is sent on every call (no
// previous_response_id), for portability across OpenAI-compatible providers.
func buildRequest(req llm.Request) (openai.CreateResponseRequest, error) {
	input, err := buildInput(req.Input)
	if err != nil {
		return openai.CreateResponseRequest{}, err
	}
	tools, err := buildTools(req.Tools)
	if err != nil {
		return openai.CreateResponseRequest{}, err
	}
	return openai.CreateResponseRequest{
		Model:        req.Model,
		Instructions: req.System,
		Input:        input,
		Tools:        tools,
		Stream:       true,
		Reasoning:    buildReasoning(req.ReasoningEffort, req.ReasoningSummary),
	}, nil
}

// buildInput converts normalized input items into SDK Responses input values.
func buildInput(input []llm.Item) ([]any, error) {
	out := make([]any, 0, len(input))
	for i, it := range input {
		item, err := translateInputItem(i, it)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// translateInputItem converts one normalized input item into an SDK Responses
// input value.
func translateInputItem(i int, it llm.Item) (any, error) {
	switch it.Type {
	case llm.ItemMessage:
		return translateInputMessage(i, it)
	case llm.ItemFunctionCall:
		return translateInputFunctionCall(i, it)
	case llm.ItemFunctionCallOutput:
		return translateInputFunctionCallOutput(i, it)
	default:
		return nil, fmt.Errorf("input[%d]: unsupported item type %q", i, it.Type)
	}
}

func translateInputMessage(i int, it llm.Item) (any, error) {
	if it.Role != llm.RoleUser && it.Role != llm.RoleAssistant {
		return nil, fmt.Errorf("input[%d]: invalid message role %q", i, it.Role)
	}
	return openai.ResponseInputMessage{Role: string(it.Role), Content: it.Content}, nil
}

func translateInputFunctionCall(i int, it llm.Item) (any, error) {
	if it.CallID == "" || it.Name == "" {
		return nil, fmt.Errorf("input[%d]: function call requires call id and name", i)
	}
	return openai.ResponseOutputItem{
		Type:      "function_call",
		CallID:    it.CallID,
		Name:      it.Name,
		Arguments: it.Args,
	}, nil
}

func translateInputFunctionCallOutput(i int, it llm.Item) (any, error) {
	if it.CallID == "" {
		return nil, fmt.Errorf("input[%d]: function call output requires call id", i)
	}
	return openai.ResponseFunctionCallOutput{
		Type:   "function_call_output",
		CallID: it.CallID,
		Output: it.Content,
	}, nil
}

// buildTools converts normalized tool definitions into SDK Responses tools.
func buildTools(tools []llm.ToolDefinition) ([]openai.ResponseTool, error) {
	out := make([]openai.ResponseTool, 0, len(tools))
	for i, td := range tools {
		if td.Name == "" {
			return nil, fmt.Errorf("tool[%d]: name is required", i)
		}
		out = append(out, openai.NewResponseFunctionTool(
			openai.FunctionDefinition{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.Parameters,
			},
		))
	}
	return out, nil
}

// buildReasoning returns the SDK reasoning config, or nil when neither effort
// nor summary is set.
func buildReasoning(effort, summary string) *openai.ResponseReasoning {
	if effort != "" || summary != "" {
		return &openai.ResponseReasoning{Effort: effort, Summary: summary}
	}
	return nil
}

// translateOutputItems converts the SDK's response output into normalized Items.
// It reconstructs assistant message text from output_text content parts and
// carries function calls through with their call ids.
func translateOutputItems(output []any) ([]llm.Item, error) {
	items := make([]llm.Item, 0, len(output))
	for i, raw := range output {
		item, err := decodeOutputItem(i, raw)
		if err != nil {
			return nil, err
		}
		translated, err := translateOutputItem(i, item)
		if err != nil {
			return nil, err
		}
		items = append(items, translated...)
	}
	return items, nil
}

// decodeOutputItem marshals and re-decodes one raw SDK output value into the
// SDK's typed output item struct.
func decodeOutputItem(i int, raw any) (openai.ResponseOutputItem, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return openai.ResponseOutputItem{}, fmt.Errorf("marshal output item %d: %w", i, err)
	}
	var item openai.ResponseOutputItem
	if err := json.Unmarshal(data, &item); err != nil {
		return openai.ResponseOutputItem{}, fmt.Errorf("decode output item %d: %w", i, err)
	}
	return item, nil
}

// translateOutputItem converts one decoded output item into normalized Items.
func translateOutputItem(i int, item openai.ResponseOutputItem) ([]llm.Item, error) {
	switch item.Type {
	case "message":
		return translateOutputMessage(item), nil
	case "function_call":
		return translateOutputFunctionCall(i, item)
	default:
		return nil, nil
	}
}

// translateOutputMessage reconstructs assistant text from an output message's
// output_text and refusal content parts.
func translateOutputMessage(item openai.ResponseOutputItem) []llm.Item {
	var sb strings.Builder
	for _, c := range item.Content {
		switch c.Type {
		case "output_text":
			sb.WriteString(c.Text)
		case "refusal":
			sb.WriteString(c.Refusal)
		}
	}
	if sb.Len() == 0 {
		return nil
	}
	return []llm.Item{{
		Type:    llm.ItemMessage,
		Role:    llm.RoleAssistant,
		Content: sb.String(),
	}}
}

// translateOutputFunctionCall converts a function_call output item, validating
// its required fields.
func translateOutputFunctionCall(i int, item openai.ResponseOutputItem) ([]llm.Item, error) {
	if item.CallID == "" || item.Name == "" {
		return nil, fmt.Errorf("output item %d: function call requires call_id and name", i)
	}
	return []llm.Item{{
		Type:   llm.ItemFunctionCall,
		CallID: item.CallID,
		Name:   item.Name,
		Args:   item.Arguments,
	}}, nil
}

// usage converts SDK usage into the normalized Usage (nil-safe).
func usage(u *openai.ResponseUsage) llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	return llm.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
}

// finishReason derives a normalized finish reason from a response status.
func finishReason(r *openai.CreateResponseResponse) string {
	if r == nil {
		return ""
	}
	switch r.Status {
	case openai.ResponseStatusIncomplete:
		if r.IncompleteDetails != nil {
			return r.IncompleteDetails.Reason
		}
		return "incomplete"
	case openai.ResponseStatusCompleted:
		return "stop"
	default:
		return string(r.Status)
	}
}
