// Package providers implements concrete inference backends.
// All providers use the OpenAI-compatible /v1/chat/completions format.
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/inference"
)

// OpenAICompat is a generic provider for any OpenAI-compatible API.
// Works with: Ollama, LM Studio, vLLM, text-generation-inference, etc.
type OpenAICompat struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	headers map[string]string
}

type compatOption func(*OpenAICompat)

func WithHeaders(h map[string]string) compatOption {
	return func(c *OpenAICompat) { c.headers = h }
}

func WithName(name string) compatOption {
	return func(c *OpenAICompat) { c.name = name }
}

func NewOpenAICompat(baseURL, apiKey, model string, opts ...compatOption) *OpenAICompat {
	c := &OpenAICompat{
		name:    "openai-compat",
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 240 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *OpenAICompat) Name() string { return c.name }

func (c *OpenAICompat) Chat(ctx context.Context, req inference.ChatRequest) (*inference.ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var r inference.ChatResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w\nbody: %s", err, truncate(string(raw), 200))
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil, fmt.Errorf("API error: %s", string(r.Error))
	}
	if len(r.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}
	return &r, nil
}

func (c *OpenAICompat) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/models", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ping: HTTP %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
