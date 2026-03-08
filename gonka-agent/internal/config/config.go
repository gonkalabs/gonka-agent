package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Inference — single key (primary) and pool (all keys including primary)
	GonkaPrivateKey string
	GonkaAddress    string
	GonkaSourceURL  string
	GonkaDirectURL  string
	GonkaAPIKey     string
	GonkaAPIKeys    []string // all keys: GONKA_API_KEYS (comma-sep) + GONKA_API_KEY merged

	// Models: PlanModel for fast planning roles, AgentModel for execute phase.
	// If PlanModel is empty, AgentModel is used for both.
	AgentModel string
	PlanModel  string

	// Workspace
	Workspace          string
	Shell              string
	CommandTimeout     time.Duration
	MaxFileSize        int64
	AllowShell         bool

	// Web
	WebSearchProvider string
	WebSearchKey      string
	SearXNGURL        string
	WebFetchTimeout   time.Duration
	WebFetchMaxSize   int64

	// Search
	RgPath string

	// Embed (optional)
	EmbedBackend string
	EmbedPython  string
	EmbedModel   string
	// EmbedURL is the base URL for the local /v1/embeddings server (embed-server.py).
	// Falls back to GonkaDirectURL if not set — but most Gonka nodes don't
	// expose an embeddings endpoint, so a local fastembed sidecar is preferred.
	EmbedURL string

	// Server
	MCPTransport string
	MCPPort      string
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	ws := getEnv("AGENT_WORKSPACE", "")
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("config: AGENT_WORKSPACE not set and os.Getwd failed: %w", err)
		}
		ws = cwd
	}

	cmdTimeout, _ := time.ParseDuration(getEnv("AGENT_COMMAND_TIMEOUT", "60s"))
	if cmdTimeout == 0 {
		cmdTimeout = 60 * time.Second
	}

	webTimeout, _ := time.ParseDuration(getEnv("AGENT_WEB_FETCH_TIMEOUT", "15s"))
	if webTimeout == 0 {
		webTimeout = 15 * time.Second
	}

	maxFile := int64(1 << 20) // 1MB default
	webMax := int64(5 << 20)  // 5MB default

	allowShell := true
	if v := strings.ToLower(getEnv("AGENT_ALLOW_SHELL", "true")); v == "false" || v == "0" {
		allowShell = false
	}

	// Build the key pool: merge GONKA_API_KEYS (comma-sep) with GONKA_API_KEY.
	primaryKey := getEnv("GONKA_API_KEY", "")
	var allKeys []string
	if raw := getEnv("GONKA_API_KEYS", ""); raw != "" {
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				allKeys = append(allKeys, k)
			}
		}
	}
	if primaryKey != "" {
		found := false
		for _, k := range allKeys {
			if k == primaryKey {
				found = true
				break
			}
		}
		if !found {
			allKeys = append([]string{primaryKey}, allKeys...)
		}
	} else if len(allKeys) > 0 {
		primaryKey = allKeys[0]
	}

	// Resolve the single inference URL: GONKA_DIRECT_URL > GONKA_SOURCE_URL > local opengnk proxy.
	// opengnk runs on :9090 and proxies to the Gonka network inference nodes.
	inferURL := getEnv("GONKA_DIRECT_URL", "")
	if inferURL == "" {
		inferURL = getEnv("GONKA_SOURCE_URL", "")
	}
	if inferURL == "" {
		inferURL = "http://localhost:9090/v1"
	}

	cfg := &Config{
		GonkaPrivateKey:   getEnv("GONKA_PRIVATE_KEY", ""),
		GonkaAddress:      getEnv("GONKA_ADDRESS", ""),
		GonkaSourceURL:    inferURL, // unified inference URL
		GonkaDirectURL:    inferURL,
		GonkaAPIKey:       primaryKey,
		GonkaAPIKeys:      allKeys,
		Workspace:         ws,
		Shell:             getEnv("AGENT_SHELL", "/bin/bash"),
		CommandTimeout:    cmdTimeout,
		MaxFileSize:       maxFile,
		AllowShell:        allowShell,
		WebSearchProvider: getEnv("AGENT_WEB_SEARCH_PROVIDER", "searxng"),
		WebSearchKey:      getEnv("AGENT_WEB_SEARCH_KEY", ""),
		SearXNGURL:        getEnv("SEARXNG_URL", "http://localhost:8888"),
		WebFetchTimeout:   webTimeout,
		WebFetchMaxSize:   webMax,
		RgPath:            getEnv("AGENT_RG_PATH", "rg"),
		EmbedBackend:      getEnv("AGENT_EMBED_BACKEND", "none"),
		EmbedPython:       getEnv("AGENT_EMBED_PYTHON", "python3"),
		EmbedModel:        getEnv("AGENT_EMBED_MODEL", "all-MiniLM-L6-v2"),
		EmbedURL:          getEnv("AGENT_EMBED_URL", ""),
		MCPTransport:      getEnv("AGENT_MCP_TRANSPORT", "stdio"),
		MCPPort:           getEnv("AGENT_MCP_PORT", "3000"),
		AgentModel:        getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"),
		PlanModel:         getEnv("AGENT_PLAN_MODEL", ""),
	}

	if cfg.GonkaPrivateKey == "" && cfg.GonkaAPIKey == "" {
		return nil, fmt.Errorf("config: GONKA_PRIVATE_KEY or GONKA_API_KEY required")
	}
	if len(cfg.GonkaAPIKeys) == 0 && cfg.GonkaAPIKey != "" {
		cfg.GonkaAPIKeys = []string{cfg.GonkaAPIKey}
	}

	// Isolation: workspace must not contain the agent binary directory.
	execPath, _ := os.Executable()
	if execPath != "" && strings.HasPrefix(execPath, cfg.Workspace) {
		return nil, fmt.Errorf("config: AGENT_WORKSPACE=%q must not contain the agent binary — use a separate user project directory", cfg.Workspace)
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
