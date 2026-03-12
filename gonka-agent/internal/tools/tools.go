package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

func isExitError(err error) bool {
	if err == nil { return false }
	_, ok := err.(*exec.ExitError)
	return ok
}

// ─── Result ───────────────────────────────────────────────────────────────────

type Result struct {
	Content string
	IsError bool
}

func ok(s string) Result  { return Result{Content: s} }
func fail(s string) Result { return Result{Content: s, IsError: true} }

// Exported for use by agent package.
func OkResult(s string) Result   { return Result{Content: s} }
func FailResult(s string) Result { return Result{Content: s, IsError: true} }

// ─── Config ──────────────────────────────────────────────────────────────────

type Cfg struct {
	Workspace       string
	Shell           string
	CmdTimeout      time.Duration
	MaxFileSize     int64
	AllowShell      bool
	RgPath          string
	WebSearchKey    string
	WebFetchTimeout time.Duration
	WebFetchMaxSize int64
	// SearXNG local instance URL (e.g. http://localhost:8888)
	SearXNGURL string
	// Gonka API for semantic search via /v1/embeddings
	GonkaBaseURL string
	GonkaAPIKey  string
	EmbedModel   string
}

// safePath resolves path strictly inside workspace.
// If the direct path does not exist, walks workspace to find a unique match
// by filename — handles cases where LLM omits directory prefix (e.g. "tools.go"
// instead of "internal/tools/tools.go").
func (c *Cfg) safePath(rel string) (string, error) {
	var target string
	if filepath.IsAbs(rel) {
		target = filepath.Clean(rel)
	} else {
		target = filepath.Join(c.Workspace, rel)
	}
	wsAbs, _ := filepath.Abs(c.Workspace)
	tAbs, _ := filepath.Abs(target)
	if !strings.HasPrefix(tAbs, wsAbs) {
		return "", fmt.Errorf("path %q escapes workspace %q", rel, c.Workspace)
	}
	// Fast path: file exists at direct location.
	if _, err := os.Stat(target); err == nil {
		return target, nil
	}
	// Fuzzy path: walk workspace and collect all paths ending with the given name.
	base := filepath.Base(rel)
	var matches []string
	_ = filepath.Walk(wsAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == base {
			matches = append(matches, path)
		}
		return nil
	})
	if len(matches) == 1 {
		return matches[0], nil
	}
	// Multiple matches or none — return original (let caller handle error).
	return target, nil
}

// ─── FILE: read_file ─────────────────────────────────────────────────────────

func (c *Cfg) ReadFile(path string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail(err.Error())
	}
	if info.Size() > c.MaxFileSize {
		return fail(fmt.Sprintf("file too large: %d bytes (max %d)", info.Size(), c.MaxFileSize))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	if !utf8.Valid(data) {
		return fail("file is not valid UTF-8 (binary file?)")
	}
	return ok(string(data))
}

// ─── FILE: write_file ────────────────────────────────────────────────────────

func (c *Cfg) WriteFile(path, content string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail(err.Error())
	}
	if err = os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fail(err.Error())
	}
	return ok(fmt.Sprintf("written %d bytes to %s", len(content), path))
}

// ─── FILE: search_replace ────────────────────────────────────────────────────
// Finds all occurrences of old_str in file and replaces with new_str.
// Returns how many replacements were made.

func (c *Cfg) SearchReplace(path, oldStr, newStr string) Result {
	if oldStr == "" {
		return fail("old_str cannot be empty")
	}
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	if !utf8.Valid(data) {
		return fail("file is not valid UTF-8")
	}
	original := string(data)
	count := strings.Count(original, oldStr)
	if count == 0 {
		return fail(fmt.Sprintf("old_str not found in %s", path))
	}
	updated := strings.ReplaceAll(original, oldStr, newStr)
	if err = os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return fail(err.Error())
	}
	return ok(fmt.Sprintf("replaced %d occurrence(s) in %s", count, path))
}

// ─── FILE: delete_file ───────────────────────────────────────────────────────

func (c *Cfg) DeleteFile(path string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	if err = os.Remove(abs); err != nil {
		return fail(err.Error())
	}
	return ok("deleted: " + path)
}

// ─── FILE: list_dir ──────────────────────────────────────────────────────────

func (c *Cfg) ListDir(path string) Result {
	if path == "" {
		path = "."
	}
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return fail(err.Error())
	}
	var sb strings.Builder
	for _, e := range entries {
		tag := "F"
		if e.IsDir() {
			tag = "D"
		}
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		sb.WriteString(fmt.Sprintf("[%s] %-40s %d bytes\n", tag, e.Name(), size))
	}
	if sb.Len() == 0 {
		return ok("(empty directory)")
	}
	return ok(sb.String())
}

// ─── FILE: glob ──────────────────────────────────────────────────────────────

func (c *Cfg) GlobSearch(pattern string) Result {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(c.Workspace, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fail(err.Error())
	}
	if len(matches) == 0 {
		return ok("no matches found")
	}
	return ok(strings.Join(matches, "\n"))
}

// ─── SEARCH: grep_search ─────────────────────────────────────────────────────

func (c *Cfg) GrepSearch(pattern, searchPath string, contextLines int, caseSensitive bool) Result {
	if searchPath == "" {
		searchPath = c.Workspace
	} else {
		abs, err := c.safePath(searchPath)
		if err != nil {
			return fail(err.Error())
		}
		searchPath = abs
	}

	// Try ripgrep first (fast)
	if c.RgPath != "" {
		args := []string{"--line-number", "--with-filename"}
		if !caseSensitive {
			args = append(args, "--ignore-case")
		}
		if contextLines > 0 {
			args = append(args, fmt.Sprintf("--context=%d", contextLines))
		}
		args = append(args, "--", pattern, searchPath)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		cmd := exec.CommandContext(ctx2, c.RgPath, args...)
		out, rgErr := cmd.Output()
		if ctx2.Err() == nil {
			// rg ran (didn't timeout): output present = matches found, empty = no matches
			// Only use rg result if the binary was actually found (not "exec: not found")
			if rgErr == nil || (len(out) == 0 && isExitError(rgErr)) {
				if len(out) > 0 {
					return ok(string(out))
				}
				return ok("no matches found")
			}
			// rg not found or other exec error — fall through to stdlib
		}
	}

	// Fallback: stdlib regexp walk
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fail("invalid regex: " + err.Error())
	}
	if !caseSensitive {
		re, _ = regexp.Compile("(?i)" + pattern)
	}

	var sb strings.Builder
	count := 0
	_ = filepath.Walk(searchPath, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || !utf8.Valid(data) {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				sb.WriteString(fmt.Sprintf("%s:%d: %s\n", p, i+1, line))
				count++
				if count > 1000 {
					sb.WriteString("... (truncated at 1000 matches)\n")
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if sb.Len() == 0 {
		return ok("no matches found")
	}
	return ok(sb.String())
}

// ─── SHELL: run_command ───────────────────────────────────────────────────────

func (c *Cfg) RunCommand(cmdStr string) Result {
	if !c.AllowShell {
		return fail("shell execution disabled (AGENT_ALLOW_SHELL=false)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.CmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Shell, "-c", cmdStr)
	cmd.Dir = c.Workspace
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return fail(fmt.Sprintf("command timed out after %s\nOutput: %s", c.CmdTimeout, output))
	}
	if err != nil {
		if output == "" {
			output = err.Error()
		}
		return Result{Content: output, IsError: true}
	}
	if output == "" {
		output = "(no output — command succeeded)"
	}
	return ok(output)
}

// ─── WEB: web_fetch ───────────────────────────────────────────────────────────

func (c *Cfg) WebFetch(url string) Result {
	return c.WebFetchWithHeaders(url, nil)
}

func (c *Cfg) WebFetchWithHeaders(url string, headers map[string]string) Result {
	timeout := c.WebFetchTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	const maxRetries = 3
	var lastErr string

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		client := &http.Client{Timeout: timeout}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fail(err.Error())
		}
		req.Header.Set("User-Agent", "gonka-agent/1.0 (bookworm)")
		req.Header.Set("Accept", "text/html,text/plain,application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Sprintf("attempt %d/%d: %v", attempt+1, maxRetries, err)
			continue
		}

		lr := io.LimitReader(resp.Body, c.WebFetchMaxSize)
		data, readErr := io.ReadAll(lr)
		resp.Body.Close()

		if readErr != nil {
			lastErr = fmt.Sprintf("attempt %d/%d: read body: %v", attempt+1, maxRetries, readErr)
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Sprintf("attempt %d/%d: HTTP %d", attempt+1, maxRetries, resp.StatusCode)
			continue
		}

		text := stripHTML(string(data))
		text = strings.TrimSpace(text)
		if text == "" {
			text = fmt.Sprintf("(empty, status %d)", resp.StatusCode)
		}
		return ok(fmt.Sprintf("URL: %s\nStatus: %d\n\n%s", url, resp.StatusCode, text))
	}

	return fail(fmt.Sprintf("web_fetch failed after %d retries for %s: %s", maxRetries, url, lastErr))
}

func stripHTML(s string) string {
	tagRe := regexp.MustCompile(`<[^>]+>`)
	s = tagRe.ReplaceAllString(s, " ")
	replacements := map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">",
		"&quot;": `"`, "&#39;": "'", "&nbsp;": " ",
	}
	for k, v := range replacements {
		s = strings.ReplaceAll(s, k, v)
	}
	wsRe := regexp.MustCompile(`[ \t]+`)
	s = wsRe.ReplaceAllString(s, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	s = nlRe.ReplaceAllString(s, "\n\n")
	return s
}

// ─── WEB: web_search ─────────────────────────────────────────────────────────

func (c *Cfg) WebSearch(query string, numResults int) Result {
	if numResults <= 0 {
		numResults = 10
	}
	timeout := c.WebFetchTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	// Try SearXNG first, then DuckDuckGo HTML as fallback.
	results := c.trySearXNG(query, numResults, timeout)
	if results != "" {
		return ok(results)
	}
	results = c.tryDDGFallback(query, numResults, timeout)
	if results != "" {
		return ok(results)
	}
	return ok("no results found for: " + query + " (SearXNG unavailable, DDG fallback returned nothing)")
}

func (c *Cfg) trySearXNG(query string, numResults int, timeout time.Duration) string {
	searxURL := c.SearXNGURL
	if searxURL == "" {
		searxURL = "http://localhost:8888"
	}

	encoded := strings.ReplaceAll(query, " ", "+")
	searchURL := searxURL + "/search?q=" + encoded + "&format=json"

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		client := &http.Client{Timeout: timeout}
		req, err := http.NewRequest("GET", searchURL, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			continue
		}

		var searxResp struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &searxResp); err != nil || len(searxResp.Results) == 0 {
			continue
		}

		var sb strings.Builder
		for i, r := range searxResp.Results {
			if i >= numResults {
				break
			}
			sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content))
		}
		return sb.String()
	}
	return ""
}

func (c *Cfg) tryDDGFallback(query string, numResults int, timeout time.Duration) string {
	encoded := strings.ReplaceAll(query, " ", "+")
	ddgURL := "https://html.duckduckgo.com/html/?q=" + encoded

	client := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest("GET", ddgURL, nil)
	if req == nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 200000))

	body := string(data)
	titleRe := regexp.MustCompile(`<a[^>]+class="result__a"[^>]*>([^<]+)</a>`)
	urlRe := regexp.MustCompile(`<a[^>]+class="result__url"[^>]*href="([^"]+)"`)
	snippetRe := regexp.MustCompile(`<a[^>]+class="result__snippet"[^>]*>([^<]+)`)

	titles := titleRe.FindAllStringSubmatch(body, numResults)
	urls := urlRe.FindAllStringSubmatch(body, numResults)
	snippets := snippetRe.FindAllStringSubmatch(body, numResults)

	if len(titles) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[DDG fallback]\n\n")
	for i := 0; i < len(titles) && i < numResults; i++ {
		title := titles[i][1]
		u := ""
		if i < len(urls) {
			u = urls[i][1]
		}
		snippet := ""
		if i < len(snippets) {
			snippet = snippets[i][1]
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, title, u, snippet))
	}
	return sb.String()
}

// ─── DIAGNOSTICS: get_diagnostics ────────────────────────────────────────────

func (c *Cfg) GetDiagnostics(path string) Result {
	if path == "" {
		path = "./..."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Run both go vet and go build to catch all issues
	cmd := exec.CommandContext(ctx, "go", "vet", path)
	cmd.Dir = c.Workspace
	out, _ := cmd.CombinedOutput()
	if len(strings.TrimSpace(string(out))) == 0 {
		return ok("go vet: no issues found — code is clean")
	}
	return ok("go vet output:\n" + string(out))
}

// ─── FILE: verify_replace (dry-run, no modification) ─────────────────────────
// Shows every occurrence of old_str with ±5 lines of context and scope info.

func (c *Cfg) VerifyReplace(path, oldStr string) Result {
	if oldStr == "" {
		return fail("old_str cannot be empty")
	}
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	if !utf8.Valid(data) {
		return fail("file is not valid UTF-8")
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return fail(fmt.Sprintf("old_str not found in %s — nothing to replace", path))
	}

	lines := strings.Split(content, "\n")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("verify_replace DRY-RUN: found %d occurrence(s) of target string in %s\n", count, path))
	sb.WriteString(strings.Repeat("─", 60) + "\n")

	occurrence := 0
	for i, line := range lines {
		if !strings.Contains(line, oldStr) {
			continue
		}
		occurrence++
		sb.WriteString(fmt.Sprintf("\nOccurrence %d/%d — line %d:\n", occurrence, count, i+1))
		// Context: ±5 lines
		start := i - 5
		if start < 0 {
			start = 0
		}
		end := i + 5
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := start; j <= end; j++ {
			marker := "  "
			if j == i {
				marker = "> "
			}
			sb.WriteString(fmt.Sprintf("%s%4d: %s\n", marker, j+1, lines[j]))
		}
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d occurrence(s). Verify scope of each before proceeding with search_replace.\n", count))
	return ok(sb.String())
}

// ─── FILE: view_range ────────────────────────────────────────────────────────
// Reads specific lines from a file with line numbers. Surgical reading.

func (c *Cfg) ViewRange(path string, startLine, endLine int) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	lines := strings.Split(string(data), "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine {
		return fail(fmt.Sprintf("start_line %d > end_line %d", startLine, endLine))
	}
	var sb strings.Builder
	for i := startLine - 1; i < endLine && i < len(lines); i++ {
		sb.WriteString(fmt.Sprintf("%4d│ %s\n", i+1, lines[i]))
	}
	return ok(sb.String())
}

// ─── CODE: code_analysis ─────────────────────────────────────────────────────
// Uses go/ast to analyse a symbol: signature, callers, callees.

func (c *Cfg) CodeAnalysis(path, symbol string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	// Use 'go doc' for symbol info, 'grep' for callers.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Doc lookup.
	docCmd := exec.CommandContext(ctx, "go", "doc", "-all", "./"+strings.TrimSuffix(filepath.Dir(abs), c.Workspace+"/"))
	docCmd.Dir = c.Workspace
	docOut, _ := docCmd.Output()

	// Find callers via grep.
	grepCmd := exec.CommandContext(ctx, "grep", "-rn", symbol, c.Workspace)
	grepOut, _ := grepCmd.Output()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Symbol analysis: %s in %s\n\n", symbol, path))
	if len(docOut) > 0 {
		sb.WriteString("Documentation:\n")
		// Find relevant lines.
		for _, line := range strings.Split(string(docOut), "\n") {
			if strings.Contains(line, symbol) {
				sb.WriteString("  " + line + "\n")
			}
		}
	}
	if len(grepOut) > 0 {
		sb.WriteString("\nReferences:\n")
		lines := strings.Split(string(grepOut), "\n")
		for _, l := range lines {
			if l != "" && !strings.Contains(l, "_test.go") {
				sb.WriteString("  " + l + "\n")
			}
		}
	}
	return ok(sb.String())
}

// ─── CODE: dependency_graph ───────────────────────────────────────────────────
// Shows what a package imports and what imports it.

func (c *Cfg) DependencyGraph(path string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-f",
		"{{.ImportPath}}\nImports: {{.Imports}}\nImportedBy: {{.Dependents}}",
		"./...")
	cmd.Dir = c.Workspace
	out, _ := cmd.CombinedOutput()
	if len(out) == 0 {
		// Fallback: grep imports.
		grep := exec.CommandContext(ctx, "grep", "-rn", `^import\|"github.com`, c.Workspace)
		grep.Dir = c.Workspace
		grepOut, _ := grep.Output()
		return ok("Dependency graph (grep fallback):\n" + string(grepOut))
	}
	return ok(string(out))
}

// ─── TEST: run_tests ─────────────────────────────────────────────────────────
// Runs go test and returns structured pass/fail per test.

type TestResult struct {
	Name   string
	Passed bool
	Output string
}

func (c *Cfg) RunTests(path string) Result {
	if path == "" {
		path = "./..."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Run go vet first to catch static errors (deadlocks, printf mismatches, etc.)
	vetCtx, vetCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer vetCancel()
	vetCmd := exec.CommandContext(vetCtx, "go", "vet", path)
	vetCmd.Dir = c.Workspace
	vetOut, vetErr := vetCmd.CombinedOutput()

	var sb strings.Builder
	if vetErr != nil {
		sb.WriteString("⚠️ go vet issues:\n" + string(vetOut) + "\n")
	}

	// Run tests with race detector enabled
	cmd := exec.CommandContext(ctx, "go", "test", "-v", "-race", "-timeout", "90s", "-count=1", path)
	cmd.Dir = c.Workspace
	out, _ := cmd.CombinedOutput()

	pass, fail := 0, 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "--- PASS:") {
			pass++
			sb.WriteString("✅ " + strings.TrimPrefix(line, "--- PASS: ") + "\n")
		} else if strings.HasPrefix(line, "--- FAIL:") {
			fail++
			sb.WriteString("❌ " + strings.TrimPrefix(line, "--- FAIL: ") + "\n")
		} else if strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok") {
			sb.WriteString(line + "\n")
		} else if strings.Contains(line, "DATA RACE") {
			fail++
			sb.WriteString("🔴 DATA RACE DETECTED\n")
		}
	}
	sb.WriteString(fmt.Sprintf("\nSummary: %d passed, %d failed\n", pass, fail))
	if fail > 0 || vetErr != nil {
		sb.WriteString("\nFull output:\n" + string(out))
		return Result{Content: sb.String(), IsError: true}
	}
	return ok(sb.String())
}

// ─── VCS: git_diff / git_status / git_reset_file ─────────────────────────────

func (c *Cfg) GitDiff() Result {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--stat", "HEAD")
	cmd.Dir = c.Workspace
	out, _ := cmd.CombinedOutput()
	if len(strings.TrimSpace(string(out))) == 0 {
		return ok("No changes (git diff is empty)")
	}
	// Also get the actual diff (truncated).
	cmd2 := exec.CommandContext(ctx, "git", "diff", "HEAD")
	cmd2.Dir = c.Workspace
	diff, _ := cmd2.Output()
	result := string(out) + "\n" + string(diff)
	if len(result) > 8000 {
		result = result[:8000] + "\n...(truncated)"
	}
	return ok(result)
}

func (c *Cfg) GitStatus() Result {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--short")
	cmd.Dir = c.Workspace
	out, _ := cmd.CombinedOutput()
	return ok(strings.TrimSpace(string(out)))
}

func (c *Cfg) GitResetFile(path string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "checkout", abs)
	cmd.Dir = c.Workspace
	out, cmdErr := cmd.CombinedOutput()
	if cmdErr != nil {
		return fail(fmt.Sprintf("git checkout failed: %s\n%s", cmdErr, out))
	}
	return ok(fmt.Sprintf("Reverted %s to HEAD", path))
}

// ─── AllToolDefs returns all tool definitions for MCP and API payloads ────────

func AllToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "read_file",
			"description": "Read a file from the workspace. Returns full file content as text.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path (relative to workspace or absolute)"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "write_file",
			"description": "Write (create or overwrite) a file with the given content.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "search_replace",
			"description": "Find old_str in a file and replace all occurrences with new_str. Precise surgical edit — better than rewriting the whole file.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"old_str": map[string]any{"type": "string", "description": "Exact string to find (must exist in file)"},
					"new_str": map[string]any{"type": "string", "description": "Replacement string"},
				},
				"required": []string{"path", "old_str", "new_str"},
			},
		},
		{
			"name":        "delete_file",
			"description": "Delete a file from the workspace.",
			"inputSchema": map[string]any{
				"type":     "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required": []string{"path"},
			},
		},
		{
			"name":        "list_dir",
			"description": "List contents of a directory (files and subdirectories with sizes).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Directory path (empty = workspace root)"},
				},
			},
		},
		{
			"name":        "glob",
			"description": "Find files matching a glob pattern (e.g. **/*.go, cmd/*/main.go).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "grep_search",
			"description": "Search for a regex pattern across files. Returns filename:line:content for each match.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":        map[string]any{"type": "string"},
					"path":           map[string]any{"type": "string", "description": "Directory to search (empty = workspace)"},
					"context_lines":  map[string]any{"type": "integer"},
					"case_sensitive": map[string]any{"type": "boolean"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "run_command",
			"description": "Execute a shell command in the workspace directory. Returns stdout+stderr and exit status.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []string{"cmd"},
			},
		},
		{
			"name":        "web_fetch",
			"description": "Fetch a URL and return its text content (HTML is stripped). Supports custom headers for API authentication.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string", "description": "URL to fetch"},
					"headers": map[string]any{"type": "object", "description": "Optional HTTP headers (e.g. {\"X-API-Key\": \"key\"})"},
				},
				"required": []string{"url"},
			},
		},
		{
			"name":        "web_search",
			"description": "Search the web via Brave Search API. Returns titles, URLs, descriptions.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       map[string]any{"type": "string"},
					"num_results": map[string]any{"type": "integer"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "get_diagnostics",
			"description": "Run go vet to check for code issues. Returns errors or confirmation that code is clean.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
		{
			"name":        "verify_replace",
			"description": "DRY-RUN: find all occurrences of old_str in file and show ±5 lines context for EACH. Does NOT modify the file. Call this BEFORE search_replace to verify count and scope.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"old_str": map[string]any{"type": "string"},
				},
				"required": []string{"path", "old_str"},
			},
		},
		{
			"name":        "view_range",
			"description": "Read specific lines from a file with line numbers. Use instead of read_file when you only need a small section.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string"},
					"start_line": map[string]any{"type": "integer"},
					"end_line":   map[string]any{"type": "integer"},
				},
				"required": []string{"path", "start_line", "end_line"},
			},
		},
		{
			"name":        "code_analysis",
			"description": "Analyse a symbol (function/type) in a Go file: find its signature, all callers, all callees.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string"},
					"symbol": map[string]any{"type": "string"},
				},
				"required": []string{"path", "symbol"},
			},
		},
		{
			"name":        "dependency_graph",
			"description": "Show what a Go package imports and what imports it. Use before modifying exported symbols.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
		{
			"name":        "run_tests",
			"description": "Run go test and return structured PASS/FAIL per test with summary count.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Package path, e.g. ./... or ./internal/tools/"},
				},
			},
		},
		{
			"name":        "git_diff",
			"description": "Show all changes since last commit (git diff HEAD). Use in VALIDATE phase.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "git_status",
			"description": "Show current git status (modified/untracked files).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "git_reset_file",
			"description": "Revert a single file to HEAD (rollback). Use when a change caused a dead-end error.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "count_lines",
			"description": "Count total lines in a file and list all symbol (function/type/const/var) definitions with their line numbers.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "todo_write",
			"description": "Update the task TODO list. Call this when you have a multi-step plan. Items have: id (string), content (string), status (pending|in_progress|completed|cancelled).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":      map[string]any{"type": "string"},
								"content": map[string]any{"type": "string"},
								"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}},
							},
							"required": []string{"id", "content", "status"},
						},
					},
				},
				"required": []string{"todos"},
			},
		},
		{
			"name":        "todo_read",
			"description": "Read the current TODO list.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "memory_write",
			"description": "Save notes to gonka.md (project memory, persists across sessions). Use to remember key decisions, context, or user preferences.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{"type": "string", "description": "Markdown content to append to gonka.md"},
				},
				"required": []string{"content"},
			},
		},
	}
}

// WorkspaceSnapshot returns a compact file tree of the workspace for injection into system prompt.
// Skips hidden dirs, vendor, node_modules, __pycache__, etc.
func (c *Cfg) WorkspaceSnapshot() string {
	var sb strings.Builder
	skipDirs := map[string]bool{
		".git": true, ".gonka-cache": true, "vendor": true,
		"node_modules": true, "__pycache__": true, ".venv": true,
		"target": true, "dist": true, "build": true, ".idea": true,
	}
	var walk func(dir, prefix string, depth int)
	walk = func(dir, prefix string, depth int) {
		if depth > 4 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") && name != ".env.example" {
				continue
			}
			if e.IsDir() {
				if skipDirs[name] {
					continue
				}
				sb.WriteString(prefix + name + "/\n")
				walk(filepath.Join(dir, name), prefix+"  ", depth+1)
			} else {
				info, err := e.Info()
				size := ""
				if err == nil {
					b := info.Size()
					switch {
					case b < 1024:
						size = fmt.Sprintf(" (%dB)", b)
					case b < 1024*1024:
						size = fmt.Sprintf(" (%.1fKB)", float64(b)/1024)
					default:
						size = fmt.Sprintf(" (%.1fMB)", float64(b)/1024/1024)
					}
				}
				sb.WriteString(prefix + name + size + "\n")
			}
		}
	}
	walk(c.Workspace, "  ", 0)
	return sb.String()
}

// SemanticSearch uses Gonka /v1/embeddings to find workspace files relevant to query.
// It walks the workspace, embeds each .go/.md/.py file, stores vectors in memory,
// then returns top_k most similar chunks to the query embedding.
func (c *Cfg) SemanticSearch(query string, topK int) Result {
	if topK <= 0 {
		topK = 5
	}
	if c.GonkaBaseURL == "" || c.GonkaAPIKey == "" {
		return fail("GONKA_BASE_URL or GONKA_API_KEY not set")
	}
	model := c.EmbedModel
	if model == "" {
		model = "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"
	}

	// Collect files from workspace.
	var files []string
	_ = filepath.Walk(c.Workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".md" || ext == ".py" || ext == ".ts" || ext == ".js" {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		return fail("no indexable files found in workspace")
	}

	embed := func(text string) ([]float64, error) {
		if v, ok := globalEmbedCache.get(text); ok {
			return v, nil
		}
		body, _ := json.Marshal(map[string]any{
			"model": model,
			"input": text,
		})
		req, err := http.NewRequest("POST", c.GonkaBaseURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.GonkaAPIKey)
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var res struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return nil, err
		}
		if len(res.Data) == 0 {
			return nil, fmt.Errorf("empty embedding response")
		}
		globalEmbedCache.set(text, res.Data[0].Embedding)
		return res.Data[0].Embedding, nil
	}

	dot := func(a, b []float64) float64 {
		var s float64
		for i := range a {
			if i < len(b) {
				s += a[i] * b[i]
			}
		}
		return s
	}

	// Embed query.
	queryVec, err := embed(query)
	if err != nil {
		return fail("embed query: " + err.Error())
	}

	type scored struct {
		path  string
		score float64
	}
	var results []scored

	// Embed each file (first 2000 chars as chunk).
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		chunk := string(data)
		if len(chunk) > 2000 {
			chunk = chunk[:2000]
		}
		vec, err := embed(chunk)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(c.Workspace, f)
		results = append(results, scored{path: rel, score: dot(queryVec, vec)})
	}

	// Sort by score descending.
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].score > results[j-1].score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Semantic search: %q — top %d results:\n\n", query, topK))
	for i, r := range results {
		if i >= topK {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. %s (score: %.4f)\n", i+1, r.path, r.score))
	}
	return ok(sb.String())
}

// ─── Build system detection ───────────────────────────────────────────────────

// BuildInfo describes the detected project type and build/test commands.
type BuildInfo struct {
	Language   string   `json:"language"`
	BuildCmd   string   `json:"build_command"`
	TestCmd    string   `json:"test_command"`
	Manifest   string   `json:"manifest"`      // e.g. "package.json", "go.mod"
	SourceExts []string `json:"source_exts"`   // e.g. [".ts", ".tsx"]
}

// DetectBuildSystem inspects the workspace root and returns project type info.
// Falls back to generic shell if nothing is recognised.
func (c *Cfg) DetectBuildSystem() BuildInfo {
	type marker struct {
		file    string
		lang    string
		build   string
		test    string
		exts    []string
	}
	markers := []marker{
		{"go.mod",           "go",                 "go build ./...",         "go test ./...",         []string{".go"}},
		{"package.json",     "typescript",          "npm run build",          "npm test",              []string{".ts", ".tsx", ".js", ".jsx", ".mts", ".cts"}},
		{"deno.json",        "typescript",          "deno task build",        "deno test",             []string{".ts", ".tsx"}},
		{"Cargo.toml",       "rust",                "cargo build",            "cargo test",            []string{".rs"}},
		{"pyproject.toml",   "python",              "pip install -e .",       "pytest",                []string{".py"}},
		{"setup.py",         "python",              "python setup.py build",  "pytest",                []string{".py"}},
		{"requirements.txt", "python",              "python -m compileall .", "pytest",                []string{".py"}},
		{"pom.xml",          "java",                "mvn compile",            "mvn test",              []string{".java"}},
		{"build.gradle.kts", "kotlin",              "gradle build",           "gradle test",           []string{".kt", ".java"}},
		{"build.gradle",     "java",                "gradle build",           "gradle test",           []string{".java", ".kt"}},
		{"CMakeLists.txt",   "c/cpp",               "cmake --build .",        "ctest",                 []string{".c", ".cpp", ".cc", ".cxx", ".h", ".hpp"}},
		{"Makefile",         "make",                "make",                   "make test",             []string{}},
		{"mix.exs",          "elixir",              "mix compile",            "mix test",              []string{".ex", ".exs"}},
		{"pubspec.yaml",     "dart",                "dart build",             "dart test",             []string{".dart"}},
		{"Package.swift",    "swift",               "swift build",            "swift test",            []string{".swift"}},
		{"composer.json",    "php",                 "composer install",       "phpunit",               []string{".php"}},
		{"Gemfile",          "ruby",                "bundle exec rake build", "bundle exec rspec",     []string{".rb"}},
	}

	for _, m := range markers {
		p := filepath.Join(c.Workspace, m.file)
		if _, err := os.Stat(p); err == nil {
			// For package.json: check if there's a "build" script, otherwise use "npm install"
			if m.file == "package.json" {
				data, err := os.ReadFile(p)
				if err == nil {
					var pkg map[string]json.RawMessage
					if json.Unmarshal(data, &pkg) == nil {
						if scripts, ok := pkg["scripts"]; ok {
							var s map[string]string
							if json.Unmarshal(scripts, &s) == nil {
								if _, hasBuild := s["build"]; !hasBuild {
									m.build = "npm install"
								}
							}
						}
					}
				}
			}
			return BuildInfo{
				Language:   m.lang,
				BuildCmd:   m.build,
				TestCmd:    m.test,
				Manifest:   m.file,
				SourceExts: m.exts,
			}
		}
	}

	return BuildInfo{
		Language:   "unknown",
		BuildCmd:   "",
		TestCmd:    "",
		Manifest:   "",
		SourceExts: []string{},
	}
}

// CountLines counts lines and lists top-level symbol definitions in a file.
func (c *Cfg) CountLines(path string) Result {
	abs, err := c.safePath(path)
	if err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	// Collect symbol definitions (func, type, const, var at column 0).
	symRe := regexp.MustCompile(`^(func|type|const|var)\s+(\w+)`)
	var syms []string
	for i, line := range lines {
		if m := symRe.FindStringSubmatch(line); m != nil {
			syms = append(syms, fmt.Sprintf("  line %d: %s %s", i+1, m[1], m[2]))
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("File: %s\nTotal lines: %d\n\nSymbols:\n", path, total))
	if len(syms) == 0 {
		sb.WriteString("  (none found)\n")
	} else {
		for _, s := range syms {
			sb.WriteString(s + "\n")
		}
	}
	return ok(sb.String())
}
