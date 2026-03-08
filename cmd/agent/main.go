// MCP server — stdio transport + HTTP/SSE transport.
// Exposes all tools via JSON-RPC 2.0 (tools, resources, prompts, sampling).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

// ─── JSON-RPC types ───────────────────────────────────────────────────────────

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ─── SSE hub ─────────────────────────────────────────────────────────────────

type sseHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newHub() *sseHub { return &sseHub{clients: map[chan string]struct{}{}} }

func (h *sseHub) subscribe() chan string {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *sseHub) broadcast(event, data string) {
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	h.mu.Unlock()
}

// ─── MCP Resources ────────────────────────────────────────────────────────────

var mcpResources = []map[string]any{
	{
		"uri":         "gonka://workspace",
		"name":        "Workspace",
		"description": "The agent workspace directory. Read this to understand project structure.",
		"mimeType":    "text/plain",
	},
	{
		"uri":         "gonka://session-state",
		"name":        "Shell Session State",
		"description": "Current persistent bash session: cwd, env vars, background jobs.",
		"mimeType":    "text/plain",
	},
	{
		"uri":         "gonka://plan",
		"name":        "Active FinalPlan",
		"description": "The current FinalPlan from Plan Builder (if in phased mode).",
		"mimeType":    "application/json",
	},
}

// ─── MCP Prompts ──────────────────────────────────────────────────────────────

var mcpPrompts = []map[string]any{
	{
		"name":        "extra-hard-task",
		"description": "Activates the full 6-phase Plan-First Chain for EXTRA HARD tasks.",
		"arguments": []map[string]any{
			{"name": "task", "description": "The task to perform", "required": true},
		},
	},
	{
		"name":        "code-review",
		"description": "Deep audit of a specific file or symbol using Deep Audit role.",
		"arguments": []map[string]any{
			{"name": "path", "description": "File to review", "required": true},
			{"name": "symbol", "description": "Symbol/function to focus on", "required": false},
		},
	},
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	t := &tools.Cfg{
		Workspace: cfg.Workspace, Shell: cfg.Shell,
		CmdTimeout: cfg.CommandTimeout, MaxFileSize: cfg.MaxFileSize,
		AllowShell: cfg.AllowShell, RgPath: cfg.RgPath,
		WebSearchKey: cfg.WebSearchKey, WebFetchTimeout: cfg.WebFetchTimeout,
		WebFetchMaxSize: cfg.WebFetchMaxSize,
	}

	hub := newHub()

	// Start SSE HTTP server on a separate port for real-time progress streaming.
	ssePort := os.Getenv("SSE_PORT")
	if ssePort == "" {
		ssePort = "8765"
	}
	go startSSEServer(hub, t, cfg, ssePort)
	slog.Info("SSE server starting", "port", ssePort)

	// Stdio MCP server (primary transport for Cursor/Claude Desktop).
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 8*1024*1024), 8*1024*1024)

	slog.Info("gonka-agent MCP server ready", "workspace", cfg.Workspace)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" { continue }

		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(Response{JSONRPC: "2.0", Error: &RPCError{Code: -32700, Message: "parse error"}})
			continue
		}

		resp := handleRequest(req, t, hub, cfg)
		if resp != nil {
			_ = enc.Encode(resp)
		}
	}
}

func handleRequest(req Request, t *tools.Cfg, hub *sseHub, cfg *config.Config) *Response {
	switch req.Method {

	case "initialize":
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools":     map[string]any{},
					"resources": map[string]any{"subscribe": false},
					"prompts":   map[string]any{},
					"sampling":  map[string]any{},
					"logging":   map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "gonka-agent",
					"version": "2.0.0",
				},
			},
		}

	case "notifications/initialized":
		return nil

	// ── Tools ──────────────────────────────────────────────────────────────

	case "tools/list":
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"tools": tools.AllToolDefs()},
		}

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		slog.Info("tool call", "name", p.Name)

		hub.broadcast("tool_start", fmt.Sprintf(`{"tool":%q}`, p.Name))
		t0 := time.Now()
		r := agent.Dispatch(t, p.Name, string(p.Arguments))
		elapsed := time.Since(t0).Milliseconds()
		hub.broadcast("tool_end", fmt.Sprintf(`{"tool":%q,"elapsed_ms":%d,"is_error":%v}`,
			p.Name, elapsed, r.IsError))

		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: ToolResult{
				Content: []ToolContent{{Type: "text", Text: r.Content}},
				IsError: r.IsError,
			},
		}

	// ── Resources ──────────────────────────────────────────────────────────

	case "resources/list":
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"resources": mcpResources},
		}

	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &p)
		content := readResource(p.URI, t)
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{
				"contents": []map[string]any{
					{"uri": p.URI, "mimeType": "text/plain", "text": content},
				},
			},
		}

	// ── Prompts ────────────────────────────────────────────────────────────

	case "prompts/list":
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"prompts": mcpPrompts},
		}

	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		text := buildPrompt(p.Name, p.Arguments)
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{
				"description": "Gonka agent prompt",
				"messages": []map[string]any{
					{"role": "user", "content": map[string]any{"type": "text", "text": text}},
				},
			},
		}

	// ── Sampling (agent executes task on behalf of caller) ─────────────────

	case "sampling/createMessage":
		var p struct {
			Messages []struct {
				Role    string `json:"role"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"maxTokens"`
		}
		_ = json.Unmarshal(req.Params, &p)
		var task string
		for _, m := range p.Messages {
			if m.Role == "user" { task = m.Content.Text; break }
		}
		model := getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")
		pool := roles.NewLLMPool(cfg.GonkaSourceURL, model, cfg.GonkaAPIKeys)
		client := agent.NewClient(cfg.GonkaDirectURL, cfg.GonkaAPIKey, model)

		hub.broadcast("agent_start", fmt.Sprintf(`{"task":%q,"pool_size":%d}`, task, pool.Size()))
		result := agent.RunPhased(pool, client, t, task, func(event, detail string) {
			hub.broadcast("agent_progress", fmt.Sprintf(`{"event":%q,"detail":%q}`, event, detail))
		})
		hub.broadcast("agent_done", fmt.Sprintf(`{"success":%v}`, result.Success))

		answer := result.FinalAnswer
		if result.Err != nil { answer = "Error: " + result.Err.Error() }
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{
				"role":       "assistant",
				"content":    map[string]any{"type": "text", "text": answer},
				"model":      model,
				"stopReason": "end_turn",
			},
		}

	// ── Logging ────────────────────────────────────────────────────────────

	case "logging/setLevel":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}

	default:
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &RPCError{Code: -32601, Message: fmt.Sprintf("unknown method: %s", req.Method)},
		}
	}
}

// ─── SSE HTTP server ─────────────────────────────────────────────────────────

func startSSEServer(hub *sseHub, t *tools.Cfg, cfg *config.Config, port string) {
	mux := http.NewServeMux()

	// Progress stream.
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		// Keepalive every 15s.
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case msg := <-ch:
				fmt.Fprint(w, msg)
				if ok { flusher.Flush() }
			case <-ticker.C:
				fmt.Fprint(w, ": keepalive\n\n")
				if ok { flusher.Flush() }
			case <-r.Context().Done():
				return
			}
		}
	})

	// HTTP tool call endpoint (alternative to stdio).
	mux.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := agent.Dispatch(t, p.Name, string(p.Arguments))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	// Run phased agent via HTTP.
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Task string `json:"task"` }
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Task == "" {
			http.Error(w, "task required", http.StatusBadRequest)
			return
		}
		model := getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")
		pool := roles.NewLLMPool(cfg.GonkaSourceURL, model, cfg.GonkaAPIKeys)
		client := agent.NewClient(cfg.GonkaDirectURL, cfg.GonkaAPIKey, model)

		hub.broadcast("agent_start", fmt.Sprintf(`{"task":%q,"pool_size":%d}`, p.Task, pool.Size()))
		result := agent.RunPhased(pool, client, t, p.Task, func(event, detail string) {
			hub.broadcast("agent_progress", fmt.Sprintf(`{"event":%q,"detail":%q}`, event, detail))
			fmt.Printf("[%s] %s\n", event, detail)
		})
		hub.broadcast("agent_done", fmt.Sprintf(`{"success":%v}`, result.Success))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	// Health.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"2.0.0"}`))
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	slog.Info("SSE server listening", "port", port)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("SSE server error", "err", err)
	}
}

// ─── Resource content ─────────────────────────────────────────────────────────

func readResource(uri string, t *tools.Cfg) string {
	switch uri {
	case "gonka://workspace":
		r := t.ListDir(".")
		return r.Content
	case "gonka://session-state":
		return fmt.Sprintf("workspace: %s\nshell: %s", t.Workspace, t.Shell)
	case "gonka://plan":
		return `{"status":"no active phased run"}`
	default:
		return "unknown resource: " + uri
	}
}

// ─── Prompt templates ─────────────────────────────────────────────────────────

func buildPrompt(name string, args map[string]string) string {
	switch name {
	case "extra-hard-task":
		task := args["task"]
		return fmt.Sprintf(`You are running a 6-phase Plan-First Chain for an EXTRA HARD task.

Task: %s

Phase sequence:
1. UNDERSTAND: Context Keeper identifies facts and gaps
2. PLAN: Deep Audit → Protocol Architect + Security Reviewer (parallel) → API/Backend Engineer → QA/Test Engineer → Scientist-Validator → Git Worker → Plan Builder
3. PRE_VERIFY: Pre-PR Validator gates execution (GO/BLOCK)
4. EXECUTE: Precise tool calls per FinalPlan, verify_replace before every search_replace
5. VALIDATE: build + tests + evidence table
6. CONFIRM: Scientist-Validator final verdict (PROVEN/INSUFFICIENT/INVALID)

Begin now.`, task)

	case "code-review":
		path := args["path"]
		symbol := args["symbol"]
		if symbol != "" {
			return fmt.Sprintf("Deep Audit of symbol '%s' in file '%s'. Trace to scalar level: find exact line, count occurrences, check scope for each, build call chain.", symbol, path)
		}
		return fmt.Sprintf("Deep Audit of file '%s'. Identify all exported symbols, their callers, and any scope issues.", path)

	default:
		return "Unknown prompt: " + name
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}
