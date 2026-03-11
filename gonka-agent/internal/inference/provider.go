// Package inference provides a multi-provider inference router with
// circuit breakers and automatic failover. Providers are tried in
// priority order; if one fails, the next is attempted transparently.
package inference

import (
	"context"
	"encoding/json"
	"time"
)

// Message mirrors the OpenAI chat message format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type ChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error    json.RawMessage `json:"error"`
	Provider string          `json:"-"` // filled by router: which provider answered
}

// Provider is the interface every inference backend must implement.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Ping(ctx context.Context) error
}

// ProviderConfig is parsed from INFER_PROVIDER_N env vars or
// convenience vars like OPENROUTER_API_KEY.
type ProviderConfig struct {
	Type    string // "openrouter", "gonka", "ollama", "openai-compat"
	BaseURL string
	APIKey  string
	Model   string
	Keys    []string // optional multi-key pool (gonka-specific)
}

// Metrics tracks per-provider performance.
type Metrics struct {
	Calls       int64
	Failures    int64
	TotalMs     int64
	LastLatency time.Duration
}
