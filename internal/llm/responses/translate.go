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
	input := make([]any, 0, len(req.Input))
	for i, it := range req.Input {
		switch it.Type {
		case llm.ItemMessage:
			if it.Role != llm.RoleUser && it.Role != llm.RoleAssistant {
				return openai.CreateResponseRequest{}, fmt.Errorf("input[%d]: invalid message role %q", i, it.Role)
			}
			input = append(input, openai.ResponseInputMessage{
				Role:    string(it.Role),
				Content: it.Content,
			})
		case llm.ItemFunctionCall:
			if it.CallID == "" || it.Name == "" {
				return openai.CreateResponseRequest{}, fmt.Errorf("input[%d]: function call requires call id and name", i)
			}
			input = append(input, openai.ResponseOutputItem{
				Type:      "function_call",
				CallID:    it.CallID,
				Name:      it.Name,
				Arguments: it.Args,
			})
		case llm.ItemFunctionCallOutput:
			if it.CallID == "" {
				return openai.CreateResponseRequest{}, fmt.Errorf("input[%d]: function call output requires call id", i)
			}
			input = append(input, openai.ResponseFunctionCallOutput{
				Type:   "function_call_output",
				CallID: it.CallID,
				Output: it.Content,
			})
		default:
			return openai.CreateResponseRequest{}, fmt.Errorf("input[%d]: unsupported item type %q", i, it.Type)
		}
	}

	tools := make([]openai.ResponseTool, 0, len(req.Tools))
	for i, td := range req.Tools {
		if td.Name == "" {
			return openai.CreateResponseRequest{}, fmt.Errorf("tool[%d]: name is required", i)
		}
		tools = append(tools, openai.NewResponseFunctionTool(
			openai.FunctionDefinition{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.Parameters,
			},
		))
	}

	return openai.CreateResponseRequest{
		Model:        req.Model,
		Instructions: req.System,
		Input:        input,
		Tools:        tools,
		Stream:       true,
	}, nil
}

// translateOutputItems converts the SDK's response output into normalized Items.
// It reconstructs assistant message text from output_text content parts and
// carries function calls through with their call ids.
func translateOutputItems(output []any) ([]llm.Item, error) {
	items := make([]llm.Item, 0, len(output))
	for i, raw := range output {
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal output item %d: %w", i, err)
		}
		var item openai.ResponseOutputItem
		if err := json.Unmarshal(data, &item); err != nil {
			return nil, fmt.Errorf("decode output item %d: %w", i, err)
		}
		switch item.Type {
		case "message":
			var sb strings.Builder
			for _, c := range item.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
				} else if c.Type == "refusal" {
					sb.WriteString(c.Refusal)
				}
			}
			if sb.Len() > 0 {
				items = append(items, llm.Item{
					Type:    llm.ItemMessage,
					Role:    llm.RoleAssistant,
					Content: sb.String(),
				})
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("output item %d: function call requires call_id and name", i)
			}
			items = append(items, llm.Item{
				Type:   llm.ItemFunctionCall,
				CallID: item.CallID,
				Name:   item.Name,
				Args:   item.Arguments,
			})
		}
	}
	return items, nil
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
