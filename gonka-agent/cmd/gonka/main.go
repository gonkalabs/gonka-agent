// gonka — unified CLI entry point for the gonka coding agent.
//
// Usage:
//
//	gonka "your task description"
//	gonka                          # interactive mode: reads task from stdin
//	gonka --clear-cache            # invalidate context cache and exit
//	gonka --mode=simple "task"     # force a specific mode
//	gonka --mode=medium "task"
//	gonka --mode=hard "task"
//
// Environment variables (see .env.example):
//
//	GONKA_API_KEY    — primary API key (required)
//	GONKA_API_KEYS   — comma-separated list of additional keys (for parallel planning)
//	GONKA_SOURCE_URL — inference endpoint URL
//	AGENT_WORKSPACE  — project directory (default: current directory)
//	AGENT_MODEL      — model for execute phase (default: Qwen/Qwen3-235B-A22B-Instruct-2507-FP8)
//	AGENT_PLAN_MODEL — model for planning roles (default: same as AGENT_MODEL)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/rolechain"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/semcache"
	"github.com/gonkalabs/gonka-agent/internal/slotstore"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

// ─── ANSI colour helpers ─────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiBlue   = "\033[34m"
	ansiRed    = "\033[31m"
	ansiGray   = "\033[90m"
)

func col(c, s string) string { return c + s + ansiReset }

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	hardMode := false
	clearCache := false
	newSession := false
	var taskArgs []string

	for _, arg := range os.Args[1:] {
		switch {
		case arg == "--clear-cache":
			clearCache = true
		case arg == "--hard":
			hardMode = true
		case arg == "--new":
			newSession = true
		case strings.HasPrefix(arg, "-"):
			// ignore unknown flags
		default:
			taskArgs = append(taskArgs, arg)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, col(ansiRed, "config error: ")+"%v\n", err)
		fmt.Fprintf(os.Stderr, "  Run: cp .env.example .env  and fill in GONKA_API_KEY\n")
		os.Exit(1)
	}

	// EmbedURL: prefer AGENT_EMBED_URL (local fastembed sidecar) over inference URL.
	// Gonka inference nodes only serve /v1/chat/completions, not /v1/embeddings.
	// Start: python3 scripts/embed-server.py  →  set AGENT_EMBED_URL=http://localhost:8001/v1
	embedURL := cfg.EmbedURL
	if embedURL == "" {
		embedURL = cfg.GonkaDirectURL
	}

	t := &tools.Cfg{
		Workspace: cfg.Workspace, Shell: cfg.Shell,
		CmdTimeout: cfg.CommandTimeout, MaxFileSize: cfg.MaxFileSize,
		AllowShell: cfg.AllowShell, RgPath: cfg.RgPath,
		WebSearchKey: cfg.WebSearchKey, SearXNGURL: cfg.SearXNGURL,
		WebFetchTimeout: cfg.WebFetchTimeout, WebFetchMaxSize: cfg.WebFetchMaxSize,
		GonkaBaseURL: embedURL, GonkaAPIKey: cfg.GonkaAPIKey,
		EmbedModel: cfg.EmbedModel,
	}

	if clearCache {
		rolechain.InvalidateCache(cfg.Workspace)
		fmt.Println(col(ansiGreen, "✓")+" context cache cleared for: "+cfg.Workspace)
		return
	}
	if newSession {
		agent.ClearSession(cfg.Workspace)
		fmt.Println(col(ansiGreen, "✓")+" session cleared for: "+cfg.Workspace)
		return
	}

	// Read task.
	var task string
	if len(taskArgs) > 0 {
		task = strings.Join(taskArgs, " ")
	} else {
		fmt.Print(col(ansiBold, "Task: "))
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			task = strings.TrimSpace(sc.Text())
		}
	}
	if task == "" {
		fmt.Fprintln(os.Stderr, "no task provided")
		os.Exit(1)
	}

	bi := t.DetectBuildSystem()
	model := cfg.AgentModel
	pool := roles.NewLLMPoolDualModel(cfg.GonkaDirectURL, model, model, cfg.GonkaAPIKeys)
	client := agent.NewClient(cfg.GonkaDirectURL, cfg.GonkaAPIKey, model)
	client.SetKeys(cfg.GonkaAPIKeys)

	// Load pending feedback from previous run and schedule it on first request.
	if fb := loadPendingFeedback(cfg.Workspace); fb != "" {
		client.SetFeedback(fb)
		clearPendingFeedback(cfg.Workspace)
	}

	// Semantic cache: lookup before running, store after success.
	sc, scErr := semcache.New(semcache.Config{
		CacheDir:   filepath.Join(cfg.Workspace, ".gonka-cache"),
		APIBaseURL: embedURL,
		APIKey:     cfg.GonkaAPIKey,
		EmbedModel: cfg.EmbedModel,
	})

	// Binary singularity slot store: load slots, ingest raw input.
	slots, slotErr := slotstore.Open(slotstore.Config{
		SlotDir:      cfg.BSSlotDir,
		EmbedURL:     cfg.BSEmbedURL,
		ChunkLines:   cfg.BSChunkLines,
		MinSimBps:    cfg.BSMinSimBps,
		RawInputPath: cfg.BSRawInput,
	})
	if slotErr != nil {
		fmt.Fprintf(os.Stderr, col(ansiYellow, "slots: ")+"%v\n", slotErr)
	}
	defer func() {
		if slots != nil {
			_ = slots.Close()
		}
	}()

	// Header — no complexity label, just project info.
	fmt.Printf("\n%s %s\n", col(ansiBold, "gonka"), col(ansiGray, "coding agent"))
	fmt.Printf("  task:      %s\n", col(ansiBold, truncateStr(task, 60)))
	if bi.Language != "" {
		fmt.Printf("  language:  %s  build: %s\n", col(ansiCyan, bi.Language), col(ansiGray, bi.BuildCmd))
	}
	fmt.Printf("  pool:      %s keys\n", col(ansiGreen, fmt.Sprintf("%d", pool.Size())))
	if slots != nil && slots.Count() > 0 {
		fmt.Printf("  slots:     %s binary patterns loaded\n", col(ansiCyan, fmt.Sprintf("%d", slots.Count())))
	}
	fmt.Printf("  workspace: %s\n\n", col(ansiGray, cfg.Workspace))

	progress := makeProgressFn()

	// Context injection: semcache + slot store.
	var sys string
	sys = agent.BuildSystemPrompt(t, bi)

	// Slot store: search for matching patterns before running.
	if slots != nil && slots.Count() > 0 {
		if matches := slots.Search(task); len(matches) > 0 {
			fmt.Printf("  %s %d slot matches (best=%.2f)\n",
				col(ansiCyan, "◇"), len(matches), matches[0].Similarity)
			progress("cache", fmt.Sprintf("slot store: %d matches, best=%.2f", len(matches), matches[0].Similarity))
			sys += slotstore.FormatContext(matches)
		}
	}

	// Semcache lookup: inject context before running.
	if scErr == nil && sc != nil {
		if hit := sc.Lookup(task); hit.Kind != semcache.Miss {
			label := "partial"
			if hit.Kind == semcache.Full {
				label = "full"
			}
			fmt.Printf("  %s %s hit (score %.2f)\n", col(ansiGray, "◈"), label, hit.Score)
			progress("cache", fmt.Sprintf("semcache %s hit score=%.2f", label, hit.Score))
			sys += "\n\n" + hit.Context
		}
	}
	fmt.Println()

	var result agent.RunResult
	if hardMode {
		result = agent.RunPhased(pool, client, t, task, progress)
	} else {
		// Single loop — model drives everything.
		history := agent.LoadSession(cfg.Workspace)
		result = agent.RunWithHistory(client, t, sys, task, history, 30, progress)
		agent.SaveSession(cfg.Workspace, result.SessionMessages)
	}

	fmt.Print("\n")
	if result.Err != nil {
		// Schedule unresolved feedback for next run.
		savePendingFeedback(cfg.Workspace, "unresolved")
		if scErr == nil && sc != nil {
			sc.UpdateQuality("unresolved")
		}
		fmt.Fprintln(os.Stderr, col(ansiRed, "FAILED: ")+result.Err.Error())
		printSummary(task, result, bi)
		os.Exit(1)
	}

	// Store successful result in semcache and schedule resolved feedback.
	if scErr == nil && sc != nil {
		scSteps := make([]semcache.Step, len(result.Steps))
		for i, s := range result.Steps {
			scSteps[i] = semcache.Step{
				ToolName: s.ToolName, ArgsJSON: s.ArgsJSON,
				Result: s.Result, IsError: s.IsError, ElapsedMs: s.ElapsedMs,
			}
		}
		sc.Store(task, scSteps, result.FinalAnswer)
		sc.UpdateQuality("resolved")
	}

	// Distill successful result into a binary slot.
	if slots != nil && result.FinalAnswer != "" {
		if err := slots.Distill(task, result.FinalAnswer, 0.8); err == nil {
			fmt.Printf("  %s new slot distilled (total: %d)\n", col(ansiCyan, "◇"), slots.Count())
		}
	}

	savePendingFeedback(cfg.Workspace, "resolved")

	if result.FinalAnswer != "" {
		fmt.Println(result.FinalAnswer)
	}
	printSummary(task, result, bi)
}

// ─── Complexity classifier ────────────────────────────────────────────────────

func classifyComplexity(task string, bi tools.BuildInfo) agent.Complexity {
	lower := strings.ToLower(task)
	words := strings.Fields(task)

	// Very short or pure question → simple.
	if len(words) <= 5 {
		return agent.ComplexitySimple
	}

	// Hard keywords: architecture/migration/multi-file refactor.
	hardKeywords := []string{
		"refactor", "migrate", "redesign", "architecture", "decouple", "extract package",
		"extract module", "rewrite", "integrate", "rename all", "move all",
		"рефактор", "мигр", "перепис", "переработ",
	}
	for _, kw := range hardKeywords {
		if strings.Contains(lower, kw) {
			return agent.ComplexityHard
		}
	}

	// Multiple file references → hard.
	fileCount := 0
	for _, ext := range append(bi.SourceExts, ".go", ".ts", ".py", ".rs") {
		if strings.Count(task, ext) > 0 {
			fileCount++
		}
	}
	if fileCount >= 2 {
		return agent.ComplexityHard
	}

	// Medium keywords: specific changes in one file.
	mediumKeywords := []string{
		"add", "fix", "update", "change", "remove", "implement", "create", "write",
		"добавь", "исправь", "обнови", "удали", "реализуй", "напиши",
	}
	for _, kw := range mediumKeywords {
		if strings.Contains(lower, kw) {
			return agent.ComplexityMedium
		}
	}

	// Default: medium.
	return agent.ComplexityMedium
}

// ─── Progress display ─────────────────────────────────────────────────────────

func makeProgressFn() agent.ProgressFn {
	lastEvent := ""
	return func(event, detail string) {
		switch event {
		case "phase":
			fmt.Printf("\n%s %s\n", col(ansiCyan+ansiBold, "▶"), col(ansiBold, detail))
		case "role":
			fmt.Printf("  %s %s\n", col(ansiBlue, "·"), col(ansiDim, detail))
		case "tool":
			fmt.Printf("  %s %s\n", col(ansiYellow, "⚙"), detail)
		case "result":
			if strings.HasPrefix(detail, "[err]") {
				fmt.Printf("  %s %s\n", col(ansiRed, "✗"), col(ansiDim, detail))
			} else {
				fmt.Printf("  %s %s\n", col(ansiGreen, "✓"), col(ansiGray, truncate(detail, 120)))
			}
		case "build":
			if strings.Contains(detail, "FAILED") {
				fmt.Printf("  %s build: %s\n", col(ansiRed, "✗"), detail)
			} else {
				fmt.Printf("  %s build: %s\n", col(ansiGreen, "✓"), detail)
			}
		case "test":
			if strings.Contains(detail, "FAILED") {
				fmt.Printf("  %s tests: %s\n", col(ansiRed, "✗"), detail)
			} else {
				fmt.Printf("  %s tests: %s\n", col(ansiGreen, "✓"), detail)
			}
		case "cache":
			fmt.Printf("  %s %s\n", col(ansiGray, "◈"), col(ansiGray, detail))
		case "think":
			if lastEvent != "think" {
				fmt.Printf("  %s %s\n", col(ansiGray, "…"), col(ansiGray, truncate(detail, 100)))
			}
		}
		lastEvent = event
	}
}

// ─── Header & summary ─────────────────────────────────────────────────────────

func printHeader(task string, complexity agent.Complexity, bi tools.BuildInfo, poolSize int, workspace string) {
	modeColor := ansiGreen
	switch complexity {
	case agent.ComplexityMedium:
		modeColor = ansiYellow
	case agent.ComplexityHard:
		modeColor = ansiRed
	}

	lang := bi.Language
	if lang == "" || lang == "unknown" {
		lang = "?"
	}
	build := bi.BuildCmd
	if build == "" {
		build = "?"
	}

	fmt.Println()
	fmt.Printf("%s %s\n", col(ansiBold, "gonka"), col(ansiGray, "coding agent"))
	fmt.Printf("  task:      %s\n", col(ansiBold, truncate(task, 70)))
	fmt.Printf("  mode:      %s\n", col(modeColor+ansiBold, string(complexity)))
	fmt.Printf("  language:  %s  build: %s\n", col(ansiCyan, lang), col(ansiGray, build))
	if poolSize > 1 {
		fmt.Printf("  pool:      %s keys (parallel planning enabled)\n", col(ansiGreen, fmt.Sprintf("%d", poolSize)))
	}
	fmt.Printf("  workspace: %s\n", col(ansiGray, workspace))
	fmt.Println()
}

func printSummary(task string, result agent.RunResult, bi tools.BuildInfo) {
	elapsed := time.Duration(result.ElapsedMs) * time.Millisecond

	toolCounts := map[string]int{}
	for _, s := range result.Steps {
		toolCounts[s.ToolName]++
	}
	var toolParts []string
	for name, count := range toolCounts {
		toolParts = append(toolParts, fmt.Sprintf("%s(%d)", name, count))
	}

	verdict := ""
	if result.ScientistFrame.Verdict != "" {
		v := string(result.ScientistFrame.Verdict)
		vc := ansiGreen
		if v == "INSUFFICIENT" || v == "INVALID" {
			vc = ansiRed
		}
		verdict = col(vc, v)
	}

	fmt.Println()
	fmt.Println(col(ansiGray, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"))
	fmt.Printf("  %-10s %s\n", col(ansiGray, "task:"), truncate(task, 60))
	fmt.Printf("  %-10s %s\n", col(ansiGray, "mode:"), string(result.Mode))
	fmt.Printf("  %-10s %s  |  lang: %s  build: %s\n",
		col(ansiGray, "project:"),
		col(ansiCyan, bi.Language),
		col(ansiGray, bi.Language),
		col(ansiGray, bi.BuildCmd))
	fmt.Printf("  %-10s %s\n", col(ansiGray, "elapsed:"), col(ansiBold, elapsed.Round(time.Second).String()))
	fmt.Printf("  %-10s %s  (%d tool calls)\n",
		col(ansiGray, "tools:"),
		col(ansiGray, strings.Join(toolParts, "  ")),
		len(result.Steps))
	if result.TotalTokens > 0 {
		fmt.Printf("  %-10s %s prompt  +  %s completion  =  %s total  (%s calls)\n",
			col(ansiGray, "tokens:"),
			col(ansiGray, fmt.Sprintf("%d", result.PromptTokens)),
			col(ansiGray, fmt.Sprintf("%d", result.CompletionTokens)),
			col(ansiBold, fmt.Sprintf("%d", result.TotalTokens)),
			col(ansiGray, fmt.Sprintf("%d", result.LLMCalls)))
	}
	if verdict != "" {
		fmt.Printf("  %-10s %s\n", col(ansiGray, "verdict:"), verdict)
	}
	fmt.Println(col(ansiGray, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func truncateStr(s string, n int) string { return truncate(s, n) }

// ─── Feedback persistence ─────────────────────────────────────────────────────
// Feedback from the current run is stored to disk and loaded on the next run,
// then sent as X-Inference-Feedback on the first inference request.
// This ensures opengnk quality middleware receives L4 signal even across
// process restarts (each gonka invocation is a separate process).

type feedbackFile struct {
	Outcome string `json:"outcome"`
}

func feedbackPath(workspace string) string {
	return filepath.Join(workspace, ".gonka-cache", "feedback.json")
}

func savePendingFeedback(workspace, outcome string) {
	_ = os.MkdirAll(filepath.Join(workspace, ".gonka-cache"), 0755)
	data, _ := json.Marshal(feedbackFile{Outcome: outcome})
	_ = os.WriteFile(feedbackPath(workspace), data, 0644)
}

func loadPendingFeedback(workspace string) string {
	data, err := os.ReadFile(feedbackPath(workspace))
	if err != nil {
		return ""
	}
	var f feedbackFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.Outcome
}

func clearPendingFeedback(workspace string) {
	_ = os.Remove(feedbackPath(workspace))
}
