// Package agent is the core orchestrator: runs the 6-phase Plan-First Chain.
// Each phase uses a targeted set of the 10 roles. Execution only starts
// after FinalPlan is approved by Pre-PR Validator.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/loopdetect"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/rolechain"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

// ─── OpenAI-compatible types ─────────────────────────────────────────────────

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

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// ─── Step tracks one tool execution ──────────────────────────────────────────

type Step struct {
	ToolName  string
	ArgsJSON  string
	Result    string
	IsError   bool
	ElapsedMs int64
}

// ─── ProgressFn ───────────────────────────────────────────────────────────────

// ProgressFn is called during execution to report real-time progress.
// event: short identifier ("phase", "role", "tool", "build", "cache").
// detail: human-readable description.
// A nil ProgressFn is safe — it is replaced by a no-op before use.
type ProgressFn func(event, detail string)

func safeProgress(fn ProgressFn) ProgressFn {
	if fn == nil {
		return func(_, _ string) {}
	}
	return fn
}

// ─── RunResult ────────────────────────────────────────────────────────────────

// Complexity describes how the task was routed.
type Complexity string

const (
	ComplexitySimple  Complexity = "simple"
	ComplexityMedium  Complexity = "medium"
	ComplexityHard    Complexity = "hard"
)

type RunResult struct {
	FinalAnswer    string
	Steps          []Step
	Success        bool
	Err            error
	ScientistFrame roles.ScientistFrame

	// Summary stats — always populated.
	ElapsedMs        int64
	TotalTokens      int64
	PromptTokens     int64
	CompletionTokens int64
	LLMCalls         int64
	Mode             Complexity

	// Session: messages to persist for next invocation.
	SessionMessages []Message
}

// ─── Client ──────────────────────────────────────────────────────────────────

type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client

	// Key pool with per-key cooldowns (populated via SetKeys).
	pool *keyPool

	// Fallback endpoint (OpenRouter etc.) — used when primary times out or errors.
	fallbackURL   string
	fallbackKey   string
	fallbackModel string

	// pendingFeedback is sent as X-Inference-Feedback on the next request then cleared.
	pendingFeedback string
	feedbackMu      sync.Mutex

	// Token counters for EXECUTE phase calls.
	execPromptTokens     int64
	execCompletionTokens int64
	execCalls            int64
}

func NewClient(baseURL, apiKey, model string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:9090/v1"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{},
		pool:    newKeyPool([]string{apiKey}),
	}
}

// SetKeys configures the key pool for rotation on 429/auth failures.
func (c *Client) SetKeys(keys []string) {
	c.pool = newKeyPool(keys)
	if len(keys) > 0 {
		c.APIKey = keys[0]
	}
}

// SetFallback configures a secondary inference endpoint used when the primary
// times out or returns errors. Typically OpenRouter.
func (c *Client) SetFallback(url, key, model string) {
	c.fallbackURL = strings.TrimRight(url, "/")
	c.fallbackKey = key
	c.fallbackModel = model
}

// SetFeedback schedules an X-Inference-Feedback header to be sent on the
// next inference request.  outcome must be "resolved" or "unresolved".
func (c *Client) SetFeedback(outcome string) {
	c.feedbackMu.Lock()
	c.pendingFeedback = `{"outcome":"` + outcome + `"}`
	c.feedbackMu.Unlock()
}

func (c *Client) chat(messages []Message, toolDefs []ToolDef) (*chatResponse, error) {
	// Primary: 15s timeout — if DAPI is slow, fail fast to fallback.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	r, err := c.doChatCtx(ctx, c.BaseURL, c.Model, c.apiKeyForRequest(), messages, toolDefs)
	cancel()
	if err != nil && c.fallbackURL != "" {
		slog.Warn("inference: primary failed, trying fallback",
			"primary_err", err, "fallback", c.fallbackURL)
		// Convert tool messages to user messages for providers that don't support tool role.
		fbMessages := sanitizeMessagesForFallback(messages)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
		r2, err2 := c.doChatCtx(ctx2, c.fallbackURL, c.fallbackModel, c.fallbackKey, fbMessages, toolDefs)
		cancel2()
		if err2 == nil {
			return r2, nil
		}
		slog.Warn("inference: fallback also failed", "err", err2)
		return nil, fmt.Errorf("primary: %w; fallback: %v", err, err2)
	}
	return r, err
}

func (c *Client) apiKeyForRequest() string {
	if c.pool != nil {
		if k := c.pool.current(); k != "" {
			return k
		}
	}
	return c.APIKey
}

// sanitizeMessagesForFallback converts tool-role messages to user-role messages
// for providers that don't support the "tool" role (e.g., StepFun).
func sanitizeMessagesForFallback(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, Message{
				Role:    "user",
				Content: fmt.Sprintf("[Tool result for call %s]:\n%s", m.ToolCallID, m.Content),
			})
			continue
		}
		// Strip tool_calls from assistant messages — wrap them as text
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			content := m.Content
			for _, tc := range m.ToolCalls {
				content += fmt.Sprintf("\n[Called tool %s(%s)]", tc.Function.Name, tc.Function.Arguments)
			}
			out = append(out, Message{Role: "assistant", Content: content})
			continue
		}
		out = append(out, m)
	}
	return out
}

func (c *Client) doChatCtx(ctx context.Context, baseURL, model, apiKey string, messages []Message, toolDefs []ToolDef) (*chatResponse, error) {
	body, _ := json.Marshal(chatRequest{Model: model, Messages: messages, Tools: toolDefs})

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	c.feedbackMu.Lock()
	if c.pendingFeedback != "" {
		req.Header.Set("X-Inference-Feedback", c.pendingFeedback)
		c.pendingFeedback = ""
	}
	c.feedbackMu.Unlock()

	resp, err := c.HTTP.Do(req)
	if err != nil {
		reason := classifyError(err, 0)
		if reason.isTransient() && c.pool != nil {
			c.pool.markCooling(reason)
		}
		return nil, fmt.Errorf("HTTP %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		reason := classifyError(nil, resp.StatusCode)
		if c.pool != nil {
			c.pool.markCooling(reason)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode == 400 {
		errStr := strings.TrimSpace(string(raw))
		if strings.Contains(errStr, "tool_choice") || strings.Contains(errStr, "tool choice") {
			slog.Warn("inference: 400 tool_choice, retrying without tools", "body", errStr[:min(len(errStr), 200)])
			return c.doChatCtx(ctx, baseURL, model, apiKey, messages, nil)
		}
		return nil, fmt.Errorf("HTTP 400: %s", errStr)
	}

	var r chatResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w\nbody: %s", err, string(raw))
	}
	c.execPromptTokens += int64(r.Usage.PromptTokens)
	c.execCompletionTokens += int64(r.Usage.CompletionTokens)
	c.execCalls++

	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil, fmt.Errorf("API error: %s", string(r.Error))
	}
	return &r, nil
}

// ─── Tool definitions and dispatch ───────────────────────────────────────────

func buildToolDefs() []ToolDef {
	raw := tools.AllToolDefs()
	out := make([]ToolDef, 0, len(raw))
	for _, def := range raw {
		var td ToolDef
		td.Type = "function"
		td.Function.Name = def["name"].(string)
		td.Function.Description = def["description"].(string)
		if schema, ok := def["inputSchema"].(map[string]any); ok {
			td.Function.Parameters = schema
		}
		out = append(out, td)
	}
	return out
}

func Dispatch(t *tools.Cfg, name, argsJSON string) tools.Result {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)

	str := func(k string) string {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok { return s }
		}
		return ""
	}
	boolVal := func(k string, def bool) bool {
		if v, ok := args[k]; ok {
			if b, ok := v.(bool); ok { return b }
		}
		return def
	}
	intVal := func(k string, def int) int {
		if v, ok := args[k]; ok {
			if n, ok := v.(float64); ok { return int(n) }
		}
		return def
	}

	switch name {
	case "read_file":        return t.ReadFile(str("path"))
	case "write_file":       return t.WriteFile(str("path"), str("content"))
	case "search_replace":   return t.SearchReplace(str("path"), str("old_str"), str("new_str"))
	case "verify_replace":   return t.VerifyReplace(str("path"), str("old_str"))
	case "view_range":       return t.ViewRange(str("path"), intVal("start_line", 1), intVal("end_line", 0))
	case "delete_file":      return t.DeleteFile(str("path"))
	case "list_dir":         return t.ListDir(str("path"))
	case "glob":             return t.GlobSearch(str("pattern"))
	case "grep_search":      return t.GrepSearch(str("pattern"), str("path"), intVal("context_lines", 0), boolVal("case_sensitive", false))
	case "run_command":      return t.RunCommand(str("cmd"))
	case "web_fetch":
		hdrs := make(map[string]string)
		if h, ok := args["headers"]; ok {
			if hm, ok := h.(map[string]any); ok {
				for k, v := range hm {
					if vs, ok := v.(string); ok {
						hdrs[k] = vs
					}
				}
			}
		}
		return t.WebFetchWithHeaders(str("url"), hdrs)
	case "web_search":       return t.WebSearch(str("query"), intVal("num_results", 10))
	case "get_diagnostics":  return t.GetDiagnostics(str("path"))
	case "code_analysis":    return t.CodeAnalysis(str("path"), str("symbol"))
	case "dependency_graph": return t.DependencyGraph(str("path"))
	case "run_tests":        return t.RunTests(str("path"))
	case "git_diff":         return t.GitDiff()
	case "git_status":       return t.GitStatus()
	case "git_reset_file":   return t.GitResetFile(str("path"))
	case "count_lines":      return t.CountLines(str("path"))
	case "semantic_search":      return t.SemanticSearch(str("query"), intVal("top_k", 5))
	case "detect_build_system":  return tools.Result{Content: func() string {
		bi := t.DetectBuildSystem()
		b, _ := json.Marshal(bi)
		return string(b)
	}()}
	default:                     return tools.Result{Content: "unknown tool: " + name, IsError: true}
	}
}

// ─── Main phased entry point ──────────────────────────────────────────────────

// RunPhased executes the full 6-phase Plan-First Chain.
// pool is used for the planning phase (multiple keys → parallel roles).
// client is used for the execute phase (single sequential tool-use loop).
func RunPhased(pool *roles.LLMPool, client *Client, t *tools.Cfg, task string, progress ProgressFn) RunResult {
	progress = safeProgress(progress)
	t0 := time.Now()
	state := newAgentState(task)
	state.LLM = pool
	state.Progress = progress
	chain := rolechain.New(pool, t, task, func(ev, detail string) { progress(ev, detail) })

	for state.RePlanCount < 3 {
		state.Phase = PhaseUnderstand

		progress("phase", "PLAN (full 8-role chain)")
		plan, err := chain.Run()
		if err != nil {
			slog.Error("role chain failed", "err", err, "cycle", state.RePlanCount)
			state.RePlanCount++
			continue
		}
		state.Plan = plan

		state.Phase = PhasePreVerify
		progress("phase", "PRE-VERIFY")
		pvResult, err := chain.PreVerify(plan)
		if err != nil {
			slog.Warn("PreVerify error, continuing", "err", err)
		}
		planIsEmpty := state.Plan == nil || len(state.Plan.TechnicalPlan.Steps) == 0
		if planIsEmpty && state.RePlanCount > 0 {
			progress("phase", "PRE-VERIFY: 0 edit steps → discovery mode")
		} else if pvResult.Verdict != "GO" {
			var hardBlockers []string
			for _, b := range pvResult.Blockers {
				if !strings.Contains(b, "old_str cannot be empty") &&
					!strings.Contains(b, "'old_str' cannot be empty") &&
					!strings.Contains(b, "cannot be empty") {
					hardBlockers = append(hardBlockers, b)
				}
			}
			if len(hardBlockers) > 0 {
				slog.Warn("PreVerify BLOCKED", "blockers", strings.Join(hardBlockers, "; "), "cycle", state.RePlanCount)
				progress("phase", fmt.Sprintf("PRE-VERIFY: BLOCKED (%d blockers) — re-planning", len(hardBlockers)))
				state.RePlanCount++
				continue
			}
			progress("phase", "PRE-VERIFY: no old_str → EXECUTE discovers via read_file")
		} else {
			progress("phase", "PRE-VERIFY: GO")
		}

		state.Phase = PhaseExecute
		progress("phase", "EXECUTE")
		execResult := executePhase(client, t, state)
		if execResult.NeedsRePlan {
			state.RePlanCount++
			rollbackAll(t, state)
			continue
		}

		state.Phase = PhaseValidate
		progress("phase", "VALIDATE")
		evidence := validatePhase(t, state)

		state.Phase = PhaseConfirm
		sci, _ := chain.UpdateScientistVerdict(plan.ScientistFrame, evidence)
		state.FinalVerdict = sci
		progress("phase", fmt.Sprintf("CONFIRM: verdict=%s", sci.Verdict))

		elapsed := time.Since(t0)
		answer, stats := buildFinalAnswer(state, evidence, execResult.Steps, client, elapsed, ComplexityHard)
		return RunResult{
			FinalAnswer:      answer,
			Steps:            execResult.Steps,
			Success:          true,
			ScientistFrame:   sci,
			ElapsedMs:        elapsed.Milliseconds(),
			TotalTokens:      stats.totalTokens,
			PromptTokens:     stats.promptTokens,
			CompletionTokens: stats.completionTokens,
			LLMCalls:         stats.llmCalls,
			Mode:             ComplexityHard,
		}
	}

	rollbackAll(t, state)
	return RunResult{
		Err:       fmt.Errorf("task failed after %d re-plan cycles", state.RePlanCount),
		ElapsedMs: time.Since(t0).Milliseconds(),
		Mode:      ComplexityHard,
	}
}

// ─── Execute phase ────────────────────────────────────────────────────────────

type execResult struct {
	Steps      []Step
	NeedsRePlan bool
	Reason      string
}

// executePhase runs the LLM tool loop guided by FinalPlan's system prompt.
// After each build failure: auto-inject exact error location via view_range.
func executePhase(client *Client, t *tools.Cfg, state *AgentState) execResult {
	progress := safeProgress(state.Progress)
	toolDefs := buildToolDefs()
	messages := buildExecuteMessages(state)
	var steps []Step

	const maxIter = 30
	for i := 0; i < maxIter; i++ {
		resp, err := client.chat(messages, toolDefs)
		if err != nil {
			// ErrTransient: retry up to 3 times.
			errSig := "chat:" + err.Error()
			state.ErrorCounts[errSig]++
			if state.ErrorCounts[errSig] <= 3 {
				time.Sleep(time.Duration(state.ErrorCounts[errSig]) * 2 * time.Second)
				continue
			}
			return execResult{Steps: steps, NeedsRePlan: true, Reason: "transient error exceeded retries: " + err.Error()}
		}
		if len(resp.Choices) == 0 {
			continue
		}

		msg := resp.Choices[0].Message
		msg.Role = "assistant"

		if msg.Content != "" {
			progress("think", truncate(msg.Content, 120))
		}

		// No tool calls = final answer from execute phase.
		if len(msg.ToolCalls) == 0 {
			return execResult{Steps: steps, NeedsRePlan: false}
		}

		messages = append(messages, msg)

		for _, tc := range msg.ToolCalls {
			progress("tool", fmt.Sprintf("%s(%s)", tc.Function.Name, truncate(tc.Function.Arguments, 80)))

			// Save original file content before first edit (rollback registry).
			if isWriteTool(tc.Function.Name) {
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				if p, ok := args["path"].(string); ok {
					if _, seen := state.OriginalContent[p]; !seen {
						orig := t.ReadFile(p)
						if !orig.IsError {
							state.OriginalContent[p] = orig.Content
							state.ModifiedFiles = append(state.ModifiedFiles, p)
						}
					}
				}
			}

			t0 := time.Now()
			r := Dispatch(t, tc.Function.Name, tc.Function.Arguments)
			elapsed := time.Since(t0).Milliseconds()

			result := r.Content
			if len(result) > 8000 {
				result = result[:8000] + "\n...(truncated)"
			}
			label := "ok"
			if r.IsError { label = "err" }
			progress("result", fmt.Sprintf("[%s] %s", label, truncate(result, 200)))

			// Auto-debug: when go build fails, inject exact error locations.
			if isBuildCommand(tc.Function.Name, tc.Function.Arguments) && r.IsError {
				errs := parseBuildErrors(result)
				var debugMsg strings.Builder
				debugMsg.WriteString("AUTO-DEBUG: build failed with the following errors:\n\n")
				for _, e := range errs {
					errSig := fmt.Sprintf("%s:%d:%s", e.File, e.Line, e.Message)
					state.ErrorCounts[errSig]++
					if state.ErrorCounts[errSig] >= 2 {
						// Dead-end: same error appears twice.
						return execResult{
							Steps:       steps,
							NeedsRePlan: true,
							Reason:      "dead-end: " + errSig,
						}
					}
					// View context around error.
					ctx := t.ViewRange(e.File, e.Line-3, e.Line+5)
					debugMsg.WriteString(fmt.Sprintf(
						"Error at %s:%d:%d: %s\nContext:\n%s\n",
						e.File, e.Line, e.Col, e.Message, ctx.Content))
				}
				// Inject debug context as additional tool result.
				steps = append(steps, Step{
					ToolName: "AUTO_DEBUG", ArgsJSON: "", Result: debugMsg.String(),
					IsError: true, ElapsedMs: 0,
				})
				messages = append(messages, Message{
					Role: "tool", ToolCallID: tc.ID,
					Name: "build_error_context", Content: debugMsg.String(),
				})
				continue
			}

			steps = append(steps, Step{
				ToolName: tc.Function.Name, ArgsJSON: tc.Function.Arguments,
				Result: result, IsError: r.IsError, ElapsedMs: elapsed,
			})
			messages = append(messages, Message{
				Role: "tool", ToolCallID: tc.ID,
				Name: tc.Function.Name, Content: result,
			})
		}

		// Context reinforcement every 5 steps.
		state.StepCount++
		if state.StepCount%5 == 0 {
			messages = append(messages, Message{
				Role: "system",
				Content: fmt.Sprintf(
					"[CONTEXT CHECK — step %d]\nOriginal task: %s\nPhase: EXECUTE\nModified files: %v\n"+
						"Plan steps remaining: verify by reviewing FinalPlan.\n"+
						"If you have completed all steps, respond with your final summary.",
					state.StepCount, state.OriginalTask, state.ModifiedFiles),
			})
		}
	}
	return execResult{Steps: steps, NeedsRePlan: false}
}

// ─── Validate phase ───────────────────────────────────────────────────────────

func validatePhase(t *tools.Cfg, state *AgentState) string {
	progress := safeProgress(state.Progress)
	var evidence strings.Builder
	evidence.WriteString("=== EVIDENCE TABLE ===\n\n")

	// Use detected build command; fall back to "go build ./..." for Go projects.
	buildCmd := "go build ./..."
	testCmd := "go test ./..."
	if state.Plan != nil && state.Plan.TechnicalPlan.BuildCommand != "" {
		buildCmd = state.Plan.TechnicalPlan.BuildCommand
		testCmd = state.Plan.TechnicalPlan.TestCommand
	}

	build := t.RunCommand(buildCmd)
	if build.IsError {
		evidence.WriteString("BUILD: ❌ FAILED\n" + build.Content + "\n")
		progress("build", "❌ FAILED")
	} else {
		evidence.WriteString("BUILD: ✅ PASSED\n")
		progress("build", "✅ PASSED")
	}

	vet := t.GetDiagnostics("./...")
	evidence.WriteString("VET: " + vet.Content + "\n")

	if testCmd != "" {
		testResult := t.RunCommand(testCmd)
		if testResult.IsError {
			evidence.WriteString("TESTS: ❌ FAILED\n" + testResult.Content + "\n")
			progress("test", "❌ FAILED")
		} else {
			evidence.WriteString("TESTS: ✅\n" + testResult.Content + "\n")
			progress("test", "✅ PASSED")
		}
	}

	diff := t.GitDiff()
	evidence.WriteString("CHANGES:\n" + diff.Content + "\n")

	if state.Plan != nil {
		evidence.WriteString("\nPLAN CRITERIA:\n")
		for i, criterion := range state.Plan.QAPlan.SuccessCriteria {
			evidence.WriteString(fmt.Sprintf("  %d. %s\n", i+1, criterion))
		}
	}

	state.EvidenceTable = evidence.String()
	return evidence.String()
}

// ─── Rollback ─────────────────────────────────────────────────────────────────

func rollbackAll(t *tools.Cfg, state *AgentState) {
	for path, orig := range state.OriginalContent {
		r := t.GitResetFile(path)
		if r.IsError {
			// Fallback: write original content back directly.
			_ = os.WriteFile(path, []byte(orig), 0o644)
		}
		slog.Info("rolled back", "path", path)
	}
	state.OriginalContent = map[string]string{}
	state.ModifiedFiles = nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func buildExecuteMessages(state *AgentState) []Message {
	planJSON, _ := json.MarshalIndent(state.Plan, "", "  ")

	// When the role chain produced 0 concrete edit steps, switch to discovery mode:
	// the LLM must read files first, find exact strings, then make changes.
	hasSteps := state.Plan != nil && len(state.Plan.TechnicalPlan.Steps) > 0

	var userMsg string
	if hasSteps {
		userMsg = `Execute the plan step by step:
STEP 0 (mandatory): call read_file on EVERY file you plan to modify. Do NOT skip this.
STEP 1+: use search_replace with old_str copied VERBATIM from the file you just read.
After each search_replace: go build ./... to verify — fix errors immediately if any.
When done: git_diff then final summary.`
	} else {
		userMsg = `No pre-computed edit steps. Discover and implement:
STEP 0 (mandatory): call read_file on every file you plan to modify — get exact content first.
Do NOT use view_range as a substitute for read_file.
STEP 1+: use search_replace with old_str copied VERBATIM from read_file output.
After each search_replace: go build ./... — fix errors immediately.
When done: git_diff then final summary.`
	}

	systemPrompt := fmt.Sprintf(`You are executing a coding task. You have full access to tools.

RULES (apply regardless of plan completeness):
1. NEVER write old_str from memory — always read_file first, copy exact text
2. ALWAYS call verify_replace before search_replace to confirm the string exists
3. After every file edit: call view_range to confirm the change was applied
4. After go build failure: the system injects exact error context — read it and fix
5. Use git_reset_file to undo a bad change
6. When done: call git_diff then provide final summary

FinalPlan (goal and context):
%s

Original task: %s`, string(planJSON), state.OriginalTask)

	return []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}
}

type answerStats struct {
	totalTokens      int64
	promptTokens     int64
	completionTokens int64
	llmCalls         int64
}

func buildFinalAnswer(state *AgentState, evidence string, steps []Step, execClient *Client, elapsed time.Duration, mode Complexity) (string, answerStats) {
	var sb strings.Builder
	var stats answerStats

	sb.WriteString(fmt.Sprintf("Task completed: %s\n\n", state.OriginalTask))
	sb.WriteString(evidence)

	toolCounts := map[string]int{}
	for _, s := range steps {
		toolCounts[s.ToolName]++
	}

	verdict := "—"
	if state.FinalVerdict.Verdict != "" {
		verdict = string(state.FinalVerdict.Verdict)
	}

	// Collect tokens from both phases.
	var planSummary string
	if state.LLM != nil {
		planSummary = state.LLM.TokenSummary()
	}
	var execP, execC, execN int64
	if execClient != nil {
		execP = execClient.execPromptTokens
		execC = execClient.execCompletionTokens
		execN = execClient.execCalls
	}

	sb.WriteString("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("  Task:     %s\n", truncate(state.OriginalTask, 60)))
	sb.WriteString(fmt.Sprintf("  Mode:     %s\n", mode))
	sb.WriteString(fmt.Sprintf("  Elapsed:  %s\n", elapsed.Round(time.Second)))
	sb.WriteString(fmt.Sprintf("  Steps:    %d tool calls\n", len(steps)))
	if len(toolCounts) > 0 {
		sb.WriteString("  Tools:    ")
		for name, count := range toolCounts {
			sb.WriteString(fmt.Sprintf("%s(%d) ", name, count))
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("  Verdict:  %s\n", verdict))
	if state.FinalVerdict.VerdictReason != "" {
		sb.WriteString(fmt.Sprintf("  Reason:   %s\n", truncate(state.FinalVerdict.VerdictReason, 80)))
	}
	if planSummary != "" {
		sb.WriteString(fmt.Sprintf("  Tokens:   %s [plan]\n", planSummary))
	}
	if execN > 0 {
		sb.WriteString(fmt.Sprintf("  Tokens:   calls=%d prompt=%d completion=%d total=%d [execute]\n",
			execN, execP, execC, execP+execC))
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	stats.promptTokens = execP
	stats.completionTokens = execC
	stats.llmCalls = execN
	stats.totalTokens = execP + execC

	return sb.String(), stats
}

func isBuildCommand(toolName, argsJSON string) bool {
	return toolName == "run_command" &&
		(strings.Contains(argsJSON, "build") || strings.Contains(argsJSON, "go test"))
}

func isWriteTool(name string) bool {
	switch name {
	case "write_file", "search_replace":
		return true
	}
	return false
}

var buildErrorRe = regexp.MustCompile(`(?m)^([^:]+):(\d+):(\d+):\s+(.+)$`)

func parseBuildErrors(output string) []BuildError {
	var errs []BuildError
	for _, m := range buildErrorRe.FindAllStringSubmatch(output, -1) {
		line, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		errs = append(errs, BuildError{
			File: m[1], Line: line, Col: col, Message: m[4],
		})
	}
	return errs
}

func truncate(s string, n int) string {
	if len(s) <= n { return s }
	return s[:n] + "…"
}

// ─── Simple (non-phased) loop — kept for backward compat ─────────────────────

// TodoItem is a single task in the agent's self-managed TODO list.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed | cancelled
}

// RunState holds mutable state for a single Run() invocation.
type RunState struct {
	Todos []TodoItem
}

// todoSummary formats the TODO list as a compact reminder injected after tool calls.
func todoSummary(todos []TodoItem) string {
	if len(todos) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n[TODO]\n")
	for _, td := range todos {
		icon := "○"
		switch td.Status {
		case "in_progress":
			icon = "▶"
		case "completed":
			icon = "✓"
		case "cancelled":
			icon = "✗"
		}
		sb.WriteString(fmt.Sprintf("  %s %s: %s\n", icon, td.ID, td.Content))
	}
	return sb.String()
}

// loadGonkaMD returns the contents of gonka.md if it exists, else "".
func loadGonkaMD(workspace string) string {
	p := workspace + "/gonka.md"
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	return "\n\n[Project memory — gonka.md]\n" + s
}

// compressMessages summarises old messages when context grows too large.
// Keeps system prompt (messages[0]) and last keepTail messages intact,
// replacing everything in between with a single summary assistant message.
func compressMessages(messages []Message, keepTail int) []Message {
	if len(messages) <= keepTail+2 {
		return messages
	}
	mid := messages[1 : len(messages)-keepTail]
	var sb strings.Builder
	sb.WriteString("[Context compressed — summary of earlier conversation]\n")
	for _, m := range mid {
		switch m.Role {
		case "user":
			sb.WriteString("User: " + truncate(m.Content, 200) + "\n")
		case "assistant":
			if m.Content != "" {
				sb.WriteString("Assistant: " + truncate(m.Content, 200) + "\n")
			}
			for _, tc := range m.ToolCalls {
				sb.WriteString(fmt.Sprintf("Tool call: %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 80)))
			}
		case "tool":
			sb.WriteString(fmt.Sprintf("Tool result (%s): %s\n", m.Name, truncate(m.Content, 150)))
		}
	}
	compressed := append([]Message{messages[0]},
		Message{Role: "assistant", Content: sb.String()})
	compressed = append(compressed, messages[len(messages)-keepTail:]...)
	return compressed
}

// Run is the single agent loop — delegates to runLoop.
func Run(client *Client, t *tools.Cfg, systemPrompt, userPrompt string, maxIter int, progress ProgressFn) RunResult {
	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
	return runLoop(client, t, messages, maxIter, progress)
}

// dispatchWithState handles todo_write/todo_read/memory_write in-process,
// delegates everything else to Dispatch.
func dispatchWithState(t *tools.Cfg, rs *RunState, name, argsJSON string) tools.Result {
	switch name {
	case "todo_write":
		var args struct {
			Todos []TodoItem `json:"todos"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return tools.FailResult("invalid todo_write args: " + err.Error())
		}
		rs.Todos = args.Todos
		return tools.OkResult("TODO list updated (" + fmt.Sprintf("%d items", len(rs.Todos)) + ")")
	case "todo_read":
		if len(rs.Todos) == 0 {
			return tools.OkResult("(no todos)")
		}
		data, _ := json.MarshalIndent(rs.Todos, "", "  ")
		return tools.OkResult(string(data))
	case "memory_write":
		var args struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return tools.FailResult("invalid memory_write args: " + err.Error())
		}
		p := t.Workspace + "/gonka.md"
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return tools.FailResult("memory_write: " + err.Error())
		}
		defer f.Close()
		_, _ = f.WriteString("\n" + args.Content + "\n")
		return tools.OkResult("saved to gonka.md")
	default:
		return Dispatch(t, name, argsJSON)
	}
}

// RunMedium runs the 3-role partial chain (ContextKeeper → DeepAudit → APIBackendEngineer)
// for medium-complexity tasks. Faster than RunPhased (3-4 LLM calls vs 8-10).
func RunMedium(pool *roles.LLMPool, client *Client, t *tools.Cfg, task string, progress ProgressFn) RunResult {
	progress = safeProgress(progress)
	t0 := time.Now()
	state := newAgentState(task)
	state.LLM = pool
	state.Progress = progress

	chain := rolechain.New(pool, t, task, func(ev, detail string) { progress(ev, detail) })

	progress("phase", "PLAN (partial 3-role chain)")
	plan, err := chain.RunPartial()
	if err != nil {
		// Fall back to simple Run on planning failure.
		progress("phase", fmt.Sprintf("partial chain failed (%v) — falling back to simple loop", err))
		bi := t.DetectBuildSystem()
		sys := SimpleSystemPromptFor(bi)
		return Run(client, t, sys, task, 15, progress)
	}
	state.Plan = plan

	state.Phase = PhaseExecute
	progress("phase", "EXECUTE")
	execRes := executePhase(client, t, state)

	state.Phase = PhaseValidate
	progress("phase", "VALIDATE")
	evidence := validatePhase(t, state)

	elapsed := time.Since(t0)
	answer, stats := buildFinalAnswer(state, evidence, execRes.Steps, client, elapsed, ComplexityMedium)
	return RunResult{
		FinalAnswer:      answer,
		Steps:            execRes.Steps,
		Success:          !execRes.NeedsRePlan,
		ElapsedMs:        elapsed.Milliseconds(),
		TotalTokens:      stats.totalTokens,
		PromptTokens:     stats.promptTokens,
		CompletionTokens: stats.completionTokens,
		LLMCalls:         stats.llmCalls,
		Mode:             ComplexityMedium,
	}
}

// BuildSystemPrompt constructs the system prompt from workspace snapshot + project memory.
// This is injected once before the first API call — model starts with full context.
func BuildSystemPrompt(t *tools.Cfg, bi tools.BuildInfo) string {
	var sb strings.Builder

	// Role + Gonka network self-context
	// The agent is a participant in the Gonka decentralised inference network.
	// Knowing the network's quality axes and its own role in them makes it
	// a more deliberate actor: it avoids redundant calls (L6), finishes tasks
	// cleanly (L9), and signals outcome explicitly (L4 feedback).
	sb.WriteString(`You are gonka-agent — an AI coding agent running on the Gonka decentralised inference network.

## Your role in the Gonka network
You are not a standalone assistant. Every inference call you make is routed through the Gonka network and measured against quality axes:
- L4  Explicit feedback: you signal task outcome via X-Inference-Feedback (resolved/unresolved). This directly improves routing quality for all participants.
- L6  Cache reuse: repeated identical requests are served from cache (X-Cache: HIT). Avoid redundant calls — each unnecessary re-embedding costs the shared resource.
- L8  Latency consistency: high variance in your call patterns degrades the network CV metric. Use batched tool calls, not sequential round-trips.
- L9  Completion rate: network baseline is 94.33%. Every task you abandon without resolving is counted. Finish what you start.

## Collective intelligence principle
Other agents on the network solve similar tasks. Your completed solutions are stored in the local semantic cache and may become context for similar tasks in the future. Write clean, verifiable steps — not just for the current user, but because the solution may be reused.

## Inference efficiency rules
- Batch all independent tool calls in a single response — each round-trip is a billable inference.
- Never re-read a file you already read in this session.
- Never re-embed content you already searched — the embedding cache handles repetition.
- If you are repeating the same tool call without progress: stop and report clearly.`)

	// Thought process — before every action
	sb.WriteString(`

## Before acting
1. Identify the true intent: what does the user actually want?
2. Check if you need live data (web) or local data (files) — choose the right tool.
3. Assess reversibility: file writes and shell commands are irreversible — be precise.
4. Plan all steps upfront. Issue all independent tool calls in ONE batch, not one by one.`)

	// Hard tool rules
	sb.WriteString(`

## Tool rules (hard constraints)
- Batch: always call all independent tools in a single response round.
- GitHub: use api.github.com/users/{u}/repos or api.github.com/repos/{o}/{r}/commits — never github.com (JS SPA, returns no data).
- Web: if 2 searches return no results, stop and answer from knowledge.
- Files: read all needed files in one batch before editing any.
- Loop prevention: if you already fetched or read something, do NOT fetch/read it again.
- search_replace: ALWAYS read the exact file content IMMEDIATELY before calling search_replace. Copy the old_str character-for-character from the read output. If search_replace fails, re-read the file — do NOT guess the content.`)

	// Go-specific concurrency rules (prevents deadlock/race bugs)
	if bi.Language == "go" {
		sb.WriteString(`

## Go concurrency rules
- After writing code with goroutines + sync.Mutex/RWMutex: verify no goroutine tries to Lock() a mutex that the caller already holds while waiting on WaitGroup/channel. This is a deadlock.
- Pattern: if you need goroutine results, release the lock BEFORE spawning goroutines, collect via channel, then re-acquire.
- After ANY file edit involving goroutines or mutexes, run: go vet ./... && go test -race -count=1 ./...
- Verify test assertions mathematically: if threshold = maxEpoch - retainCount, manually trace which values are in range [minPruned, threshold) before asserting expected output.`)
	}

	// Multi-file coordination rules (prevents medium-scenario failures)
	sb.WriteString(`

## Multi-file coordination (critical)
- Before creating multiple files that share types/functions: plan the FULL dependency graph FIRST.
- Rule: define types in ONE canonical location. Import everywhere else. Never duplicate type definitions.
- Go packages: if package A uses types from package B, define types in B. A imports B.
- After writing ANY new file: immediately run ` + "`go build ./...`" + ` to catch import/type errors BEFORE writing the next file.
- If a build fails after writing file N: fix file N BEFORE proceeding to file N+1.
- Maximum 3 retries on the same build error. If stuck: simplify the approach.`)

	// Standard development flows — baked in so agent never re-derives obvious patterns
	sb.WriteString(`

## Standard flows (execute natively, never re-derive)

### Flow: New Go package
1. Create directory + file with package declaration + exported types/functions
2. ` + "`go build ./...`" + ` — verify compiles
3. Update consumers to import the new package
4. ` + "`go build ./...`" + ` — verify again

### Flow: Add HTTP endpoint
1. Define handler function: ` + "`func handleX(w http.ResponseWriter, r *http.Request)`" + `
2. Register in main: ` + "`http.HandleFunc(\"/path\", handleX)`" + `
3. Write httptest integration test
4. ` + "`go test -v ./...`" + `

### Flow: Fix a bug
1. Read the buggy file(s)
2. Write a FAILING test that reproduces the bug
3. Fix the code
4. ` + "`go test -race ./...`" + ` — verify fix + no regressions

### Flow: Refactor / extract
1. Read ALL files involved in one batch
2. Create new package with the extracted code
3. ` + "`go build ./...`" + `
4. Update old files to import the new package (remove old code)
5. ` + "`go build ./...`" + ` + ` + "`go test ./...`" + `

### Flow: Git commit
1. ` + "`git status`" + ` — verify what changed
2. ` + "`git add -A`" + ` — stage
3. ` + "`git commit -m \"type(scope): description\"`" + `
4. Never force-push to main without explicit user confirmation`)

	// Reversibility and action policy
	sb.WriteString(`

## Action policy
- Do it, don't ask: never say "should I..." or "want me to..." — act immediately.
- Memory: when user shares name or preferences, call memory_write immediately without asking.
- Scope: only touch what the task requires. No refactors, no extra files, no speculative changes.
- Stop condition: when the task is done, output the result and stop. Do not keep exploring.

## Capabilities baked into this binary
voice_input: whisper.cpp transcription — listen any language, return text
n8n: workflow engine — docker spin, import workflow JSON, webhook triggers
docker: write docker-compose.yml, run_command "docker compose up -d", verify with curl
ansible: write inventory + playbook YAML, run_command "ansible-playbook -i inv play.yml"
web_search: SearXNG local + DuckDuckGo fallback, retry x2, always returns results
web_fetch: HTTP GET any URL with custom headers, retry x3, strips HTML
run_command: execute any shell command on host — apt, docker, git, curl, make, go, python
write_file: create/overwrite any file in workspace
read_file: read any file
search_replace: edit files precisely
grep_search: regex search across codebase
glob: find files by pattern

Execute immediately. No explanation. No asking. Tools exist — call them.`)

	// Project context
	if bi.Language != "" {
		sb.WriteString("\n\n## Project\n")
		sb.WriteString("Language: " + bi.Language)
		if bi.BuildCmd != "" {
			sb.WriteString(" | build: `" + bi.BuildCmd + "`")
		}
		if bi.TestCmd != "" {
			sb.WriteString(" | test: `" + bi.TestCmd + "`")
		}
		sb.WriteString("\nWorkspace: " + t.Workspace)
	} else {
		sb.WriteString("\n\nWorkspace: " + t.Workspace)
	}

	snapshot := t.WorkspaceSnapshot()
	if snapshot != "" {
		sb.WriteString("\nFiles:\n" + snapshot)
	}
	if mem := loadGonkaMD(t.Workspace); mem != "" {
		sb.WriteString(mem)
	}
	return sb.String()
}

// SimpleSystemPromptFor kept for backward compatibility.
func SimpleSystemPromptFor(bi tools.BuildInfo) string {
	ctx := "workspace"
	if bi.Language != "" {
		ctx = bi.Language
		if bi.BuildCmd != "" {
			ctx += " (build: " + bi.BuildCmd + ")"
		}
	}
	return fmt.Sprintf("You are a coding agent working on a %s project. Use tools to complete tasks accurately. Read files before editing them. Use search_replace for targeted edits.", ctx)
}

// ─── Session persistence ─────────────────────────────────────────────────────

type sessionFile struct {
	Workspace string    `json:"workspace"`
	Messages  []Message `json:"messages"`
	UpdatedAt time.Time `json:"updated_at"`
}

func sessionPath(workspace string) string {
	return workspace + "/.gonka-cache/session.json"
}

// LoadSession loads conversation history from .gonka-cache/session.json.
// Returns nil if no session exists or session is corrupt.
func LoadSession(workspace string) []Message {
	data, err := os.ReadFile(sessionPath(workspace))
	if err != nil {
		return nil
	}
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil
	}
	// Only restore if same workspace.
	if sf.Workspace != workspace {
		return nil
	}
	return sf.Messages
}

// SaveSession persists conversation history to .gonka-cache/session.json.
// A file-based lock prevents concurrent writes from corrupting the file.
func SaveSession(workspace string, messages []Message) {
	if len(messages) == 0 {
		return
	}
	_ = os.MkdirAll(workspace+"/.gonka-cache", 0755)
	sp := sessionPath(workspace)
	release, err := sessionLock(sp)
	if err != nil {
		// Lock timeout: write anyway (best-effort; concurrent write is unlikely).
		_ = writeSessionFile(sp, workspace, messages)
		return
	}
	defer release()
	_ = writeSessionFile(sp, workspace, messages)
}

func writeSessionFile(sp, workspace string, messages []Message) error {
	sf := sessionFile{Workspace: workspace, Messages: messages, UpdatedAt: time.Now()}
	data, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	tmp := sp + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, sp) // atomic replace on Linux
}

// ClearSession removes the session file.
func ClearSession(workspace string) {
	_ = os.Remove(sessionPath(workspace))
}

// RunWithHistory is Run() with session history support.
// history: previous messages from LoadSession (nil = fresh session).
// The system prompt always reflects current workspace (snapshot is re-built each call).
func RunWithHistory(client *Client, t *tools.Cfg, systemPrompt, userPrompt string, history []Message, maxIter int, progress ProgressFn) RunResult {
	// Build messages: system + history (without old system) + new user message.
	messages := []Message{{Role: "system", Content: systemPrompt}}
	for _, m := range history {
		if m.Role == "system" {
			continue // skip stale system from previous session
		}
		messages = append(messages, m)
	}
	messages = append(messages, Message{Role: "user", Content: userPrompt})

	result := runLoop(client, t, messages, maxIter, progress)

	// SessionMessages = everything except the system prompt (to save).
	var sess []Message
	for _, m := range result.SessionMessages {
		if m.Role != "system" {
			sess = append(sess, m)
		}
	}
	result.SessionMessages = sess
	return result
}

// runLoop is the inner loop shared by Run and RunWithHistory.
func runLoop(client *Client, t *tools.Cfg, messages []Message, maxIter int, progress ProgressFn) RunResult {
	progress = safeProgress(progress)
	if maxIter <= 0 {
		maxIter = 30
	}
	t0 := time.Now()
	rs := &RunState{}
	ld := loopdetect.New()
	toolDefs := buildToolDefs()
	var steps []Step
	var totalPrompt, totalCompletion int64
	const ctxLimit = 28000
	const keepTail = 12

	for i := 0; i < maxIter; i++ {
		if totalPrompt > ctxLimit {
			messages = compressMessages(messages, keepTail)
			progress("think", "[context compressed]")
		}
		var resp *chatResponse
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			resp, err = client.chat(messages, toolDefs)
			if err == nil {
				break
			}
			reason := classifyError(err, 0)
			if attempt < 2 && reason.isTransient() {
				jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
				wait := time.Duration(1<<uint(attempt))*4*time.Second + jitter
				progress("think", fmt.Sprintf("[transient %s, retry %d/2 in %s]", reason, attempt+1, wait.Round(time.Millisecond)))
				time.Sleep(wait)
				continue
			}
			break
		}
		if err != nil {
			SaveSession(t.Workspace, messages)
			return RunResult{Err: fmt.Errorf("iteration %d: %w", i, err), Steps: steps, ElapsedMs: time.Since(t0).Milliseconds(), SessionMessages: messages}
		}
		if len(resp.Choices) == 0 {
			return RunResult{Err: fmt.Errorf("empty choices"), Steps: steps, ElapsedMs: time.Since(t0).Milliseconds(), SessionMessages: messages}
		}
		totalPrompt += int64(resp.Usage.PromptTokens)
		totalCompletion += int64(resp.Usage.CompletionTokens)

		msg := resp.Choices[0].Message
		msg.Role = "assistant"
		if msg.Content != "" {
			progress("think", truncate(msg.Content, 160))
		}
		if len(msg.ToolCalls) == 0 {
			messages = append(messages, msg)
			elapsed := time.Since(t0)
			return RunResult{
				FinalAnswer:      msg.Content,
				Steps:            steps,
				Success:          true,
				ElapsedMs:        elapsed.Milliseconds(),
				PromptTokens:     totalPrompt,
				CompletionTokens: totalCompletion,
				TotalTokens:      totalPrompt + totalCompletion,
				LLMCalls:         int64(i + 1),
				Mode:             ComplexitySimple,
				SessionMessages:  messages,
			}
		}
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			// Loop detection: check before dispatch.
			ld.Record(tc.Function.Name, tc.Function.Arguments)
			if det := ld.Detect(tc.Function.Name, tc.Function.Arguments); det.Stuck {
				if det.Level == loopdetect.Critical {
					return RunResult{
						Err:             fmt.Errorf("loop:%s: %s", det.Kind, det.Message),
						Steps:           steps,
						ElapsedMs:       time.Since(t0).Milliseconds(),
						SessionMessages: messages,
					}
				}
				// Warning: inject message so agent self-corrects.
				progress("think", "[loop-detect] "+det.Message)
				warnMsg := Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
					Content: "LOOP WARNING: " + det.Message}
				messages = append(messages, warnMsg)
				continue
			}

			progress("tool", fmt.Sprintf("%s(%s)", tc.Function.Name, truncate(tc.Function.Arguments, 80)))
			t1 := time.Now()
			result := dispatchWithState(t, rs, tc.Function.Name, tc.Function.Arguments)
			elapsedMs := time.Since(t1).Milliseconds()

			// Record outcome for loop detection.
			ld.RecordOutcome(tc.Function.Name, tc.Function.Arguments, result.Content, result.IsError)

			content := result.Content
			if len(content) > 6000 {
				content = content[:6000] + "\n...(truncated)"
			}
			label := "ok"
			if result.IsError {
				label = "err"
			}
			progress("result", fmt.Sprintf("[%s] %s", label, truncate(content, 200)))
			steps = append(steps, Step{
				ToolName: tc.Function.Name, ArgsJSON: tc.Function.Arguments,
				Result: content, IsError: result.IsError, ElapsedMs: elapsedMs,
			})
			toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: content}
			if tc.Function.Name == "todo_write" {
				toolMsg.Content += todoSummary(rs.Todos)
			}
			messages = append(messages, toolMsg)
		}
	}
	return RunResult{Err: fmt.Errorf("max iterations reached"), Steps: steps, ElapsedMs: time.Since(t0).Milliseconds(), SessionMessages: messages}
}
