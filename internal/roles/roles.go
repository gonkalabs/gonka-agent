// Package roles defines the 10 Plan-First Chain roles.
// Each role is a separate LLM call with its own system prompt and
// typed input/output. Plan Builder merges all outputs into FinalPlan.
package roles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ─── LLMPool ──────────────────────────────────────────────────────────────────

// LLMPool manages multiple LLMClient instances (one per API key).
// When multiple keys are provided, concurrent callers receive different
// clients — enabling true parallel inference across the planning chain.
// With a single key the behaviour is identical to a single LLMClient.
type LLMPool struct {
	clients []*LLMClient
	avail   chan int // buffered channel of available client indices
}

// NewLLMPool creates a pool from a slice of API keys.
// All clients share the same baseURL and model.
func NewLLMPool(baseURL, model string, apiKeys []string) *LLMPool {
	if len(apiKeys) == 0 {
		panic("NewLLMPool: at least one API key required")
	}
	clients := make([]*LLMClient, len(apiKeys))
	avail := make(chan int, len(apiKeys))
	for i, key := range apiKeys {
		clients[i] = NewLLMClient(baseURL, key, model)
		avail <- i
	}
	return &LLMPool{clients: clients, avail: avail}
}

// NewLLMPoolDualModel creates a pool where each client uses planModel for
// planning roles (fast/cheap) and a separate execute model is used by the
// execute phase client. If planModel is empty, model is used for both.
func NewLLMPoolDualModel(baseURL, planModel, fallbackModel string, apiKeys []string) *LLMPool {
	m := planModel
	if m == "" {
		m = fallbackModel
	}
	return NewLLMPool(baseURL, m, apiKeys)
}

// Size returns the number of clients (keys) in the pool.
func (p *LLMPool) Size() int { return len(p.clients) }

// BaseURL returns the base URL of the first client (all share the same URL).
func (p *LLMPool) BaseURL() string { return p.clients[0].BaseURL }

// APIKey returns the primary (first) API key.
func (p *LLMPool) APIKey() string { return p.clients[0].APIKey }

// Model returns the model of the first client.
func (p *LLMPool) Model() string { return p.clients[0].Model }

// Call acquires the first available client and executes the LLM request.
// If all clients are busy it blocks until one becomes free.
func (p *LLMPool) Call(roleID RoleID, systemPrompt, userMessage string) (string, error) {
	idx := <-p.avail
	defer func() { p.avail <- idx }()
	c := p.clients[idx]

	resp, err := c.callWithRetry(systemPrompt, userMessage)
	if err != nil {
		return "", err
	}
	// Role-block enforcement (same logic as LLMClient.Call).
	if !strings.Contains(resp, "[РОЛИ ПРИМЕНЕНЫ]") && !strings.Contains(resp, "[ROLES APPLIED]") {
		retry, err2 := c.callOnce(systemPrompt, userMessage+"\n\n"+roleEnforcementReminder)
		if err2 == nil && (strings.Contains(retry, "[РОЛИ ПРИМЕНЕНЫ]") || strings.Contains(retry, "[ROLES APPLIED]")) {
			return retry, nil
		}
	}
	return resp, nil
}

// TokenSummary returns aggregated token usage across all clients in the pool.
func (p *LLMPool) TokenSummary() string {
	var totalP, totalC, totalCalls int64
	for _, c := range p.clients {
		totalP += atomic.LoadInt64(&c.TotalPromptTokens)
		totalC += atomic.LoadInt64(&c.TotalCompletionTokens)
		totalCalls += atomic.LoadInt64(&c.TotalCalls)
	}
	return fmt.Sprintf("LLM calls: %d | prompt tokens: %d | completion tokens: %d | total tokens: %d [pool=%d keys]",
		totalCalls, totalP, totalC, totalP+totalC, len(p.clients))
}

// ─── Role IDs ────────────────────────────────────────────────────────────────

type RoleID string

const (
	RoleContextKeeper      RoleID = "context_keeper"
	RoleDeepAudit          RoleID = "deep_audit"
	RoleProtocolArchitect  RoleID = "protocol_architect"
	RoleSecurityReviewer   RoleID = "security_reviewer"
	RoleAPIBackendEngineer RoleID = "api_backend_engineer"
	RoleQATestEngineer     RoleID = "qa_test_engineer"
	RoleScientistValidator RoleID = "scientist_validator"
	RoleGitWorker          RoleID = "git_worker"
	RolePlanBuilder        RoleID = "plan_builder"
	RolePrePRValidator     RoleID = "pre_pr_validator"
)

// ─── Shared types ─────────────────────────────────────────────────────────────

// Location is a precise code location: file + line + symbol + scope status.
type Location struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Symbol        string `json:"symbol"`
	ScopeOK       bool   `json:"scope_ok"`       // target variable in scope?
	Context       string `json:"context"`         // ±3 lines around location
}

// EditStep is one planned code modification.
type EditStep struct {
	File                string `json:"file"`
	OldStr              string `json:"old_str"`
	NewStr              string `json:"new_str"`
	ExpectedOccurrences int    `json:"expected_occurrences"`
	ScopeVerified       bool   `json:"scope_verified"`
	Reason              string `json:"reason"`
}

// Risk describes a potential failure mode.
type Risk struct {
	Source      RoleID `json:"source"`
	Description string `json:"description"`
	Mitigation  string `json:"mitigation"`
}

// Verdict is the Scientist-Validator outcome.
type Verdict string

const (
	VerdictProven       Verdict = "PROVEN"
	VerdictInsufficient Verdict = "INSUFFICIENT"
	VerdictInvalid      Verdict = "INVALID"
	VerdictPending      Verdict = "PENDING"
)

// ─── Role outputs ─────────────────────────────────────────────────────────────

type ContextSnapshot struct {
	KnownFacts    []string `json:"known_facts"`
	UnknownGaps   []string `json:"unknown_gaps"`
	RelevantFiles []string `json:"relevant_files"`
}

type AuditResult struct {
	ExactLocations []Location `json:"exact_locations"`
	CallChain      []string   `json:"call_chain"`
	AffectedFiles  []string   `json:"affected_files"`
	RootCause      string     `json:"root_cause"`
}

type ProtoConstraints struct {
	APIChanges      []string `json:"api_changes"`
	VersionImpact   string   `json:"version_impact"`
	BreakingChanges []string `json:"breaking_changes"`
}

type SecurityRisks struct {
	Races              []string   `json:"races"`
	ScopeViolations    []Location `json:"scope_violations"`
	DangerousPatterns  []string   `json:"dangerous_patterns"`
	Mitigations        []string   `json:"mitigations"`
}

type TechnicalPlan struct {
	Steps          []EditStep `json:"steps"`
	NewFiles       []string   `json:"new_files"`
	BuildCommand   string     `json:"build_command"`
	TestCommand    string     `json:"test_command"`
}

type QAPlan struct {
	SuccessCriteria  []string          `json:"success_criteria"`
	TestsToRun       []string          `json:"tests_to_run"`
	EvidenceMetrics  []string          `json:"evidence_metrics"`
	MissingTestCases []string          `json:"missing_test_cases"`
	BaselineValues   map[string]string `json:"baseline_values"`
}

type ScientistFrame struct {
	Hypothesis     string  `json:"hypothesis"`
	Baseline       string  `json:"baseline"`
	Experiment     string  `json:"experiment"`
	ProvenCriteria string  `json:"proven_criteria"`
	Verdict        Verdict `json:"verdict"`
	VerdictReason  string  `json:"verdict_reason"`
}

type GitPlan struct {
	FilesToModify  []string `json:"files_to_modify"`
	RollbackSteps  []string `json:"rollback_steps"`
	CommitStrategy string   `json:"commit_strategy"`
}

type PreVerifyResult struct {
	Verdict  string   `json:"verdict"` // "GO" or "BLOCK"
	Blockers []string `json:"blockers"`
}

// FinalPlan is the Plan Builder's merged output — the only thing that
// passes to the PRE_VERIFY and EXECUTE phases.
type FinalPlan struct {
	Goal             string     `json:"goal"`
	Steps            []PlanStep `json:"steps"`
	RiskRegister     []Risk     `json:"risk_register"`
	ReadinessCriteria []string  `json:"readiness_criteria"`
	RollbackPlan     string     `json:"rollback_plan"`
	TechnicalPlan    TechnicalPlan `json:"technical_plan"`
	QAPlan           QAPlan     `json:"qa_plan"`
	ScientistFrame   ScientistFrame `json:"scientist_frame"`
	GitPlan          GitPlan    `json:"git_plan"`
}

type PlanStep struct {
	Index      int    `json:"index"`
	Role       RoleID `json:"role"`
	Action     string `json:"action"`
	VerifyHow  string `json:"verify_how"`
	Blocking   bool   `json:"blocking"`
}

// ─── AllOutputs carries all role outputs for Plan Builder ────────────────────

type AllOutputs struct {
	Context    ContextSnapshot
	Audit      AuditResult
	Proto      ProtoConstraints
	Security   SecurityRisks
	Technical  TechnicalPlan
	QA         QAPlan
	Scientist  ScientistFrame
	Git        GitPlan
}

// ─── LLM Client ──────────────────────────────────────────────────────────────

// LLMClient enforces single-flight inference: Gonka allows one active
// inference per wallet. The semaphore ensures no second call starts
// until the first completes — regardless of call duration.
type LLMClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
	sem     chan struct{} // capacity 1 = one inference at a time

	// Token counters — updated atomically after each successful call.
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TotalCalls            int64
}

// TokenSummary returns a human-readable token usage line.
func (c *LLMClient) TokenSummary() string {
	p := atomic.LoadInt64(&c.TotalPromptTokens)
	comp := atomic.LoadInt64(&c.TotalCompletionTokens)
	calls := atomic.LoadInt64(&c.TotalCalls)
	return fmt.Sprintf("LLM calls: %d | prompt tokens: %d | completion tokens: %d | total tokens: %d",
		calls, p, comp, p+comp)
}

func NewLLMClient(baseURL, apiKey, model string) *LLMClient {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // pre-fill: first caller takes it immediately
	return &LLMClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 360 * time.Second},
		sem:     sem,
	}
}

func (c *LLMClient) acquire() { <-c.sem }
func (c *LLMClient) release() { c.sem <- struct{}{} }

type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Call sends an LLM request. Blocks until the previous Call() completes
// (single-flight semaphore), then retries on transient errors.
func (c *LLMClient) Call(roleID RoleID, systemPrompt, userMessage string) (string, error) {
	c.acquire()
	defer c.release()

	resp, err := c.callWithRetry(systemPrompt, userMessage)
	if err != nil {
		return "", err
	}
	// Enforce mandatory role block.
	if !strings.Contains(resp, "[РОЛИ ПРИМЕНЕНЫ]") && !strings.Contains(resp, "[ROLES APPLIED]") {
		retry, err2 := c.callOnce(systemPrompt, userMessage+"\n\n"+roleEnforcementReminder)
		if err2 == nil && (strings.Contains(retry, "[РОЛИ ПРИМЕНЕНЫ]") || strings.Contains(retry, "[ROLES APPLIED]")) {
			return retry, nil
		}
		// Accept anyway — log the violation but don't block execution.
	}
	return resp, nil
}

func clampLen(a, b int) int {
	if a < b { return a }
	return b
}

const roleEnforcementReminder = `IMPORTANT: Your response MUST begin with:
[РОЛИ ПРИМЕНЕНЫ]
- [your role]: [what you identified]
- Вывод: [conclusion]
Without this block your response is considered invalid. Please repeat your answer with this block.`

// Call wraps callOnce with retry on rate-limit (429) and transient errors.
// "empty choices" means Gonka wallet is busy (1-inference-per-wallet limit) — retryable.
func (c *LLMClient) callWithRetry(systemPrompt, userMessage string) (string, error) {
	// Longer backoff for wallet-busy (empty choices) vs transient network errors.
	backoff := []time.Duration{8 * time.Second, 20 * time.Second, 45 * time.Second, 90 * time.Second}
	var lastErr error
	for attempt, wait := range backoff {
		resp, err := c.callOnce(systemPrompt, userMessage)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		errStr := err.Error()
		isRetryable := strings.Contains(errStr, "rate limit") ||
			strings.Contains(errStr, "502") ||
			strings.Contains(errStr, "503") ||
			strings.Contains(errStr, "context deadline") ||
			strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "EOF") ||
			strings.Contains(errStr, "empty choices") || // Gonka wallet busy
			strings.Contains(errStr, "i/o timeout")
		if !isRetryable {
			return "", err
		}
		fmt.Printf("[LLM retry %d/%d] %s — waiting %s\n", attempt+1, len(backoff), errStr[:clampLen(80, len(errStr))], wait)
		time.Sleep(wait)
	}
	return "", fmt.Errorf("all retries exhausted: %w", lastErr)
}

func (c *LLMClient) callOnce(systemPrompt, userMessage string) (string, error) {
	messages := []llmMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMessage},
	}
	body, _ := json.Marshal(map[string]any{
		"model":    c.Model,
		"messages": messages,
	})
	req, err := http.NewRequest("POST", c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode: %w\nbody: %s", err, string(raw))
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return "", fmt.Errorf("API error: %s", string(r.Error))
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("empty choices from API")
	}
	// Accumulate token usage atomically.
	atomic.AddInt64(&c.TotalPromptTokens, int64(r.Usage.PromptTokens))
	atomic.AddInt64(&c.TotalCompletionTokens, int64(r.Usage.CompletionTokens))
	atomic.AddInt64(&c.TotalCalls, 1)
	return r.Choices[0].Message.Content, nil
}

// ─── System prompts ───────────────────────────────────────────────────────────

const ContextKeeperPrompt = `You are Context Keeper in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: workspace state. Read files, build a picture of what exists.
ONLY use read-only tools (read_file, list_dir, glob, detect_build_system).
NEVER suggest edits or run commands.

Output a ContextSnapshot JSON with:
- known_facts: list of confirmed facts about the codebase (language, build system, key files)
- unknown_gaps: what information is missing to complete the plan
- relevant_files: files that need to be read/modified for this task

Rules:
- Use detect_build_system to determine project language and build commands.
- Never assume Go or any specific language — the project may be TypeScript, Python, Rust, etc.
- Never state a fact as CONFIRMED unless you read the actual file or got a real API response.`

const DeepAuditPrompt = `You are Deep Audit / Scalpel in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: trace to scalar level. Not "affects area X" but "file src/tools.ts, line 192,
variable searchPath is undefined in scope because it belongs to GrepSearch not GlobSearch."

For each planned change:
1. Find the EXACT line number
2. Count EXACT occurrences of the target string
3. For EACH occurrence: is the target variable/symbol in scope?
4. Build the full call chain: A calls B calls C
5. List ALL files that would break if this code is modified

Use grep_search to count occurrences. Use view_range to read exact context.
Do NOT assume file extensions — the project may be .ts, .py, .rs, .go, etc.

Output an AuditResult JSON with exact_locations (file+line+symbol+scope_ok),
call_chain, affected_files, root_cause.`

const ProtocolArchitectPrompt = `You are Protocol Architect in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: API contracts, interface changes, MCP protocol compliance,
tool schemas, function signatures, breaking changes.

Given the planned technical changes, identify:
- Any public API or interface that changes (function signatures, return types)
- Backward compatibility: does anything break for callers?
- MCP tool schema changes: if tool parameters change, all callers must update
- Version impact: is this a patch, minor, or major change?

Output a ProtoConstraints JSON with api_changes, version_impact, breaking_changes.
If no protocol concerns: state that explicitly.`

const SecurityReviewerPrompt = `You are Security Reviewer in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: safety of code changes. Focus on:
- Scope violations: replacing text in a scope where a referenced variable doesn't exist
- Race conditions: concurrent access to shared state
- Path traversal / workspace escape in file operations
- Injection in shell commands (run_command with user-controlled strings)
- Dangerous patterns: rm -rf, format, overwrite critical files

For each planned EditStep: verify the old_str is unique enough and the scope is safe.
For shell commands: verify they are bounded and not destructive.

Output a SecurityRisks JSON with races, scope_violations (as Location objects),
dangerous_patterns, mitigations.`

const APIBackendEngineerPrompt = `You are API/Backend Engineer in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: technical correctness of the implementation plan.
Given the task, audit result, protocol constraints and security risks — build
a precise TechnicalPlan with exact EditSteps.

For each EditStep specify:
- file: exact path (use the actual language file extension, not .go unless it is Go)
- old_str: the EXACT string to find (must be unique in context)
- new_str: exact replacement
- expected_occurrences: how many times old_str appears (from Deep Audit)
- scope_verified: confirmed by Deep Audit that all occurrences are safe
- reason: why this change is needed

Also specify:
- build_command: the ACTUAL build command for this project (e.g. "npm run build", "cargo build", "go build ./...")
- test_command: the ACTUAL test command (e.g. "npm test", "cargo test", "pytest")
- new_files: any new files to create

IMPORTANT: use the build_command and test_command from the project's detected build system.
Do NOT default to "go build ./..." unless the project is actually Go.

Output a TechnicalPlan JSON. If Deep Audit found scope violations in planned edits,
redesign the edit to use unique strings that only appear in safe scopes.`

const QATestEngineerPrompt = `You are QA/Test Engineer in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: tests, validation, Evidence Table, acceptance criteria.

Given the TechnicalPlan and the project's detected build system, define:
- success_criteria: measurable, not "works" but "build exits 0, tests pass,
  output of function X for input Y equals Z"
- tests_to_run: exact test commands appropriate for the project language
  (e.g. "npm test", "pytest -v", "cargo test", "go test ./...")
- evidence_metrics: what to measure before and after (build errors, test count,
  specific function output)
- missing_test_cases: tests that don't exist yet but are needed
- baseline_values: current state before the change (e.g. "build: 0 errors", "test: 3 pass")

IMPORTANT: use the test command from the project's detected build system.
Do NOT default to "go test ./..." unless the project is actually Go.

Note: 20 unit tests = INSUFFICIENT for proving a feature works end-to-end.
Define at least one integration-level check.

Output a QAPlan JSON.`

const ScientistValidatorPrompt = `You are Scientist-Validator in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: scientific validity of the result.

Ask: is there a testable hypothesis? Is there a baseline (before) and experiment (after)?
Can the result be reproduced without private keys or closed infrastructure?

Your verdict options:
- PROVEN: data proves the thesis (measurable, reproducible, specific)
- INSUFFICIENT: data exists but is not enough (say what's missing)
- INVALID: methodology does not allow a conclusion

Note: "go build passes" = INSUFFICIENT alone. You need: the feature actually works
as specified, demonstrated by a concrete test or observable output.

Before execution: set verdict to PENDING with the hypothesis.
After validation: update verdict to PROVEN/INSUFFICIENT/INVALID.

Output a ScientistFrame JSON.`

const GitWorkerPrompt = `You are Git Worker in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

Your zone: version control strategy.

Given the TechnicalPlan's list of files to modify:
- List all files_to_modify
- Define rollback_steps: for each file, how to revert (git checkout path, or manual)
- Define commit_strategy: what to commit when, in what order

Output a GitPlan JSON. Keep it simple — one commit per logical change unit.`

const PlanBuilderPrompt = `You are Plan Builder in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

You are the COORDINATOR and MERGER. You receive outputs from ALL other roles
and produce the single FinalPlan that governs execution.

Your rules:
1. NEVER invent new technical steps — only consolidate what roles provided
2. If any role identified a risk: it MUST appear in risk_register
3. If Security or Audit found a scope violation: the TechnicalPlan EditStep
   MUST be redesigned (use unique strings, split into separate replaces)
4. Steps must be ordered: verify assumptions → make changes → build → test → validate
5. Every step must have a verify_how: how do we confirm it succeeded?
6. Use the build_command and test_command from TechnicalPlan as-is.
   Do NOT replace them with "go build ./..." unless that is actually what was provided.

Output a FinalPlan JSON. This plan — and only this plan — drives execution.
If any role's output is missing or contradicts another, flag it explicitly.`

const PrePRValidatorPrompt = `You are Pre-PR Validator in the Plan-First Chain.

[РОЛИ ПРИМЕНЕНЫ] block is MANDATORY at the start of your response.

You are the GATE before any execution begins.

Mandatory checklist — all items, not selective:
1. For each EditStep in TechnicalPlan: verify_replace shows expected_occurrences count
2. For each occurrence: is the referenced variable/symbol in scope?
3. dependency_graph confirms no importer will break
4. get_diagnostics confirms no EXISTING errors before our changes
5. No dangerous patterns (rm, format, overwrite of non-workspace files)

Verdict:
- GO: all checks passed, execution may begin
- BLOCK: list every blocker with exact file:line and reason

A BLOCK returns execution to Deep Audit for a new planning cycle.
Output a PreVerifyResult JSON.`
