// gonka-phased: production entry point for EXTRA HARD tasks.
// Uses the full 6-phase Plan-First Chain with 10 roles.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	t := &tools.Cfg{
		Workspace: cfg.Workspace, Shell: cfg.Shell,
		CmdTimeout: cfg.CommandTimeout, MaxFileSize: cfg.MaxFileSize,
		AllowShell: cfg.AllowShell, RgPath: cfg.RgPath,
		WebSearchKey: cfg.WebSearchKey, WebFetchTimeout: cfg.WebFetchTimeout,
		WebFetchMaxSize: cfg.WebFetchMaxSize,
		GonkaBaseURL: cfg.GonkaDirectURL, GonkaAPIKey: cfg.GonkaAPIKey,
		EmbedModel: cfg.EmbedModel,
	}

	model := getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")
	pool := roles.NewLLMPool(cfg.GonkaDirectURL, model, cfg.GonkaAPIKeys)
	client := agent.NewClient(cfg.GonkaDirectURL, cfg.GonkaAPIKey, model)

	var task string
	if len(os.Args) > 1 {
		task = strings.Join(os.Args[1:], " ")
	} else {
		fmt.Print("Task (EXTRA HARD): ")
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() { task = sc.Text() }
	}
	if task == "" {
		fmt.Fprintln(os.Stderr, "no task")
		os.Exit(1)
	}

	// Input classifier: reject chat/incomplete before spinning up 10-role chain.
	switch classifyInput(task) {
	case "chat":
		fmt.Printf("[CHAT] %s\n", task)
		fmt.Println("Answer: This agent handles code tasks. Please describe a specific engineering task.")
		return
	case "incomplete":
		fmt.Printf("[INCOMPLETE] Task too vague: %q\n", task)
		fmt.Println("Please specify: what to change, in which file/system, and what the expected outcome is.")
		return
	}

	fmt.Printf("\n[GONKA AGENT — PHASED MODE]\n")
	fmt.Printf("Model: %s | Pool: %d key(s)\n", model, pool.Size())
	fmt.Printf("Task: %s\n\n", task)

	progress := func(event, detail string) {
		fmt.Printf("[%s] %s\n", event, detail)
	}
	result := agent.RunPhased(pool, client, t, task, progress)
	if result.Err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", result.Err)
		os.Exit(1)
	}

	fmt.Printf("\n%s\n", result.FinalAnswer)
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}

// classifyInput returns "chat", "incomplete", or "task".
// "task" means it has enough information to route into the 6-phase chain.
func classifyInput(s string) string {
	s = strings.TrimSpace(s)
	words := strings.Fields(s)

	// Very short input — likely a question or greeting.
	if len(words) <= 4 {
		return "chat"
	}

	// Contains task-bearing keywords: imperative verbs + noun (file/func/module).
	taskKeywords := []string{
		"add", "implement", "fix", "refactor", "rename", "remove", "create",
		"update", "replace", "migrate", "decouple", "extract", "integrate",
		"build", "write", "change", "move", "найди", "добавь", "исправь",
		"рефактор", "переименуй", "удали", "реализуй",
	}
	lower := strings.ToLower(s)
	for _, kw := range taskKeywords {
		if strings.Contains(lower, kw) {
			return "task"
		}
	}

	// Has a file reference or package path (any language).
	fileMarkers := []string{
		".go", ".ts", ".tsx", ".js", ".py", ".rs", ".java", ".kt", ".rb", ".php",
		".c", ".cpp", ".swift", ".dart", ".ex", "/", "::",
	}
	for _, m := range fileMarkers {
		if strings.Contains(s, m) {
			return "task"
		}
	}

	// Looks like a question without a concrete deliverable.
	if strings.HasSuffix(strings.TrimRight(s, " ?"), "?") {
		return "chat"
	}

	// Short but no imperative — treat as incomplete.
	if len(words) < 8 {
		return "incomplete"
	}

	return "task"
}
