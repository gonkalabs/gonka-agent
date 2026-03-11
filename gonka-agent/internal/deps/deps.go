// Package deps manages external repository dependencies required for
// the full slot-flow pipeline. `gonka deps` auto-pulls and builds
// dependencies like opengnk and gonka-main.
package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Dependency defines an external repo that gonka needs.
type Dependency struct {
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Build   string `json:"build"` // build command (e.g. "go build ./cmd/...")
	Version string `json:"version"`
}

// DefaultDeps returns the standard dependency list.
func DefaultDeps() []Dependency {
	return []Dependency{
		{
			Name:   "opengnk",
			Repo:   "https://github.com/gonkalabs/opengnk.git",
			Branch: "main",
			Build:  "go build -o opengnk ./cmd/opengnk",
		},
		{
			Name:   "gonka-main",
			Repo:   "https://github.com/AizelNetwork/gonka-main.git",
			Branch: "main",
			Build:  "",
		},
	}
}

// LockFile records resolved versions.
type LockFile struct {
	Deps    map[string]string `json:"deps"`    // name -> commit hash
	Updated time.Time         `json:"updated"`
}

// Manager handles dependency resolution and pulling.
type Manager struct {
	baseDir  string // ~/.gonka-cache/deps/
	lockPath string
}

// NewManager creates a dependency manager.
func NewManager(cacheDir string) *Manager {
	baseDir := filepath.Join(cacheDir, "deps")
	os.MkdirAll(baseDir, 0755)
	return &Manager{
		baseDir:  baseDir,
		lockPath: filepath.Join(baseDir, "deps.lock.json"),
	}
}

// Resolve clones or updates all dependencies.
func (m *Manager) Resolve(ctx context.Context, deps []Dependency) error {
	lock := m.readLock()

	for _, dep := range deps {
		depDir := filepath.Join(m.baseDir, dep.Name)

		if _, err := os.Stat(filepath.Join(depDir, ".git")); err == nil {
			slog.Info("deps: updating", "name", dep.Name)
			cmd := exec.CommandContext(ctx, "git", "pull", "--ff-only")
			cmd.Dir = depDir
			if out, err := cmd.CombinedOutput(); err != nil {
				slog.Warn("deps: pull failed", "name", dep.Name, "err", err, "out", string(out))
			}
		} else {
			slog.Info("deps: cloning", "name", dep.Name, "repo", dep.Repo)
			args := []string{"clone", "--depth", "1"}
			if dep.Branch != "" {
				args = append(args, "-b", dep.Branch)
			}
			args = append(args, dep.Repo, depDir)
			cmd := exec.CommandContext(ctx, "git", args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("deps: clone %s: %s: %w", dep.Name, string(out), err)
			}
		}

		// Record commit
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmd.Dir = depDir
		out, err := cmd.Output()
		if err == nil {
			lock.Deps[dep.Name] = strings.TrimSpace(string(out))
		}

		// Build if specified
		if dep.Build != "" {
			slog.Info("deps: building", "name", dep.Name, "cmd", dep.Build)
			parts := strings.Fields(dep.Build)
			buildCmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
			buildCmd.Dir = depDir
			if out, err := buildCmd.CombinedOutput(); err != nil {
				slog.Warn("deps: build failed", "name", dep.Name, "err", err, "out", string(out))
			} else {
				slog.Info("deps: build ok", "name", dep.Name)
			}
		}
	}

	lock.Updated = time.Now()
	return m.writeLock(lock)
}

// Path returns the local path of a dependency.
func (m *Manager) Path(name string) string {
	return filepath.Join(m.baseDir, name)
}

func (m *Manager) readLock() *LockFile {
	lf := &LockFile{Deps: make(map[string]string)}
	data, err := os.ReadFile(m.lockPath)
	if err != nil {
		return lf
	}
	json.Unmarshal(data, lf)
	if lf.Deps == nil {
		lf.Deps = make(map[string]string)
	}
	return lf
}

func (m *Manager) writeLock(lf *LockFile) error {
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.lockPath, data, 0644)
}
