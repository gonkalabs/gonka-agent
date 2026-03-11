package providers

import (
	"github.com/gonkalabs/gonka-agent/internal/inference"
)

// NewOllama creates a provider for a local Ollama instance.
// Ollama serves OpenAI-compatible endpoints at /v1.
func NewOllama(baseURL, model string) inference.Provider {
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	if model == "" {
		model = "qwen2.5-coder:7b"
	}
	return NewOpenAICompat(baseURL, "", model,
		WithName("ollama"),
	)
}
