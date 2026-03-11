package healthmon

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Check represents one diagnostic check result.
type Check struct {
	Name   string
	Status string // "ok", "warn", "fail"
	Detail string
}

// Doctor runs a suite of self-diagnostic checks for `gonka doctor`.
func Doctor(ctx context.Context) []Check {
	var checks []Check

	checks = append(checks, checkRuntime())
	checks = append(checks, checkDocker(ctx))
	checks = append(checks, checkEndpoint(ctx, "OpenRouter", "https://openrouter.ai/api/v1/models"))
	checks = append(checks, checkEndpoint(ctx, "Ollama", "http://localhost:11434/api/tags"))
	checks = append(checks, checkEndpoint(ctx, "Gonka Proxy", "http://localhost:8090/v1/models"))
	checks = append(checks, checkEndpoint(ctx, "SearXNG", "http://localhost:8888/search?q=test&format=json"))
	checks = append(checks, checkGit(ctx))
	checks = append(checks, checkChrome(ctx))

	return checks
}

func checkRuntime() Check {
	detail := fmt.Sprintf("go%s %s/%s, goroutines=%d",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumGoroutine())
	return Check{Name: "runtime", Status: "ok", Detail: detail}
}

func checkDocker(ctx context.Context) Check {
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		return Check{Name: "docker", Status: "fail", Detail: "docker not available: " + err.Error()}
	}
	return Check{Name: "docker", Status: "ok", Detail: "v" + strings.TrimSpace(string(out))}
}

func checkEndpoint(ctx context.Context, name, url string) Check {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Check{Name: name, Status: "warn", Detail: "unreachable: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Check{Name: name, Status: "warn", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return Check{Name: name, Status: "ok", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func checkGit(ctx context.Context) Check {
	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "git", "--version").CombinedOutput()
	if err != nil {
		return Check{Name: "git", Status: "fail", Detail: err.Error()}
	}
	return Check{Name: "git", Status: "ok", Detail: strings.TrimSpace(string(out))}
}

func checkChrome(ctx context.Context) Check {
	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, bin := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		out, err := exec.CommandContext(ctx2, bin, "--version").CombinedOutput()
		if err == nil {
			return Check{Name: "chrome", Status: "ok", Detail: strings.TrimSpace(string(out))}
		}
	}
	return Check{Name: "chrome", Status: "warn", Detail: "no chromium/chrome found (needed for web_fetch)"}
}
