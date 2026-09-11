package responses

import (
	"encoding/json"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"aiharn/internal/llm"
)

// buildRequest converts a normalized llm.Request into the SDK's Responses
// request. Complete stateless history is sent on every call (no
// previous_response_id), for portability across OpenAI-compatible providers.
func buildRequest(req llm.Request) (openai.CreateResponseRequest, error) {
	input := make([]any, 0, len(req.Input))
	for _, it := range req.Input {
		switch it.Type {
		case llm.ItemMessage:
			input = append(input, openai.ResponseInputMessage{
				Role:    string(it.Role),
				Content: it.Content,
			})
		case llm.ItemFunctionCall:
			input = append(input, openai.ResponseOutputItem{
				Type:      "function_call",
				CallID:    it.CallID,
				Name:      it.Name,
				Arguments: it.Args,
			})
		case llm.ItemFunctionCallOutput:
			input = append(input, openai.ResponseFunctionCallOutput{
				Type:   "function_call_output",
				CallID: it.CallID,
				Output: it.Content,
			})
		}
	}

	tools := make([]openai.ResponseTool, 0, len(req.Tools))
	for _, td := range req.Tools {
		tools = append(tools, openai.ResponseTool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.Parameters,
			},
		})
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
func translateOutputItems(output []any) []llm.Item {
	items := make([]llm.Item, 0, len(output))
	for _, raw := range output {
		data, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var item openai.ResponseOutputItem
		if err := json.Unmarshal(data, &item); err != nil {
			continue
		}
		switch item.Type {
		case "message":
			var sb strings.Builder
			for _, c := range item.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
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
			items = append(items, llm.Item{
				Type:   llm.ItemFunctionCall,
				CallID: item.CallID,
				Name:   item.Name,
				Args:   item.Arguments,
			})
		}
	}
	return items
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
