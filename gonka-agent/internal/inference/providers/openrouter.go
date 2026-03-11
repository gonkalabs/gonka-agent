package providers

import (
	"github.com/gonkalabs/gonka-agent/internal/inference"
)

const openRouterBaseURL = "https://openrouter.ai/api/v1"

// NewOpenRouter creates a provider for OpenRouter.
// OpenRouter is OpenAI-compatible but requires HTTP-Referer and X-Title.
func NewOpenRouter(apiKey, model string) inference.Provider {
	if model == "" {
		model = "openrouter/free"
	}
	return NewOpenAICompat(openRouterBaseURL, apiKey, model,
		WithName("openrouter"),
		WithHeaders(map[string]string{
			"HTTP-Referer": "https://github.com/gonkalabs/gonka-agent",
			"X-Title":      "gonka-agent",
		}),
	)
}
