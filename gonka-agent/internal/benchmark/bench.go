// Package benchmark provides `gonka bench` — a reproducible benchmark
// runner that executes N iterations of a reference task and measures
// tokens, time, latency, slot hits, and cache hits.
package benchmark

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// Task defines a reference benchmark task.
type Task struct {
	Name        string
	Description string
	Input       string
	Validate    func(output string) bool // returns true if output is correct
}

// Result captures one benchmark iteration.
type Result struct {
	Task       string
	Iteration  int
	Success    bool
	Tokens     int
	LatencyMs  int64
	SlotHits   int
	CacheHits  int
	ToolCalls  int
	Error      string
}

// Suite holds the configuration for a benchmark run.
type Suite struct {
	Tasks      []Task
	Iterations int
	RunFn      func(ctx context.Context, task Task) Result
}

// DefaultTasks returns the built-in reference tasks.
func DefaultTasks() []Task {
	return []Task{
		{
			Name:        "hello-go",
			Description: "Create a Hello World Go program",
			Input:       "Create a file hello.go that prints 'Hello, Gonka!' and compiles with go build",
			Validate:    func(out string) bool { return strings.Contains(out, "Hello") },
		},
		{
			Name:        "fix-syntax",
			Description: "Fix a Go syntax error",
			Input:       "The file broken.go has a missing closing brace. Find and fix it.",
			Validate:    func(out string) bool { return strings.Contains(strings.ToLower(out), "fix") },
		},
		{
			Name:        "web-search",
			Description: "Search the web for current Go version",
			Input:       "What is the latest stable Go version? Search the web to find out.",
			Validate:    func(out string) bool { return strings.Contains(out, "go") || strings.Contains(out, "Go") },
		},
	}
}

// Run executes the full benchmark suite.
func Run(ctx context.Context, s Suite) []Result {
	var results []Result

	for _, task := range s.Tasks {
		for i := 1; i <= s.Iterations; i++ {
			t0 := time.Now()
			r := s.RunFn(ctx, task)
			r.Task = task.Name
			r.Iteration = i
			if r.LatencyMs == 0 {
				r.LatencyMs = time.Since(t0).Milliseconds()
			}
			results = append(results, r)
		}
	}

	return results
}

// Report generates a markdown benchmark report.
func Report(results []Result) string {
	var sb strings.Builder
	sb.WriteString("# Gonka Benchmark Report\n\n")
	sb.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))

	// Group by task
	grouped := map[string][]Result{}
	for _, r := range results {
		grouped[r.Task] = append(grouped[r.Task], r)
	}

	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Task | Iters | Success% | Avg Latency | Avg Tokens | Avg Tools |\n")
	sb.WriteString("|------|-------|----------|-------------|------------|----------|\n")

	for task, runs := range grouped {
		var successes, totalTokens, totalTools int
		var totalLatency int64
		for _, r := range runs {
			if r.Success {
				successes++
			}
			totalTokens += r.Tokens
			totalLatency += r.LatencyMs
			totalTools += r.ToolCalls
		}
		n := len(runs)
		pct := float64(successes) / float64(n) * 100
		avgLat := time.Duration(totalLatency/int64(n)) * time.Millisecond
		avgTok := totalTokens / n
		avgTool := totalTools / n

		sb.WriteString(fmt.Sprintf("| %s | %d | %.0f%% | %s | %d | %d |\n",
			task, n, pct, avgLat.Round(time.Millisecond), avgTok, avgTool))
	}

	// Detailed results
	sb.WriteString("\n## Detailed Results\n\n")
	sb.WriteString("| Task | Iter | OK | Latency | Tokens | Tools | Slots | Cache | Error |\n")
	sb.WriteString("|------|------|----|---------|--------|-------|-------|-------|-------|\n")

	for _, r := range results {
		ok := "✓"
		if !r.Success {
			ok = "✗"
		}
		errStr := ""
		if r.Error != "" {
			errStr = truncate(r.Error, 40)
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %s | %dms | %d | %d | %d | %d | %s |\n",
			r.Task, r.Iteration, ok, r.LatencyMs, r.Tokens, r.ToolCalls,
			r.SlotHits, r.CacheHits, errStr))
	}

	// Stats
	sb.WriteString("\n## Aggregate Statistics\n\n")
	var totalSuccess int
	var allLatencies []float64
	for _, r := range results {
		if r.Success {
			totalSuccess++
		}
		allLatencies = append(allLatencies, float64(r.LatencyMs))
	}

	if len(allLatencies) > 0 {
		mean, stddev := meanStddev(allLatencies)
		sb.WriteString(fmt.Sprintf("- Total iterations: %d\n", len(results)))
		sb.WriteString(fmt.Sprintf("- Success rate: %.1f%%\n", float64(totalSuccess)/float64(len(results))*100))
		sb.WriteString(fmt.Sprintf("- Latency mean: %.0fms, stddev: %.0fms\n", mean, stddev))
		sb.WriteString(fmt.Sprintf("- P50: %.0fms, P95: %.0fms\n", percentile(allLatencies, 0.5), percentile(allLatencies, 0.95)))
	}

	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func meanStddev(vals []float64) (float64, float64) {
	n := float64(len(vals))
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	mean := sum / n
	variance := 0.0
	for _, v := range vals {
		diff := v - mean
		variance += diff * diff
	}
	return mean, math.Sqrt(variance / n)
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	// Simple insertion sort (small N)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}
