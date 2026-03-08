// Matrix test runner — runs all scenarios against configured models and prints a results table.
//
// Env vars:
//
//	AGENT_MODELS=ModelA,ModelB   (comma-separated, tested sequentially)
//	AGENT_SCENARIOS=S1,S2        (subset filter)
//	AGENT_VERBOSE=1              (print all tool calls live)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/agent"
	"github.com/gonkalabs/gonka-agent/internal/config"
	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

// ─── Difficulty ───────────────────────────────────────────────────────────────

type Difficulty int

const (
	EASY      Difficulty = 1
	MEDIUM    Difficulty = 2
	HARD      Difficulty = 3
	VERYHARD  Difficulty = 4
	EXTREME   Difficulty = 5
	EXTRAHARD Difficulty = 6
)

func (d Difficulty) String() string {
	return [...]string{"", "EASY", "MEDIUM", "HARD", "VERYHARD", "EXTREME", "EXTRA_HARD"}[d]
}
func (d Difficulty) ANSI() string {
	return [...]string{"", "\033[32m", "\033[36m", "\033[33m", "\033[35m", "\033[31m", "\033[91m"}[d]
}

// ─── Scenario ─────────────────────────────────────────────────────────────────

type Scenario struct {
	ID         string
	Difficulty Difficulty
	Prompt     string
	Verify     func(r agent.RunResult) (passed bool, reason string)
}

// ─── Scenario definitions ─────────────────────────────────────────────────────

func buildScenarios() []Scenario {
	return []Scenario{

		// ─────────────────────────────────────────────────────────────────────
		// S1 EASY: list + read → answer a factual question
		// Any model should pass this in ≤3 steps
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S1_list_and_read", Difficulty: EASY,
			Prompt: `List the files in the workspace root directory using list_dir.
Then read go.mod and tell me the exact Go module name declared in it.`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil { return false, "agent error: " + r.Err.Error() }
				usedList, usedRead := false, false
				for _, s := range r.Steps {
					if s.ToolName == "list_dir" { usedList = true }
					if s.ToolName == "read_file" { usedRead = true }
				}
				if !usedList { return false, "did not call list_dir" }
				if !usedRead { return false, "did not call read_file" }
				if !strings.Contains(r.FinalAnswer, "gonkalabs/gonka-agent") {
					return false, fmt.Sprintf("wrong/missing module name in: %q", trunc(r.FinalAnswer, 120))
				}
				return true, fmt.Sprintf("✓ list+read, module found (%d steps)", len(r.Steps))
			},
		},

		// ─────────────────────────────────────────────────────────────────────
		// S2 MEDIUM: search across code + synthesize multi-file answer
		// Requires: grep → read → analyze → list all items
		// Weak models miss some functions or don't read the file
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S2_exported_functions", Difficulty: MEDIUM,
			Prompt: `Find all exported (uppercase) functions defined in internal/tools/tools.go.
Use grep_search to locate function definitions, then read the file to confirm.
List EVERY exported function name. There are exactly 12 exported functions — do not miss any.`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil { return false, "agent error: " + r.Err.Error() }
				usedGrep, usedRead := false, false
				for _, s := range r.Steps {
					if s.ToolName == "grep_search" { usedGrep = true }
					if s.ToolName == "read_file" { usedRead = true }
				}
				if !usedGrep { return false, "did not call grep_search" }
				if !usedRead { return false, "did not read the file to confirm" }
				expected := []string{
					"ReadFile", "WriteFile", "SearchReplace", "DeleteFile",
					"ListDir", "GlobSearch", "GrepSearch", "RunCommand",
					"WebFetch", "WebSearch", "GetDiagnostics", "AllToolDefs",
				}
				missing := []string{}
				for _, fn := range expected {
					if !strings.Contains(r.FinalAnswer, fn) { missing = append(missing, fn) }
				}
				if len(missing) > 2 {
					return false, fmt.Sprintf("missing %d functions: %v", len(missing), missing)
				}
				return true, fmt.Sprintf("✓ found functions, missing only %v (%d steps)", missing, len(r.Steps))
			},
		},

		// ─────────────────────────────────────────────────────────────────────
		// S3 HARD: multi-step analysis — find exact line + value + context
		// Requires: grep → read with context_lines → precise answer
		// Tests whether model reads evidence and reports exact facts
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S3_find_hardcoded_limit", Difficulty: HARD,
			Prompt: `In internal/tools/tools.go, the GrepSearch function has a hardcoded limit on results.
Your task:
1. Use grep_search to find the line where this limit is defined (search for "1000")
2. Use read_file on tools.go and look at the GrepSearch function to understand the context
3. Report:
   - The exact line number where the limit is set
   - The exact value of the limit
   - What happens when the limit is reached (exact message returned)`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil { return false, "agent error: " + r.Err.Error() }
				ans := strings.ToLower(r.FinalAnswer)
				if !strings.Contains(ans, "1000") {
					return false, fmt.Sprintf("did not identify limit=1000, got: %q", trunc(r.FinalAnswer, 120))
				}
				if !strings.Contains(ans, "truncat") && !strings.Contains(ans, "truncated at") {
					return false, "found 1000 but missing truncation message context"
				}
				return true, fmt.Sprintf("✓ limit=1000 + truncation message identified (%d steps)", len(r.Steps))
			},
		},

		// ─────────────────────────────────────────────────────────────────────
		// S4 VERY HARD: targeted code edit → build verification → self-correction
		// Requires: read → understand → search_replace (exact string) → build → fix if needed
		// Weak models: write wrong replacement, can't find exact string, fail to build
		// Strong models: read carefully, use search_replace precisely, verify build
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S4_targeted_edit_and_build", Difficulty: VERYHARD,
			Prompt: `In internal/tools/tools.go, the GrepSearch function returns a plain "no matches found"
message without telling the user what path was searched.

Your task is to improve this message to include the search path.

Steps:
1. Read internal/tools/tools.go to find the EXACT string(s) that need changing
2. There are exactly TWO occurrences of: return ok("no matches found")
   - One in the rg subprocess branch
   - One in the stdlib fallback branch
3. Use search_replace TWICE to change BOTH to:
   return ok(fmt.Sprintf("no matches found (searched: %s)", searchPath))
4. Run: go build ./...
5. If build fails, read the error and fix it, then rebuild
6. Report: how many replacements you made and whether the build succeeded`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil { return false, "agent error: " + r.Err.Error() }
				editCount := 0
				hasBuild := false
				buildOK := false
				for _, s := range r.Steps {
					if s.ToolName == "search_replace" && strings.Contains(s.ArgsJSON, "no matches found") {
						editCount++
					}
					if s.ToolName == "run_command" && strings.Contains(s.ArgsJSON, "build") {
						hasBuild = true
						// Success = no error flag AND output is either empty or doesn't contain "Error"
						if !s.IsError {
							buildOK = true
						}
					}
				}
				if editCount == 0 { return false, "no search_replace calls targeting 'no matches found'" }
				if !hasBuild { return false, fmt.Sprintf("made %d edit(s) but never ran go build", editCount) }
				if !buildOK { return false, fmt.Sprintf("made %d edit(s), go build FAILED", editCount) }
				return true, fmt.Sprintf("✓ %d replacement(s) + build succeeded (%d steps)", editCount, len(r.Steps))
			},
		},

		// ─────────────────────────────────────────────────────────────────────
		// S5 EXTREME: implement new function + register + build + test
		// This is what separates Claude Opus from smaller models:
		// - Must understand existing code structure (method on Cfg receiver)
		// - Must add to AllToolDefs() correctly (complex map literal)
		// - Must also update MCP server dispatch in cmd/agent/main.go
		// - Must handle build errors iteratively
		// - Must verify via actual MCP server call
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S5_implement_count_lines", Difficulty: EXTREME,
			Prompt: `Add a new tool called "count_lines" to this project. The tool should count
the number of lines in a file.

Requirements:
1. Read internal/tools/tools.go to understand the code style (Cfg receiver methods, Result type)
2. Add this function using search_replace (insert before AllToolDefs):

func (c *Cfg) CountLines(path string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	count := len(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
	return ok(fmt.Sprintf("%s has %d lines", path, count))
}

3. Add "count_lines" to AllToolDefs() in tools.go (use search_replace to insert before the closing bracket)
4. Read cmd/agent/main.go — find the dispatch switch statement — add case "count_lines": return t.CountLines(getString("path"))
5. Also add the tool definition to cmd/loop/main.go toolDefs list — read the file first to find the dispatch switch and add: case "count_lines": return t.CountLines(str("path"))

Wait — cmd/loop/main.go now uses internal/agent package. So instead:
5. Read internal/agent/agent.go — find the Dispatch function switch — add: case "count_lines": return t.CountLines(str("path"))

6. Run: go build ./... — fix any errors
7. Once build succeeds, test via:
   echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"count_lines","arguments":{"path":"go.mod"}}}' | ./bin/gonka-agent-mcp 2>/dev/null | tail -1

8. Report the test result and confirm count_lines returns the number of lines in go.mod`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil { return false, "agent error: " + r.Err.Error() }

				hasCountLines := false
				hasToolsEdit := false
				hasDispatchEdit := false
				hasBuild := false
				buildOK := false
				hasTest := false
				testOK := false

				for _, s := range r.Steps {
					// Implemented the function
					if (s.ToolName == "search_replace" || s.ToolName == "write_file") &&
						strings.Contains(s.ArgsJSON, "CountLines") {
						hasCountLines = true
					}
					// Added to AllToolDefs
					if s.ToolName == "search_replace" &&
						strings.Contains(s.ArgsJSON, "count_lines") &&
						strings.Contains(s.ArgsJSON, "AllToolDefs") ||
						(s.ToolName == "search_replace" &&
							strings.Contains(s.ArgsJSON, "count_lines") &&
							strings.Contains(s.ArgsJSON, "tools.go")) {
						hasToolsEdit = true
					}
					// Added to dispatch
					if (s.ToolName == "search_replace" || s.ToolName == "write_file") &&
						strings.Contains(s.ArgsJSON, "count_lines") &&
						(strings.Contains(s.ArgsJSON, "agent.go") ||
							strings.Contains(s.ArgsJSON, "main.go")) {
						hasDispatchEdit = true
					}
					// Build
					if s.ToolName == "run_command" && strings.Contains(s.ArgsJSON, "build") {
						hasBuild = true
						if !s.IsError { buildOK = true }
					}
					// MCP test
					if s.ToolName == "run_command" && strings.Contains(s.ArgsJSON, "count_lines") {
						hasTest = true
						testOK = strings.Contains(s.Result, "line") || strings.Contains(s.Result, "has ")
					}
				}

				if !hasCountLines { return false, "did not implement CountLines function" }
				if !hasBuild { return false, "implemented but never ran go build" }
				if !buildOK { return false, fmt.Sprintf("go build FAILED (hasDispatch=%v, hasToolsEdit=%v)", hasDispatchEdit, hasToolsEdit) }
				if !hasTest { return false, "build OK but never tested the tool via MCP" }
				if !testOK { return false, "test ran but result doesn't mention lines" }
				return true, fmt.Sprintf("✓ CountLines implemented+dispatched+built+tested (%d steps)", len(r.Steps))
			},
		},
		// ─────────────────────────────────────────────────────────────────────
		// S6 EXTRA_HARD: DECOUPLING — remove a direct dependency between two
		// internal packages while preserving all existing functionality.
		// Only RunPhased (6-phase chain) reliably handles this multi-step task.
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S6_decouple_packages", Difficulty: EXTRAHARD,
			Prompt: `DECOUPLING TASK:
Context: internal/rolechain/chain.go directly imports and calls internal/tools.Cfg methods.
         This creates a tight coupling: rolechain cannot be tested without the full tools layer.

Task:
1. Read internal/rolechain/chain.go and internal/tools/tools.go in full.
2. Introduce a minimal interface ToolReader in internal/rolechain/chain.go that declares only the tool methods chain.go actually calls (e.g. ReadFile, ListDir, GrepSearch).
3. Change Chain struct to hold ToolReader (interface) instead of *tools.Cfg.
4. Update the Chain constructor (NewChain or equivalent) to accept ToolReader.
5. Ensure internal/tools/tools.go Cfg already satisfies ToolReader without changes (it does via its methods).
6. Run go build ./... — must pass with zero errors.
7. Run go vet ./... — must pass.
8. The Scientist-Validator marks PROVEN if: build passes, vet passes, and Chain no longer imports tools.Cfg by type (only via interface).`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil {
					return false, "run error: " + r.Err.Error()
				}
				if r.ScientistFrame.Verdict == "PROVEN" {
					return true, fmt.Sprintf("✓ PROVEN: %s", shorten(r.ScientistFrame.VerdictReason, 80))
				}
				// Fallback: check for interface introduction + clean build.
				hasInterface, hasBuild, hasVet := false, false, false
				for _, s := range r.Steps {
					if s.ToolName == "search_replace" && strings.Contains(s.ArgsJSON, "ToolReader") {
						hasInterface = true
					}
					if s.ToolName == "run_command" {
						if strings.Contains(s.ArgsJSON, "go build") && !s.IsError {
							hasBuild = true
						}
						if strings.Contains(s.ArgsJSON, "go vet") && !s.IsError {
							hasVet = true
						}
					}
				}
				if hasInterface && hasBuild && hasVet {
					return true, fmt.Sprintf("✓ interface+build+vet confirmed (%d steps)", len(r.Steps))
				}
				if hasInterface && hasBuild {
					return false, fmt.Sprintf("interface+build ok but vet not run (%d steps)", len(r.Steps))
				}
				return false, fmt.Sprintf("missing: interface=%v build=%v vet=%v (%d steps)", hasInterface, hasBuild, hasVet, len(r.Steps))
			},
		},

		// ─────────────────────────────────────────────────────────────────────
		// S7 EXTRA_HARD: MULTI-FILE REFACTOR — rename a symbol across 3+ files.
		// Tests: verify_replace, scope awareness, dependency_graph, no dead-ends.
		// ─────────────────────────────────────────────────────────────────────
		{
			ID: "S7_multifile_refactor", Difficulty: EXTRAHARD,
			Prompt: `MULTI-FILE REFACTOR TASK:
Context: The type 'Result' in internal/tools/tools.go is used across multiple files.
Task: Rename the 'Content' field of the Result struct to 'Output' across ALL files where it is accessed.

Steps you MUST follow:
1. Use dependency_graph to find all files that import internal/tools
2. Use grep_search to find all occurrences of ".Content" on a Result value across .go files
3. Use verify_replace on EACH file to count occurrences before editing
4. Use search_replace for each file — ONLY replace occurrences that are on Result.Content, not other structs
5. After all replacements: run go build ./... 
6. If build fails: use view_range to read exact error, use git_reset_file for any bad change, retry
7. Run go vet ./... to confirm clean
8. Use git_diff to show all changes
9. The Scientist-Validator verdict should be PROVEN if: build passes, vet passes, git diff shows exactly the .Content→.Output renames and nothing else broke.

IMPORTANT: if a file has a different struct with .Content that should NOT be renamed, skip it.`,
			Verify: func(r agent.RunResult) (bool, string) {
				if r.Err != nil {
					return false, "run error: " + r.Err.Error()
				}
				if r.ScientistFrame.Verdict == "PROVEN" {
					return true, fmt.Sprintf("✓ PROVEN: %s", shorten(r.ScientistFrame.VerdictReason, 80))
				}
				// Check steps for multi-file awareness
				hasVerify, hasMultiFile, hasBuild, hasVet := false, false, false, false
				filesEdited := map[string]bool{}
				for _, s := range r.Steps {
					if s.ToolName == "verify_replace" { hasVerify = true }
					if s.ToolName == "search_replace" {
						var args map[string]any
						_ = json.Unmarshal([]byte(s.ArgsJSON), &args)
						if p, ok := args["path"].(string); ok { filesEdited[p] = true }
					}
					if s.ToolName == "run_command" && strings.Contains(s.ArgsJSON, "build") && !s.IsError { hasBuild = true }
					if s.ToolName == "run_command" && strings.Contains(s.ArgsJSON, "vet") && !s.IsError { hasVet = true }
				}
				if len(filesEdited) >= 2 { hasMultiFile = true }
				if hasVerify && hasMultiFile && hasBuild {
					msg := fmt.Sprintf("✓ %d files edited, verify+build passed", len(filesEdited))
					if hasVet { msg += "+vet" }
					return true, msg
				}
				return false, fmt.Sprintf("verify=%v multifile=%v(%d) build=%v vet=%v",
					hasVerify, hasMultiFile, len(filesEdited), hasBuild, hasVet)
			},
		},
	}
}

// ─── Matrix row ───────────────────────────────────────────────────────────────

type Row struct {
	ScenarioID string
	Difficulty Difficulty
	Model      string
	Passed     bool
	Reason     string
	Steps      int
	ToolsUsed  string
	ElapsedMs  int64
}

// ─── Run matrix ───────────────────────────────────────────────────────────────

const systemPrompt = `You are a precise, methodical coding agent. Use tools to complete tasks.

RULES (follow strictly):
1. Always read files before editing — never assume content
2. Use search_replace for targeted edits with exact strings
3. After any code change, run go build ./... to verify
4. If build fails: read error carefully, identify problem, fix with search_replace, rebuild
5. Report results precisely — include exact values, line numbers, error messages`

func runMatrix(cfg *config.Config, t *tools.Cfg, models []string, scenarios []Scenario, verbose bool) []Row {
	var rows []Row
	for _, model := range models {
		fmt.Printf("\n\033[1m╔══ MODEL: %s ══╗\033[0m\n", model)
		for _, sc := range scenarios {
			fmt.Printf("\n  %s\033[1m[%s]\033[0m %s\n", sc.Difficulty.ANSI(), sc.Difficulty.String(), sc.ID)

			// Reset code modifications from previous runs
			if sc.Difficulty >= VERYHARD {
				resetCodeChanges(t)
			}

			t0 := time.Now()
			client := agent.NewClient(cfg.GonkaDirectURL, cfg.GonkaAPIKey, model)
			pool := roles.NewLLMPool(cfg.GonkaSourceURL, model, cfg.GonkaAPIKeys)
			var result agent.RunResult
			matrixProgress := func(event, detail string) {
				if verbose {
					fmt.Printf("  [%s] %s\n", event, detail)
				}
			}
			if sc.Difficulty >= EXTRAHARD {
				fmt.Printf("  [PHASED MODE — Plan-First Chain | pool=%d keys]\n", pool.Size())
				result = agent.RunPhased(pool, client, t, sc.Prompt, matrixProgress)
			} else {
				result = agent.Run(client, t, systemPrompt, sc.Prompt, 30, matrixProgress)
			}
			elapsed := time.Since(t0).Milliseconds()

			passed, reason := sc.Verify(result)

			toolSet := map[string]bool{}
			for _, s := range result.Steps { toolSet[s.ToolName] = true }
			toolList := make([]string, 0, len(toolSet))
			for k := range toolSet { toolList = append(toolList, k) }

			rows = append(rows, Row{
				ScenarioID: sc.ID, Difficulty: sc.Difficulty, Model: model,
				Passed: passed, Reason: reason, Steps: len(result.Steps),
				ToolsUsed: strings.Join(toolList, ","), ElapsedMs: elapsed,
			})

			icon := "❌"
			if passed { icon = "✅" }
			fmt.Printf("  %s %s  (%ds, %d steps)\n", icon, reason, elapsed/1000, len(result.Steps))
		}
	}
	return rows
}

// resetCodeChanges undoes modifications made by VERYHARD/EXTREME scenarios
func resetCodeChanges(t *tools.Cfg) {
	// Try git checkout first (cleanest)
	r := t.RunCommand("git checkout internal/tools/tools.go internal/agent/agent.go cmd/agent/main.go 2>&1")
	if !r.IsError {
		return
	}
	// Manual undo of S4: restore plain "no matches found"
	t.SearchReplace(
		"internal/tools/tools.go",
		`return ok(fmt.Sprintf("no matches found (searched: %s)", searchPath))`,
		`return ok("no matches found")`,
	)
}

// ─── Print table ─────────────────────────────────────────────────────────────

const (
	reset = "\033[0m"
	bold  = "\033[1m"
	green = "\033[32m"
	red   = "\033[31m"
)

func printTable(rows []Row) {
	divider := strings.Repeat("═", 108)
	fmt.Printf("\n%s%s\n%s  GONKA AGENT — MATRIX TEST RESULTS%s\n%s%s\n",
		bold, divider, bold, reset, bold, divider)

	models := uniqueModels(rows)
	totals := map[string][2]int{} // [pass, total]

	for _, model := range models {
		fmt.Printf("\n%s%s  Model: %-60s%s\n", bold, "\033[44m", model, reset)
		fmt.Printf("  %-28s %-10s %-8s %-6s %-10s %s\n",
			"Scenario", "Level", "Result", "Steps", "Time(s)", "Tools Used")
		fmt.Println("  " + strings.Repeat("─", 100))

		pass, total := 0, 0
		for _, row := range rows {
			if row.Model != model { continue }
			total++
			icon := red + "❌ FAIL" + reset
			if row.Passed { icon = green + "✅ PASS" + reset; pass++ }
			tools := row.ToolsUsed
			if len(tools) > 45 { tools = tools[:45] + ".." }
			fmt.Printf("  %-28s %s%-10s%s %-8s %-6d %-10.1f %s\n",
				row.ScenarioID,
				row.Difficulty.ANSI(), row.Difficulty.String(), reset,
				icon,
				row.Steps,
				float64(row.ElapsedMs)/1000,
				tools,
			)
		}
		pct := 0
		if total > 0 { pct = pass * 100 / total }
		totals[model] = [2]int{pass, total}
		fmt.Printf("\n  Score: %s%d/%d (%d%%)%s\n", bold, pass, total, pct, reset)
	}

	// Cross-model comparison (only if multiple models)
	if len(models) > 1 {
		fmt.Printf("\n%s%s\n%s  CROSS-MODEL COMPARISON%s\n%s%s\n",
			bold, divider, bold, reset, bold, divider)

		scenarios := uniqueScenarios(rows)
		header := fmt.Sprintf("  %-28s", "Scenario")
		for _, m := range models { header += fmt.Sprintf("  %-22s", shortModel(m)) }
		fmt.Println(header)
		fmt.Println("  " + strings.Repeat("─", 28+len(models)*24))

		for _, sc := range scenarios {
			line := fmt.Sprintf("  %-28s", sc)
			for _, m := range models {
				cell := red + "❌ N/A" + reset
				for _, r := range rows {
					if r.ScenarioID == sc && r.Model == m {
						if r.Passed {
							cell = fmt.Sprintf("%s✅ %ds%s", green, r.ElapsedMs/1000, reset)
						} else {
							cell = fmt.Sprintf("%s❌ %s%s", red, trunc(r.Reason, 16), reset)
						}
					}
				}
				line += fmt.Sprintf("  %-22s", cell)
			}
			fmt.Println(line)
		}

		// Summary
		fmt.Printf("\n%s  SUMMARY%s\n", bold, reset)
		for _, m := range models {
			t := totals[m]
			pct := 0
			if t[1] > 0 { pct = t[0] * 100 / t[1] }
			bar := progressBar(pct)
			fmt.Printf("  %-55s %s %d/%d (%d%%)\n", shortModel(m), bar, t[0], t[1], pct)
		}
	}

	fmt.Println("\n" + bold + divider + reset)
}

func progressBar(pct int) string {
	filled := pct / 10
	bar := "[" + strings.Repeat("█", filled) + strings.Repeat("░", 10-filled) + "]"
	if pct >= 80 { return green + bar + reset }
	if pct >= 40 { return "\033[33m" + bar + reset }
	return red + bar + reset
}

func uniqueModels(rows []Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if !seen[r.Model] { seen[r.Model] = true; out = append(out, r.Model) }
	}
	return out
}

func uniqueScenarios(rows []Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if !seen[r.ScenarioID] { seen[r.ScenarioID] = true; out = append(out, r.ScenarioID) }
	}
	return out
}

func shortModel(m string) string {
	parts := strings.Split(m, "/")
	return parts[len(parts)-1]
}

func trunc(s string, n int) string {
	if len(s) <= n { return s }
	return s[:n] + "…"
}

func shorten(s string, n int) string {
	if len(s) <= n { return s }
	return s[:n] + "…"
}

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

	modelsEnv := os.Getenv("AGENT_MODELS")
	var models []string
	if modelsEnv != "" {
		for _, m := range strings.Split(modelsEnv, ",") {
			if m = strings.TrimSpace(m); m != "" { models = append(models, m) }
		}
	}
	if len(models) == 0 {
		models = []string{getEnv("AGENT_MODEL", "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")}
	}

	filter := os.Getenv("AGENT_SCENARIOS")
	allScenarios := buildScenarios()
	scenarios := allScenarios
	if filter != "" {
		scenarios = nil
		for _, sc := range allScenarios {
			for _, f := range strings.Split(filter, ",") {
				if strings.HasPrefix(sc.ID, strings.TrimSpace(f)) {
					scenarios = append(scenarios, sc)
					break
				}
			}
		}
	}

	verbose := os.Getenv("AGENT_VERBOSE") == "1"

	fmt.Printf("\n%sGONKA AGENT — MATRIX TEST%s\n", bold, reset)
	fmt.Printf("Models:    %s\n", strings.Join(models, ", "))
	fmt.Printf("Scenarios: %d\n", len(scenarios))
	fmt.Printf("Workspace: %s\n", cfg.Workspace)

	rows := runMatrix(cfg, t, models, scenarios, verbose)
	printTable(rows)
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}
