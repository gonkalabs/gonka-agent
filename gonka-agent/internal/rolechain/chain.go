// Package rolechain runs the Plan-First Chain roles in sequence (and in parallel
// where roles are independent). LLMPool dispatches to the first available client,
// enabling concurrent inference when multiple API keys are provided.
package rolechain

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

// Chain orchestrates the role sequence for one planning cycle.
type Chain struct {
	llm      *roles.LLMPool
	tools    *tools.Cfg
	task     string
	progress func(event, detail string) // nil-safe: always set in New()
	buildInfo tools.BuildInfo           // detected once at the start of Run()
}

func New(pool *roles.LLMPool, t *tools.Cfg, task string, progress func(string, string)) *Chain {
	if progress == nil {
		progress = func(_, _ string) {}
	}
	return &Chain{llm: pool, tools: t, task: task, progress: progress}
}

// Run executes the full role chain and returns a FinalPlan.
// Roles that are independent of each other run in parallel when the pool
// has 2+ clients (i.e. 2+ API keys are provided).
func (c *Chain) Run() (*roles.FinalPlan, error) {
	// Detect project language and build system once — used by all roles below.
	c.buildInfo = c.tools.DetectBuildSystem()
	c.log("=== PHASE: UNDERSTAND === (lang=%s build=%q pool=%d keys)",
		c.buildInfo.Language, c.buildInfo.BuildCmd, c.llm.Size())

	ctxSnap, err := c.runContextKeeperCached()
	if err != nil {
		return nil, fmt.Errorf("ContextKeeper: %w", err)
	}
	c.log("ContextKeeper: %d known facts, %d gaps, %d relevant files",
		len(ctxSnap.KnownFacts), len(ctxSnap.UnknownGaps), len(ctxSnap.RelevantFiles))

	audit, err := c.runDeepAudit(ctxSnap)
	if err != nil {
		return nil, fmt.Errorf("DeepAudit: %w", err)
	}
	c.log("DeepAudit: %d locations, %d affected files, root: %s",
		len(audit.ExactLocations), len(audit.AffectedFiles), shorten(audit.RootCause, 80))

	c.log("=== PHASE: HARD THINK === (parallel=%v)", c.llm.Size() >= 2)

	// ProtocolArchitect and SecurityReviewer both depend only on AuditResult —
	// run them in parallel when 2+ keys are available.
	// Small stagger (300ms) ensures Gonka's 1-inference-per-wallet sees sequential
	// starts even if the underlying wallets share a rate-limit window.
	var proto roles.ProtoConstraints
	var sec roles.SecurityRisks
	if c.llm.Size() >= 2 {
		var protoErr, secErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); proto, protoErr = c.runProtocolArchitect(audit) }()
		time.Sleep(300 * time.Millisecond) // stagger start
		go func() { defer wg.Done(); sec, secErr = c.runSecurityReviewer(audit) }()
		wg.Wait()
		if protoErr != nil {
			return nil, fmt.Errorf("ProtocolArchitect: %w", protoErr)
		}
		if secErr != nil {
			return nil, fmt.Errorf("SecurityReviewer: %w", secErr)
		}
	} else {
		if proto, err = c.runProtocolArchitect(audit); err != nil {
			return nil, fmt.Errorf("ProtocolArchitect: %w", err)
		}
		if sec, err = c.runSecurityReviewer(audit); err != nil {
			return nil, fmt.Errorf("SecurityReviewer: %w", err)
		}
	}
	c.log("Proto: %d API changes | Security: %d scope violations, %d races",
		len(proto.APIChanges), len(sec.ScopeViolations), len(sec.Races))

	tech, err := c.runAPIBackendEngineer(audit, proto, sec)
	if err != nil {
		return nil, fmt.Errorf("APIBackendEngineer: %w", err)
	}
	c.log("TechnicalPlan: %d edit steps, build: %s", len(tech.Steps), tech.BuildCommand)

	// QATestEngineer and GitWorker both depend only on TechnicalPlan —
	// run them in parallel when 2+ keys are available.
	var qa roles.QAPlan
	var git roles.GitPlan
	if c.llm.Size() >= 2 {
		var qaErr, gitErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); qa, qaErr = c.runQATestEngineer(tech) }()
		time.Sleep(300 * time.Millisecond)
		go func() { defer wg.Done(); git, gitErr = c.runGitWorker(tech) }()
		wg.Wait()
		if qaErr != nil {
			return nil, fmt.Errorf("QATestEngineer: %w", qaErr)
		}
		if gitErr != nil {
			return nil, fmt.Errorf("GitWorker: %w", gitErr)
		}
	} else {
		if qa, err = c.runQATestEngineer(tech); err != nil {
			return nil, fmt.Errorf("QATestEngineer: %w", err)
		}
		if git, err = c.runGitWorker(tech); err != nil {
			return nil, fmt.Errorf("GitWorker: %w", err)
		}
	}
	c.log("QA: %d success criteria, %d tests | Git: %d files",
		len(qa.SuccessCriteria), len(qa.TestsToRun), len(git.FilesToModify))

	sci, err := c.runScientistValidator(tech, qa)
	if err != nil {
		return nil, fmt.Errorf("ScientistValidator: %w", err)
	}
	c.log("Scientist: hypothesis=%s, verdict=%s", shorten(sci.Hypothesis, 60), sci.Verdict)

	c.log("=== PHASE: PLAN BUILDER ===")

	all := roles.AllOutputs{
		Context: ctxSnap, Audit: audit, Proto: proto, Security: sec,
		Technical: tech, QA: qa, Scientist: sci, Git: git,
	}
	plan, err := c.runPlanBuilder(all)
	if err != nil {
		return nil, fmt.Errorf("PlanBuilder: %w", err)
	}
	c.log("FinalPlan: %d steps, %d risks", len(plan.Steps), len(plan.RiskRegister))
	return plan, nil
}

// RunPartial runs a 3-role chain (ContextKeeper → DeepAudit → APIBackendEngineer)
// and returns a synthetic FinalPlan. Suitable for medium-complexity tasks.
// Saves ~5 LLM calls compared to the full Run().
func (c *Chain) RunPartial() (*roles.FinalPlan, error) {
	c.buildInfo = c.tools.DetectBuildSystem()
	c.log("=== PARTIAL CHAIN (3 roles) lang=%s pool=%d keys ===", c.buildInfo.Language, c.llm.Size())

	ctxSnap, err := c.runContextKeeperCached()
	if err != nil {
		return nil, fmt.Errorf("ContextKeeper: %w", err)
	}
	c.log("ContextKeeper: %d facts, %d files", len(ctxSnap.KnownFacts), len(ctxSnap.RelevantFiles))

	audit, err := c.runDeepAudit(ctxSnap)
	if err != nil {
		return nil, fmt.Errorf("DeepAudit: %w", err)
	}
	c.log("DeepAudit: %d locations, root: %s", len(audit.ExactLocations), shorten(audit.RootCause, 80))

	tech, err := c.runAPIBackendEngineer(audit, roles.ProtoConstraints{}, roles.SecurityRisks{})
	if err != nil {
		return nil, fmt.Errorf("APIBackendEngineer: %w", err)
	}
	c.log("TechnicalPlan: %d edit steps, build: %s", len(tech.Steps), tech.BuildCommand)

	// Synthetic FinalPlan — no PlanBuilder LLM call needed.
	buildCmd := tech.BuildCommand
	if buildCmd == "" { buildCmd = c.buildInfo.BuildCmd }
	plan := &roles.FinalPlan{
		Goal:          c.task,
		TechnicalPlan: tech,
		QAPlan: roles.QAPlan{
			SuccessCriteria: []string{fmt.Sprintf("build exits 0 (%s)", buildCmd)},
			TestsToRun:      []string{tech.TestCommand},
		},
		ScientistFrame: roles.ScientistFrame{
			Verdict:    roles.VerdictPending,
			Hypothesis: c.task,
		},
		GitPlan: roles.GitPlan{
			FilesToModify: audit.AffectedFiles,
		},
	}
	return plan, nil
}

// ─── Individual role runners ──────────────────────────────────────────────────

// runContextKeeperCached checks the disk cache before calling the LLM.
func (c *Chain) runContextKeeperCached() (roles.ContextSnapshot, error) {
	if snap, bi, ok := loadCache(c.tools.Workspace, c.buildInfo); ok {
		c.buildInfo = bi
		c.log("ContextKeeper: CACHE HIT — skipping LLM call")
		return snap, nil
	}
	snap, err := c.runContextKeeper()
	if err == nil {
		saveCache(c.tools.Workspace, snap, c.buildInfo)
	}
	return snap, err
}

func (c *Chain) runContextKeeper() (roles.ContextSnapshot, error) {
	listing := c.tools.ListDir(".")

	// Read project manifest based on detected build system (not hardcoded to go.mod).
	var manifestContent string
	if c.buildInfo.Manifest != "" {
		r := c.tools.ReadFile(c.buildInfo.Manifest)
		if !r.IsError {
			manifestContent = r.Content
		}
	}

	// Discover relevant source files dynamically using SourceExts from BuildInfo.
	var relevantFiles []string
	if c.buildInfo.Manifest != "" {
		relevantFiles = append(relevantFiles, c.buildInfo.Manifest)
	}
	for _, line := range strings.Split(listing.Content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, ext := range c.buildInfo.SourceExts {
			if strings.HasSuffix(line, ext) {
				r := c.tools.ReadFile(line)
				if !r.IsError {
					relevantFiles = append(relevantFiles, line)
				}
				break
			}
		}
		// Cap at 10 files to avoid oversized prompts.
		if len(relevantFiles) >= 10 {
			break
		}
	}

	knownFacts := []string{
		"workspace: " + c.tools.Workspace,
		"language: " + c.buildInfo.Language,
		"build_command: " + c.buildInfo.BuildCmd,
		"test_command: " + c.buildInfo.TestCmd,
		"manifest: " + c.buildInfo.Manifest,
		"listing (top 300 chars): " + listing.Content[:minInt(300, len(listing.Content))],
	}
	if manifestContent != "" {
		knownFacts = append(knownFacts, c.buildInfo.Manifest+": "+strings.TrimSpace(manifestContent))
	}

	userMsg := fmt.Sprintf(
		"Task: %s\n\nProject language: %s\nBuild command: %s\nTest command: %s\n"+
			"Manifest file: %s\n\nWorkspace listing:\n%s\n\nManifest content:\n%s\n\n"+
			`Respond ONLY with JSON: {"known_facts":["..."],"unknown_gaps":["..."],"relevant_files":["..."]}`,
		c.task,
		c.buildInfo.Language, c.buildInfo.BuildCmd, c.buildInfo.TestCmd,
		c.buildInfo.Manifest,
		listing.Content,
		manifestContent)

	resp, err := c.llm.Call(roles.RoleContextKeeper, roles.ContextKeeperPrompt, userMsg)
	if err != nil {
		return roles.ContextSnapshot{KnownFacts: knownFacts, RelevantFiles: relevantFiles}, nil
	}
	snap, _ := extractJSON[roles.ContextSnapshot](resp, roles.ContextSnapshot{})
	if len(snap.KnownFacts) == 0 {
		snap.KnownFacts = knownFacts
	} else {
		snap.KnownFacts = append(knownFacts, snap.KnownFacts...)
	}
	if len(snap.RelevantFiles) == 0 {
		snap.RelevantFiles = relevantFiles
	}
	return snap, nil
}

func (c *Chain) runDeepAudit(ctx roles.ContextSnapshot) (roles.AuditResult, error) {
	var fileContents strings.Builder
	for _, f := range ctx.RelevantFiles {
		r := c.tools.ReadFile(f)
		if !r.IsError {
			content := r.Content
			if len(content) > 6000 {
				content = content[:6000] + "\n...(truncated)"
			}
			fileContents.WriteString(fmt.Sprintf("\n--- %s ---\n%s\n", f, content))
		}
	}

	userMsg := fmt.Sprintf(
		"Task: %s\n\nFacts: %v\n\nFiles:\n%s\n\n"+
			`Respond ONLY with JSON: {"exact_locations":[{"file":"","line":0,"symbol":"","scope_ok":true,"context":""}],"call_chain":[],"affected_files":[],"root_cause":""}`,
		c.task, ctx.KnownFacts, fileContents.String())

	resp, err := c.llm.Call(roles.RoleDeepAudit, roles.DeepAuditPrompt, userMsg)
	if err != nil {
		return roles.AuditResult{AffectedFiles: ctx.RelevantFiles, RootCause: err.Error()}, nil
	}
	result, _ := extractJSON[roles.AuditResult](resp, roles.AuditResult{})
	if len(result.AffectedFiles) == 0 {
		result.AffectedFiles = ctx.RelevantFiles
	}
	if result.RootCause == "" {
		result.RootCause = "see file contents above"
	}
	return result, nil
}

func (c *Chain) runProtocolArchitect(audit roles.AuditResult) (roles.ProtoConstraints, error) {
	userMsg := fmt.Sprintf("Task: %s\n\nAudit root cause: %s\nAffected files: %v\n\n"+
		`Respond ONLY with JSON: {"api_changes":[],"version_impact":"patch","breaking_changes":[]}`,
		c.task, audit.RootCause, audit.AffectedFiles)
	resp, err := c.llm.Call(roles.RoleProtocolArchitect, roles.ProtocolArchitectPrompt, userMsg)
	if err != nil {
		return roles.ProtoConstraints{VersionImpact: "patch"}, nil
	}
	r, _ := extractJSON[roles.ProtoConstraints](resp, roles.ProtoConstraints{VersionImpact: "patch"})
	return r, nil
}

func (c *Chain) runSecurityReviewer(audit roles.AuditResult) (roles.SecurityRisks, error) {
	userMsg := fmt.Sprintf("Task: %s\n\nAudit root cause: %s\n\n"+
		`Respond ONLY with JSON: {"races":[],"scope_violations":[],"dangerous_patterns":[],"mitigations":[]}`,
		c.task, audit.RootCause)
	resp, err := c.llm.Call(roles.RoleSecurityReviewer, roles.SecurityReviewerPrompt, userMsg)
	if err != nil {
		return roles.SecurityRisks{}, nil
	}
	r, _ := extractJSON[roles.SecurityRisks](resp, roles.SecurityRisks{})
	return r, nil
}

func (c *Chain) runAPIBackendEngineer(
	audit roles.AuditResult,
	proto roles.ProtoConstraints,
	sec roles.SecurityRisks,
) (roles.TechnicalPlan, error) {
	// If AuditResult found no affected files, discover via list_dir.
	// Use detected SourceExts — not hardcoded .go.
	affectedFiles := audit.AffectedFiles
	if len(affectedFiles) == 0 {
		r := c.tools.ListDir(".")
		if !r.IsError {
			for _, line := range strings.Split(r.Content, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if len(c.buildInfo.SourceExts) == 0 {
					// Unknown project type — include all non-binary files.
					if !strings.HasPrefix(line, ".") {
						affectedFiles = append(affectedFiles, line)
					}
				} else {
					for _, ext := range c.buildInfo.SourceExts {
						if strings.HasSuffix(line, ext) {
							affectedFiles = append(affectedFiles, line)
							break
						}
					}
				}
			}
		}
	}

	var fileSections strings.Builder
	for _, f := range affectedFiles {
		r := c.tools.ReadFile(f)
		if !r.IsError {
			content := r.Content
			if len(content) > 5000 {
				content = content[:5000] + "\n...(truncated)"
			}
			fileSections.WriteString(fmt.Sprintf("\n--- %s ---\n%s\n", f, content))
		}
	}
	defaultBuild := c.buildInfo.BuildCmd
	defaultTest := c.buildInfo.TestCmd

	userMsg := fmt.Sprintf(
		"Task: %s\n\nProject language: %s\nBuild command: %s\nTest command: %s\n\n"+
			"Root cause: %s\nSecurity: %v\n\nFile contents (read verbatim — copy old_str EXACTLY from these):\n%s\n\n"+
			"RULE: old_str MUST be copied character-for-character from the file content above. Never invent old_str.\n"+
			"Use the actual build_command and test_command for this project language, not 'go build ./...' unless it is Go.\n"+
			`Respond ONLY with JSON: {"steps":[{"file":"","old_str":"","new_str":"","expected_occurrences":1,"scope_verified":true,"reason":""}],"new_files":[],"build_command":"%s","test_command":"%s"}`,
		c.task,
		c.buildInfo.Language, defaultBuild, defaultTest,
		audit.RootCause, sec.DangerousPatterns, fileSections.String(),
		defaultBuild, defaultTest)
	resp, err := c.llm.Call(roles.RoleAPIBackendEngineer, roles.APIBackendEngineerPrompt, userMsg)
	if err != nil {
		return roles.TechnicalPlan{BuildCommand: defaultBuild, TestCommand: defaultTest}, nil
	}
	r, _ := extractJSON[roles.TechnicalPlan](resp, roles.TechnicalPlan{BuildCommand: defaultBuild, TestCommand: defaultTest})
	if r.BuildCommand == "" {
		r.BuildCommand = defaultBuild
	}
	if r.TestCommand == "" {
		r.TestCommand = defaultTest
	}
	return r, nil
}

func (c *Chain) runQATestEngineer(tech roles.TechnicalPlan) (roles.QAPlan, error) {
	buildCmd := tech.BuildCommand
	if buildCmd == "" {
		buildCmd = c.buildInfo.BuildCmd
	}
	testCmd := tech.TestCommand
	if testCmd == "" {
		testCmd = c.buildInfo.TestCmd
	}
	defaultCriteria := fmt.Sprintf("build exits 0 (%s)", buildCmd)

	userMsg := fmt.Sprintf(
		"Task: %s\n\nProject language: %s\nBuild command: %s\nTest command: %s\n%d edit steps planned.\n\n"+
			`Respond ONLY with JSON: {"success_criteria":["%s"],"tests_to_run":[],"evidence_metrics":[],"missing_test_cases":[],"baseline_values":{}}`,
		c.task, c.buildInfo.Language, buildCmd, testCmd, len(tech.Steps), defaultCriteria)
	resp, err := c.llm.Call(roles.RoleQATestEngineer, roles.QATestEngineerPrompt, userMsg)
	if err != nil {
		return roles.QAPlan{SuccessCriteria: []string{defaultCriteria}}, nil
	}
	r, _ := extractJSON[roles.QAPlan](resp, roles.QAPlan{})
	if len(r.SuccessCriteria) == 0 {
		r.SuccessCriteria = []string{defaultCriteria}
	}
	return r, nil
}

func (c *Chain) runScientistValidator(tech roles.TechnicalPlan, qa roles.QAPlan) (roles.ScientistFrame, error) {
	userMsg := fmt.Sprintf("Task: %s\n\nSteps: %d. Criteria: %v\n\n"+
		`Respond ONLY with JSON: {"hypothesis":"","baseline":"before change","experiment":"after change","proven_criteria":"","verdict":"PENDING","verdict_reason":""}`,
		c.task, len(tech.Steps), qa.SuccessCriteria)
	resp, err := c.llm.Call(roles.RoleScientistValidator, roles.ScientistValidatorPrompt, userMsg)
	if err != nil {
		return roles.ScientistFrame{Verdict: roles.VerdictPending, Hypothesis: c.task}, nil
	}
	r, _ := extractJSON[roles.ScientistFrame](resp, roles.ScientistFrame{Verdict: roles.VerdictPending})
	if r.Verdict == "" {
		r.Verdict = roles.VerdictPending
	}
	if r.Hypothesis == "" {
		r.Hypothesis = c.task
	}
	return r, nil
}

func (c *Chain) runGitWorker(tech roles.TechnicalPlan) (roles.GitPlan, error) {
	files := make([]string, 0, len(tech.Steps))
	seen := map[string]bool{}
	for _, s := range tech.Steps {
		if !seen[s.File] {
			files = append(files, s.File)
			seen[s.File] = true
		}
	}
	userMsg := fmt.Sprintf("Task: %s\n\nFiles: %v\n\n"+
		`Respond ONLY with JSON: {"files_to_modify":[],"rollback_steps":[],"commit_strategy":"one commit per logical change"}`,
		c.task, files)
	resp, err := c.llm.Call(roles.RoleGitWorker, roles.GitWorkerPrompt, userMsg)
	if err != nil {
		return roles.GitPlan{FilesToModify: files}, nil
	}
	r, _ := extractJSON[roles.GitPlan](resp, roles.GitPlan{})
	if len(r.FilesToModify) == 0 {
		r.FilesToModify = files
	}
	return r, nil
}

func (c *Chain) runPlanBuilder(all roles.AllOutputs) (*roles.FinalPlan, error) {
	// Compact summary — avoid 504 from oversized prompts on 235B model.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task: %s\n", c.task))
	sb.WriteString(fmt.Sprintf("Root cause: %s\n", all.Audit.RootCause))
	sb.WriteString(fmt.Sprintf("Files to change: %v\n", all.Audit.AffectedFiles))
	sb.WriteString(fmt.Sprintf("Edit steps: %d\n", len(all.Technical.Steps)))
	for i, s := range all.Technical.Steps {
		sb.WriteString(fmt.Sprintf("  %d. %s in %s\n", i+1, shorten(s.Reason, 80), s.File))
	}
	sb.WriteString(fmt.Sprintf("Build: %s\n", all.Technical.BuildCommand))
	sb.WriteString(fmt.Sprintf("Success: %v\n", all.QA.SuccessCriteria))
	sb.WriteString(fmt.Sprintf("Hypothesis: %s\n", all.Scientist.Hypothesis))
	sb.WriteString(fmt.Sprintf("Risks: %v\n", all.Security.DangerousPatterns))

	buildCmd := all.Technical.BuildCommand
	if buildCmd == "" {
		buildCmd = c.buildInfo.BuildCmd
	}
	testCmd := all.Technical.TestCommand
	if testCmd == "" {
		testCmd = c.buildInfo.TestCmd
	}

	userMsg := sb.String() + fmt.Sprintf("\n"+`Respond ONLY with JSON:
{"goal":"","steps":[{"index":1,"role":"api_backend_engineer","action":"","verify_how":"","blocking":true}],"risk_register":[],"readiness_criteria":[],"rollback_plan":"","technical_plan":{"steps":[],"build_command":"%s","test_command":"%s"},"qa_plan":{"success_criteria":[],"tests_to_run":[]},"scientist_frame":{"hypothesis":"","verdict":"PENDING"},"git_plan":{"files_to_modify":[],"rollback_steps":[]}}`,
		buildCmd, testCmd)

	resp, err := c.llm.Call(roles.RolePlanBuilder, roles.PlanBuilderPrompt, userMsg)
	if err != nil {
		return nil, err
	}
	plan, _ := extractJSON[roles.FinalPlan](resp, roles.FinalPlan{})
	if len(plan.TechnicalPlan.Steps) == 0 {
		plan.TechnicalPlan = all.Technical
	}
	if len(plan.QAPlan.SuccessCriteria) == 0 {
		plan.QAPlan = all.QA
	}
	plan.ScientistFrame = all.Scientist
	plan.GitPlan = all.Git
	if plan.Goal == "" {
		plan.Goal = c.task
	}
	return &plan, nil
}

// ─── Pre-PR Validator ─────────────────────────────────────────────────────────

func (c *Chain) PreVerify(plan *roles.FinalPlan) (roles.PreVerifyResult, error) {
	var verifyReport strings.Builder
	for i, step := range plan.TechnicalPlan.Steps {
		r := c.tools.VerifyReplace(step.File, step.OldStr)
		verifyReport.WriteString(fmt.Sprintf("\nStep %d (%s): %s\n", i+1, step.File, r.Content[:minInt(200, len(r.Content))]))
	}

	userMsg := fmt.Sprintf("Task: %s\n\nSteps: %d\nVerify results:\n%s\n\n"+
		`Respond ONLY with JSON: {"verdict":"GO","blockers":[]}`,
		c.task, len(plan.TechnicalPlan.Steps), verifyReport.String())

	resp, err := c.llm.Call(roles.RolePrePRValidator, roles.PrePRValidatorPrompt, userMsg)
	if err != nil {
		return roles.PreVerifyResult{Verdict: "GO"}, nil
	}
	r, _ := extractJSON[roles.PreVerifyResult](resp, roles.PreVerifyResult{Verdict: "GO"})
	return r, nil
}

// ─── Scientist verdict update ─────────────────────────────────────────────────

func (c *Chain) UpdateScientistVerdict(frame roles.ScientistFrame, evidenceTable string) (roles.ScientistFrame, error) {
	prompt := roles.ScientistValidatorPrompt + "\nPOST-EXECUTION: update verdict based on actual evidence."
	userMsg := fmt.Sprintf("Hypothesis: %s\nEvidence:\n%s\n\n"+
		`Respond ONLY with JSON: {"hypothesis":"","baseline":"","experiment":"","proven_criteria":"","verdict":"PROVEN","verdict_reason":""}`,
		frame.Hypothesis, evidenceTable[:minInt(2000, len(evidenceTable))])
	resp, err := c.llm.Call(roles.RoleScientistValidator, prompt, userMsg)
	if err != nil {
		return frame, nil
	}
	r, _ := extractJSON[roles.ScientistFrame](resp, frame)
	if r.Verdict == "" {
		r.Verdict = frame.Verdict
	}
	return r, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func extractJSON[T any](text string, defaultVal T) (T, error) {
	start := strings.Index(text, "{")
	if start == -1 {
		return defaultVal, nil
	}
	depth, end := 0, -1
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if depth == 0 && end >= 0 {
			break
		}
	}
	if end == -1 {
		return defaultVal, nil
	}
	var result T
	if err := json.Unmarshal([]byte(text[start:end+1]), &result); err != nil {
		return defaultVal, nil
	}
	return result, nil
}

func (c *Chain) log(format string, args ...any) {
	c.progress("role", fmt.Sprintf(format, args...))
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
