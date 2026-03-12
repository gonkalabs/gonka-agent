// Package n8n provides automated bootstrap and management of n8n
// as a visual UI mode. It handles Docker lifecycle, pre-baked workflow
// import, and an HTTP bridge between the agent and n8n webhooks.
package n8n

import (
	"context"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed workflows/*.json
var workflowFS embed.FS

const (
	containerName = "gonka-n8n"
	n8nPort       = "5678"
)

// Manager handles the n8n lifecycle.
type Manager struct {
	dataDir string // persistent n8n data directory
	baseURL string
	client  *http.Client
}

// NewManager creates an n8n manager. dataDir is where n8n stores its SQLite DB.
func NewManager(cacheDir string) *Manager {
	dataDir := filepath.Join(cacheDir, "n8n-data")
	os.MkdirAll(dataDir, 0755)
	return &Manager{
		dataDir: dataDir,
		baseURL: "http://localhost:" + n8nPort,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Start ensures n8n is running in Docker.
func (m *Manager) Start(ctx context.Context) error {
	if m.isRunning(ctx) {
		slog.Info("n8n: already running")
		return nil
	}

	slog.Info("n8n: starting Docker container")
	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", containerName,
		"-p", n8nPort+":5678",
		"-v", m.dataDir+":/home/node/.n8n",
		"-e", "N8N_BASIC_AUTH_ACTIVE=false",
		"-e", "N8N_SECURE_COOKIE=false",
		"-e", "N8N_PROTOCOL=http",
		"-e", "N8N_PERSONALIZATION_ENABLED=false",
		"-e", "N8N_DIAGNOSTICS_ENABLED=false",
		"-e", "N8N_VERSION_NOTIFICATIONS_ENABLED=false",
		"-e", "GENERIC_TIMEZONE=UTC",
		"--restart", "unless-stopped",
		"n8nio/n8n:1.72.1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already in use") {
			exec.CommandContext(ctx, "docker", "start", containerName).Run()
		} else {
			return fmt.Errorf("n8n: docker start: %s: %w", string(out), err)
		}
	}

	if err := m.waitReady(ctx, 30*time.Second); err != nil {
		return err
	}

	return m.importWorkflows(ctx)
}

// Stop halts the n8n container.
func (m *Manager) Stop(ctx context.Context) error {
	return exec.CommandContext(ctx, "docker", "stop", containerName).Run()
}

// URL returns the n8n web interface URL.
func (m *Manager) URL() string {
	return m.baseURL
}

func (m *Manager) isRunning(ctx context.Context) bool {
	resp, err := m.client.Get(m.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.isRunning(ctx) {
			slog.Info("n8n: ready", "url", m.baseURL)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("n8n: not ready after %s", timeout)
}

func (m *Manager) importWorkflows(ctx context.Context) error {
	entries, err := workflowFS.ReadDir("workflows")
	if err != nil {
		return fmt.Errorf("n8n: read embedded workflows: %w", err)
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := workflowFS.ReadFile("workflows/" + e.Name())
		if err != nil {
			slog.Warn("n8n: failed to read workflow", "file", e.Name(), "err", err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			m.baseURL+"/api/v1/workflows",
			strings.NewReader(string(data)))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := m.client.Do(req)
		if err != nil {
			slog.Warn("n8n: workflow import failed", "file", e.Name(), "err", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		slog.Info("n8n: imported workflow", "file", e.Name(), "status", resp.StatusCode)
	}
	return nil
}

// SendWebhook posts data to an n8n webhook endpoint.
func (m *Manager) SendWebhook(ctx context.Context, webhookPath string, payload string) error {
	url := m.baseURL + "/webhook/" + webhookPath
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("n8n webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}
