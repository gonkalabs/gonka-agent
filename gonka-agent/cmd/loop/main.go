package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

const systemPrompt = `You are a precise coding agent. Use the available tools to complete tasks step by step.
- Always read files before modifying them
- Use detect_build_system first to learn the project language and build/test commands
- Use search_replace for targeted edits (better than rewriting whole files)
- After modifying code, verify with the project's build command (use detect_build_system to find it)
- If build fails: read the error, fix with search_replace, rebuild
- Be methodical: understand → plan → execute → verify
- Never assume the project is Go — it may be TypeScript, Python, Rust, etc.`

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	t := toolsCfg(cfg)
	client := agent.NewClient(
		cfg.GonkaDirectURL,
		cfg.GonkaAPIKey,
		getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"),
	)

	var task string
	if len(os.Args) > 1 {
		task = strings.Join(os.Args[1:], " ")
	} else {
		fmt.Print("Task: ")
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() { task = sc.Text() }
	}
	if task == "" {
		fmt.Fprintln(os.Stderr, "no task provided")
		os.Exit(1)
	}

	fmt.Printf("[USER]: %s\n", task)
	progress := func(event, detail string) {
		fmt.Printf("[%s] %s\n", event, detail)
	}
	result := agent.Run(client, t, systemPrompt, task, 30, progress)
	if result.Err != nil {
		fmt.Fprintln(os.Stderr, "error:", result.Err)
		os.Exit(1)
	}
}

func toolsCfg(cfg *config.Config) *tools.Cfg {
	return &tools.Cfg{
		Workspace: cfg.Workspace, Shell: cfg.Shell,
		CmdTimeout: cfg.CommandTimeout, MaxFileSize: cfg.MaxFileSize,
		AllowShell: cfg.AllowShell, RgPath: cfg.RgPath,
		WebSearchKey: cfg.WebSearchKey, WebFetchTimeout: cfg.WebFetchTimeout,
		WebFetchMaxSize: cfg.WebFetchMaxSize,
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}
