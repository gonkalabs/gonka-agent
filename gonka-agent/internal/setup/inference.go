// Package setup wires together the inference router from environment
// variables, breaking the import cycle between inference and providers.
package setup

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/gonkalabs/gonka-agent/internal/inference"
	"github.com/gonkalabs/gonka-agent/internal/inference/providers"
)

// InferenceRouter builds a Router from environment variables.
// Priority order: OpenRouter > Gonka > Ollama > custom INFER_PROVIDER_N.
func InferenceRouter() (*inference.Router, error) {
	var pp []inference.Provider

	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		model := os.Getenv("OPENROUTER_MODEL")
		pp = append(pp, providers.NewOpenRouter(key, model))
		slog.Info("inference: added OpenRouter", "model", model)
	}

	if url := os.Getenv("GONKA_API_URL"); url != "" {
		model := os.Getenv("GONKA_MODEL")
		var keys []string
		if k := os.Getenv("GONKA_API_KEY"); k != "" {
			keys = strings.Split(k, ",")
		}
		pp = append(pp, providers.NewGonka(url, model, keys))
		slog.Info("inference: added Gonka", "url", url)
	}

	if url := os.Getenv("OLLAMA_URL"); url != "" {
		model := os.Getenv("OLLAMA_MODEL")
		pp = append(pp, providers.NewOllama(url, model))
		slog.Info("inference: added Ollama", "url", url)
	}

	for i := 1; i <= 9; i++ {
		prefix := fmt.Sprintf("INFER_PROVIDER_%d", i)
		ptype := os.Getenv(prefix + "_TYPE")
		if ptype == "" {
			continue
		}
		url := os.Getenv(prefix + "_URL")
		key := os.Getenv(prefix + "_KEY")
		model := os.Getenv(prefix + "_MODEL")

		switch strings.ToLower(ptype) {
		case "openrouter":
			pp = append(pp, providers.NewOpenRouter(key, model))
		case "gonka":
			pp = append(pp, providers.NewGonka(url, model, strings.Split(key, ",")))
		case "ollama":
			pp = append(pp, providers.NewOllama(url, model))
		case "openai", "openai-compat":
			pp = append(pp, providers.NewOpenAICompat(url, key, model, providers.WithName("custom-"+fmt.Sprint(i))))
		default:
			slog.Warn("inference: unknown provider type", "type", ptype, "idx", i)
		}
	}

	if len(pp) == 0 {
		slog.Warn("inference: no providers configured, falling back to local Ollama")
		pp = append(pp, providers.NewOllama("", ""))
	}

	return inference.NewRouter(pp), nil
}
