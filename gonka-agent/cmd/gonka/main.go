// gonka — unified CLI entry point for the gonka coding agent.
//
// Usage:
//
//	gonka "your task description"         # run task in CLI mode
//	gonka                                 # interactive mode: reads task from stdin
//	gonka init                            # first-run setup: pick role + UI mode
//	gonka doctor                          # run self-diagnostics
//	gonka slots                           # show binary slot stats
//	gonka bench                           # run benchmark suite
//	gonka update                          # check for + apply updates
//	gonka deps                            # pull required dependencies
//	gonka --tui "task"                    # force TUI mode
//	gonka --n8n                           # launch n8n visual UI
//	gonka --voice "task"                  # voice input mode
//	gonka --clear-cache                   # invalidate context cache
//	gonka --new                           # start a new session
//	gonka --hard "task"                   # force hard/phased mode
//
// Environment variables (see .env.example):
//
//	GONKA_API_URL       — PRIMARY: Gonka DAPI via opengnk proxy
//	GONKA_API_KEY       — Gonka API keys (comma-separated for rotation)
//	GONKA_MODEL         — model for Gonka (default: Qwen3-235B)
//	OPENROUTER_API_KEY  — OVERFLOW: distributes tool-call / non-critical streams
//	OPENROUTER_MODEL    — model for OpenRouter
//	OLLAMA_URL          — LOCAL FALLBACK: offline-capable
//	OLLAMA_MODEL        — model for Ollama (default: qwen2.5-coder:7b)
//	AGENT_WORKSPACE     — project directory (default: current directory)
//	AGENT_MODEL         — model for execute phase
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/healthmon"
	"github.com/gonkalabs/gonka-agent/internal/n8n"
	"github.com/gonkalabs/gonka-agent/internal/profile"
	"github.com/gonkalabs/gonka-agent/internal/rolechain"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/semcache"
	"github.com/gonkalabs/gonka-agent/internal/setup"
	"github.com/gonkalabs/gonka-agent/internal/skills"
	"github.com/gonkalabs/gonka-agent/internal/slotstore"
	"github.com/gonkalabs/gonka-agent/internal/tools"
	appTUI "github.com/gonkalabs/gonka-agent/internal/tui"
	"github.com/gonkalabs/gonka-agent/internal/version"
)

// ─── ANSI colour helpers ─────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiGreen  = "\033[38;2;0;230;118m"
	ansiYellow = "\033[38;2;255;214;0m"
	ansiCyan   = "\033[38;2;0;229;255m"
	ansiBlue   = "\033[38;2;80;180;255m"
	ansiRed    = "\033[38;2;255;82;82m"

	// Gonka palette — bright blue / turquoise / metallic ONLY
	colorTurquoise  = "\033[38;2;0;212;170m"
	colorBrightBlue = "\033[38;2;80;180;255m"
	colorMetallic   = "\033[38;2;160;200;220m"

	// ansiGray replaced with metallic — no black/dark anywhere
	ansiGray = "\033[38;2;160;200;220m"
	colorBlue = "\033[38;2;0;136;255m"
)

func col(c, s string) string { return c + s + ansiReset }

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "doctor":
			runDoctor()
			return
		case "init":
			runInit()
			return
		case "slots":
			runSlots()
			return
		case "bench":
			runBench()
			return
		case "update":
			runUpdate()
			return
		case "deps":
			runDeps()
			return
		}
	}

	// Parse flags
	hardMode := false
	clearCache := false
	newSession := false
	tuiMode := false
	n8nMode := false
	voiceMode := false
	var taskArgs []string

	for _, arg := range os.Args[1:] {
		switch {
		case arg == "--version" || arg == "-v":
			fmt.Println("gonka " + version.String())
			return
		case arg == "--clear-cache":
			clearCache = true
		case arg == "--hard":
			hardMode = true
		case arg == "--new":
			newSession = true
		case arg == "--tui":
			tuiMode = true
		case arg == "--n8n":
			n8nMode = true
		case arg == "--ui":
			// --ui: interactive choice between TUI and n8n
			tuiMode = true // default to TUI; user can switch to n8n with Ctrl+N inside
		case arg == "--voice":
			voiceMode = true
		case strings.HasPrefix(arg, "-"):
			// ignore unknown flags
		default:
			taskArgs = append(taskArgs, arg)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, col(ansiRed, "config error: ")+"%v\n", err)
		fmt.Fprintf(os.Stderr, "  Run: cp .env.example .env  and fill in GONKA_API_KEY or OPENROUTER_API_KEY\n")
		os.Exit(1)
	}

	// Initialize global health monitor
	cacheDir := filepath.Join(cfg.Workspace, ".gonka-cache")
	monitor := healthmon.New(cacheDir)
	monitor.Start()
	defer monitor.Stop()

	// Load skill packs
	if err := skills.Load(); err != nil {
		monitor.Reportf("skills", healthmon.SevWarn, "failed to load skill packs: %v", err)
	}

	// Initialize inference router from env
	router, err := setup.InferenceRouter()
	if err != nil {
		monitor.Reportf("inference", healthmon.SevError, "router init: %v", err)
	}

	// Probe all providers in background
	if router != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		probeResults := router.ProbeAll(ctx)
		cancel()
		for name, err := range probeResults {
			if err != nil {
				monitor.Reportf("inference", healthmon.SevWarn, "provider %s unreachable: %v", name, err)
			}
		}
	}

	// n8n mode: launch visual UI and exit
	if n8nMode {
		runN8N(cacheDir, monitor)
		return
	}

	// TUI mode
	if tuiMode {
		_ = voiceMode // will be used in C.10
		runTUI(taskArgs, cfg, monitor)
		return
	}

	// ─── Classic CLI mode ─────────────────────────────────────────────────

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
		fmt.Println(col(ansiGreen, "✓") + " context cache cleared for: " + cfg.Workspace)
		return
	}
	if newSession {
		agent.ClearSession(cfg.Workspace)
		fmt.Println(col(ansiGreen, "✓") + " session cleared for: " + cfg.Workspace)
		return
	}

	var task string
	if len(taskArgs) > 0 {
		task = strings.Join(taskArgs, " ")
	} else {
		printBanner()
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
	if cfg.FallbackKey != "" {
		client.SetFallback(cfg.FallbackURL, cfg.FallbackKey, cfg.FallbackModel)
	}

	if fb := loadPendingFeedback(cfg.Workspace); fb != "" {
		client.SetFeedback(fb)
		clearPendingFeedback(cfg.Workspace)
	}

	sc, scErr := semcache.New(semcache.Config{
		CacheDir:   filepath.Join(cfg.Workspace, ".gonka-cache"),
		APIBaseURL: embedURL,
		APIKey:     cfg.GonkaAPIKey,
		EmbedModel: cfg.EmbedModel,
	})

	slots, slotErr := slotstore.Open(slotstore.Config{
		SlotDir:      cfg.BSSlotDir,
		EmbedURL:     cfg.BSEmbedURL,
		EmbedModel:   cfg.EmbedModel,
		QualityURL:   cfg.BSQualityURL,
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

	// Header
	fmt.Printf("\n%s %s\n", col(ansiBold, "gonka"), col(colorBrightBlue, "coding agent"))
	fmt.Printf("  task:      %s\n", col(ansiBold, truncateStr(task, 60)))
	if bi.Language != "" {
		fmt.Printf("  language:  %s  build: %s\n", col(ansiCyan, bi.Language), col(ansiGray, bi.BuildCmd))
	}
	fmt.Printf("  pool:      %s keys\n", col(ansiGreen, fmt.Sprintf("%d", pool.Size())))
	if router != nil {
		status := router.ProviderStatus()
		var provNames []string
		for _, s := range status {
			icon := col(ansiGreen, "●")
			if s.BreakerState != "closed" {
				icon = col(ansiRed, "○")
			}
			provNames = append(provNames, icon+" "+s.Name)
		}
		fmt.Printf("  inference: %s\n", strings.Join(provNames, "  "))
	}
	if slots != nil && slots.Count() > 0 {
		fmt.Printf("  slots:     %s binary patterns loaded\n", col(ansiCyan, fmt.Sprintf("%d", slots.Count())))
	}
	fmt.Printf("  workspace: %s\n\n", col(ansiGray, cfg.Workspace))

	progressBus := appTUI.NewProgressBus(true)
	progress := makeBusProgressFn(progressBus)

	var sys string
	sys = agent.BuildSystemPrompt(t, bi)

	// Inject seed patterns (baked into binary) + skill pack prompt rules
	seedAug := profile.SeedPromptAugmentation(profile.RoleDeveloper)
	if seedAug != "" {
		sys += "\n" + seedAug
	}
	skillRules := skills.PromptAugmentation("developer")
	if skillRules != "" {
		sys += "\n\n## Active Skill Rules\n" + skillRules
	}

	if slots != nil {
		var allMatches []slotstore.SearchResult
		if slots.Count() > 0 {
			allMatches = append(allMatches, slots.Search(task)...)
		}
		if meshResults := slots.SearchMesh(task); len(meshResults) > 0 {
			fmt.Printf("  %s %d mesh pool matches\n", col(ansiGray, "↗"), len(meshResults))
			allMatches = append(allMatches, meshResults...)
		}
		if len(allMatches) > 0 {
			fmt.Printf("  %s %d total slot matches (best=%.2f)\n",
				col(ansiCyan, "◇"), len(allMatches), allMatches[0].Similarity)
			progress("cache", fmt.Sprintf("slots: %d matches, best=%.2f", len(allMatches), allMatches[0].Similarity))
			sys += slotstore.FormatContext(allMatches)
		}
	}

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
		history := agent.LoadSession(cfg.Workspace)
		result = agent.RunWithHistory(client, t, sys, task, history, 30, progress)
		agent.SaveSession(cfg.Workspace, result.SessionMessages)
	}

	fmt.Print("\n")
	if result.Err != nil {
		savePendingFeedback(cfg.Workspace, "unresolved")
		if scErr == nil && sc != nil {
			sc.UpdateQuality("unresolved")
		}
		fmt.Fprintln(os.Stderr, col(ansiRed, "FAILED: ")+result.Err.Error())
		printSummary(task, result, bi)
		os.Exit(1)
	}

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

	if slots != nil && result.FinalAnswer != "" {
		if newSlot, err := slots.Distill(task, result.FinalAnswer, 0.8); err == nil && len(newSlot.Vec) > 0 {
			fmt.Printf("  %s new slot distilled (total: %d)\n", col(ansiCyan, "◇"), slots.Count())
			if err := slots.ShareToMesh(newSlot); err == nil {
				fmt.Printf("  %s shared to mesh pool\n", col(ansiGray, "↗"))
			}
		}
	}

	savePendingFeedback(cfg.Workspace, "resolved")

	if result.FinalAnswer != "" {
		fmt.Println(result.FinalAnswer)
	}
	printSummary(task, result, bi)
}

// ─── Subcommands ──────────────────────────────────────────────────────────────

func runDoctor() {
	printBanner()
	fmt.Println(col(ansiBold, "  Self-Diagnostics\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	checks := healthmon.Doctor(ctx)
	for _, c := range checks {
		icon := col(ansiGreen, "✓")
		switch c.Status {
		case "warn":
			icon = col(ansiYellow, "!")
		case "fail":
			icon = col(ansiRed, "✗")
		}
		fmt.Printf("  %s %-14s %s\n", icon, c.Name, col(ansiGray, c.Detail))
	}
	fmt.Println()
}

func runInit() {
	printBanner()
	fmt.Println(col(ansiBold, "  First-Run Setup\n"))
	fmt.Println("  Select your role:")
	fmt.Println("    " + col(colorTurquoise, "[1]") + " Developer  — coding, debugging, deployment")
	fmt.Println("    " + col(colorBlue, "[2]") + " Researcher — analysis, data, experiments")
	fmt.Println("    " + col(ansiYellow, "[3]") + " Bot        — automated tasks, CI/CD integration")
	fmt.Println()
	fmt.Print("  Choice [1-3]: ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	role := strings.TrimSpace(sc.Text())
	switch role {
	case "1", "developer":
		role = "developer"
	case "2", "researcher":
		role = "researcher"
	case "3", "bot":
		role = "bot"
	default:
		role = "developer"
	}

	fmt.Println()
	fmt.Println("  Select UI mode:")
	fmt.Println("    " + col(colorTurquoise, "[1]") + " TUI    — rich terminal interface (recommended)")
	fmt.Println("    " + col(colorBlue, "[2]") + " n8n    — visual workflow UI in browser")
	fmt.Println("    " + col(ansiGray, "[3]") + " CLI    — minimal, pipeline-friendly output")
	fmt.Println()
	fmt.Print("  Choice [1-3]: ")
	sc.Scan()
	uiMode := strings.TrimSpace(sc.Text())

	cwd, _ := os.Getwd()
	initCfg := map[string]string{
		"role": role,
		"ui":   uiMode,
	}
	data, _ := json.MarshalIndent(initCfg, "", "  ")
	cfgDir := filepath.Join(cwd, ".gonka-cache")
	os.MkdirAll(cfgDir, 0755)
	os.WriteFile(filepath.Join(cfgDir, "profile.json"), data, 0644)

	fmt.Printf("\n  %s Saved profile: role=%s, ui=%s\n", col(ansiGreen, "✓"), col(ansiBold, role), uiMode)
	fmt.Println("  Run " + col(ansiBold, "gonka") + " to start working!\n")
}

func runSlots() {
	printBanner()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, col(ansiRed, "config: ")+"%v\n", err)
		os.Exit(1)
	}
	slots, err := slotstore.Open(slotstore.Config{
		SlotDir:    cfg.BSSlotDir,
		EmbedURL:   cfg.BSEmbedURL,
		EmbedModel: cfg.EmbedModel,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, col(ansiRed, "slots: ")+"%v\n", err)
		os.Exit(1)
	}
	defer slots.Close()

	fmt.Printf("  %s Binary Slot Store\n", col(ansiBold, "◇"))
	fmt.Printf("  slots:   %s\n", col(ansiCyan, fmt.Sprintf("%d", slots.Count())))
	fmt.Printf("  dir:     %s\n\n", col(ansiGray, cfg.BSSlotDir))
}

func runBench() {
	fmt.Println(col(colorTurquoise, "  gonka bench") + " — not yet implemented (Phase C.12)")
	fmt.Println("  Will run N iterations of a reference task, measure tokens/time/latency.")
}

func runUpdate() {
	fmt.Println(col(colorTurquoise, "  gonka update") + " — not yet implemented (Phase D.13)")
	fmt.Println("  Will check GitHub Releases for newer binary + apply.")
}

func runDeps() {
	fmt.Println(col(colorTurquoise, "  gonka deps") + " — not yet implemented (Phase D.14)")
	fmt.Println("  Will auto-pull required repos (opengnk, gonka-main) for slot flow.")
}

func runN8N(cacheDir string, monitor *healthmon.Monitor) {
	printBanner()
	fmt.Printf("  %s Starting n8n...\n\n", col(colorTurquoise, "◈"))

	mgr := n8n.NewManager(cacheDir)
	ctx := context.Background()
	if err := mgr.Start(ctx); err != nil {
		monitor.Reportf("n8n", healthmon.SevError, "start failed: %v", err)
		fmt.Fprintf(os.Stderr, col(ansiRed, "n8n: ")+"%v\n", err)
		fmt.Fprintln(os.Stderr, "  Ensure Docker is running: docker info")
		os.Exit(1)
	}

	fmt.Printf("  %s %s\n", col(ansiGreen, "✓"), col(colorBrightBlue, mgr.URL()))
	fmt.Printf("  %s\n\n", col(colorMetallic, "Ctrl+C to stop"))

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
}

func runTUI(taskArgs []string, cfg *config.Config, monitor *healthmon.Monitor) {
	model := appTUI.New()
	p := tea.NewProgram(model, tea.WithAltScreen())

	if len(taskArgs) > 0 {
		task := strings.Join(taskArgs, " ")
		go func() {
			time.Sleep(500 * time.Millisecond)
			p.Send(appTUI.ChatMsg{Role: "user", Content: task})
		}()
	}

	_, err := p.Run()
	if err != nil {
		monitor.Reportf("tui", healthmon.SevError, "TUI error: %v", err)
		fmt.Fprintf(os.Stderr, col(ansiRed, "TUI error: ")+"%v\n", err)
		os.Exit(1)
	}
}

// ─── Banner ───────────────────────────────────────────────────────────────────

func printBanner() {
	fmt.Println()
	fmt.Println(col(colorTurquoise, "  ╔═══════════════════════════════════╗"))
	fmt.Println(col(colorTurquoise, "  ║") + col(ansiBold, "     GONKA GO ") + col(colorBrightBlue, "coding agent      ") + col(colorTurquoise, "║"))
	fmt.Println(col(colorTurquoise, "  ╚═══════════════════════════════════╝"))
	fmt.Println()
}

// ─── Progress with bus integration ────────────────────────────────────────────

func makeBusProgressFn(bus *appTUI.ProgressBus) agent.ProgressFn {
	return func(event, detail string) {
		switch event {
		case "phase":
			bus.Emit(detail, "", "", 0)
			fmt.Printf("\n%s %s\n", col(colorTurquoise+ansiBold, "▶"), col(ansiBold, detail))
		case "role":
			fmt.Printf("  %s %s\n", col(colorBlue, "·"), col(ansiDim, detail))
		case "tool":
			bus.Emit("", detail, detail, 0)
			fmt.Printf("  %s %s\n", col(ansiYellow, "⚙"), detail)
		case "result":
			if strings.HasPrefix(detail, "[err]") {
				fmt.Printf("  %s %s\n", col(ansiRed, "✗"), col(ansiDim, detail))
			} else if strings.Contains(detail, "web_fetch failed after") {
				fmt.Printf("  %s %s\n", col(ansiRed, "⚠"), col(ansiYellow, truncate(detail, 140)))
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
			fmt.Printf("  %s %s\n", col(ansiGray, "…"), col(ansiGray, truncate(detail, 100)))
		}
	}
}

// ─── Complexity classifier ────────────────────────────────────────────────────

func classifyComplexity(task string, bi tools.BuildInfo) agent.Complexity {
	lower := strings.ToLower(task)
	words := strings.Fields(task)

	if len(words) <= 5 {
		return agent.ComplexitySimple
	}

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

	fileCount := 0
	for _, ext := range append(bi.SourceExts, ".go", ".ts", ".py", ".rs") {
		if strings.Count(task, ext) > 0 {
			fileCount++
		}
	}
	if fileCount >= 2 {
		return agent.ComplexityHard
	}

	mediumKeywords := []string{
		"add", "fix", "update", "change", "remove", "implement", "create", "write",
		"добавь", "исправь", "обнови", "удали", "реализуй", "напиши",
	}
	for _, kw := range mediumKeywords {
		if strings.Contains(lower, kw) {
			return agent.ComplexityMedium
		}
	}

	return agent.ComplexityMedium
}

// ─── Header & summary ─────────────────────────────────────────────────────────

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
	fmt.Println(col(colorTurquoise, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"))
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
	fmt.Println(col(colorTurquoise, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func truncateStr(s string, n int) string { return truncate(s, n) }

// ─── Feedback persistence ─────────────────────────────────────────────────────

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
