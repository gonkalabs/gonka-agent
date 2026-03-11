package profile

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Bootstrap ensures all required services for the role are running.
// Returns a list of services that failed to start.
func Bootstrap(ctx context.Context, role Role) []string {
	specs := RequiredServices(role)
	var failed []string

	for _, spec := range specs {
		if isServiceUp(ctx, spec.CheckURL) {
			slog.Info("bootstrap: service already running", "name", spec.Name)
			continue
		}

		slog.Info("bootstrap: starting service", "name", spec.Name, "image", spec.DockerImg)
		if err := startDocker(ctx, spec); err != nil {
			slog.Warn("bootstrap: failed to start", "name", spec.Name, "err", err)
			failed = append(failed, spec.Name)
			continue
		}

		if err := waitForService(ctx, spec.CheckURL, 20*time.Second); err != nil {
			slog.Warn("bootstrap: service not responsive", "name", spec.Name, "err", err)
			failed = append(failed, spec.Name)
		}
	}

	return failed
}

func isServiceUp(ctx context.Context, url string) bool {
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

func startDocker(ctx context.Context, spec ServiceSpec) error {
	args := append([]string{"run", "-d", "--restart", "unless-stopped"}, spec.DockerRun...)
	args = append(args, spec.DockerImg)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(out)
		if strings.Contains(outStr, "already in use") {
			containerName := ""
			for i, a := range spec.DockerRun {
				if a == "--name" && i+1 < len(spec.DockerRun) {
					containerName = spec.DockerRun[i+1]
					break
				}
			}
			if containerName != "" {
				exec.CommandContext(ctx, "docker", "start", containerName).Run()
				return nil
			}
		}
		return fmt.Errorf("%s: %w", outStr, err)
	}
	return nil
}

func waitForService(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isServiceUp(ctx, url) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("service at %s not ready after %s", url, timeout)
}
