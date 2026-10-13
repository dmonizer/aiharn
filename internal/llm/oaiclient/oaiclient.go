// Package oaiclient provides the shared go-openai client construction used by
// the chat-completions and responses adapters.
package oaiclient

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// NewClient returns a go-openai client for baseURL (the API root; empty keeps
// the SDK default) and apiKey. The base URL is right-trimmed of trailing
// slashes so the SDK's path concatenation never doubles them.
func NewClient(baseURL, apiKey string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = strings.TrimRight(baseURL, "/")
	}
	return openai.NewClientWithConfig(cfg)
}
