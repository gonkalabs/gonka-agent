// Package rolechain — file-based context cache for ContextSnapshot.
// Like Anthropic's prompt caching, this avoids re-reading and re-analyzing
// workspace files that haven't changed since the last run.
//
// Cache is stored in <workspace>/.gonka-cache/context.json.
// It is invalidated when any tracked file's mtime changes or after 30 minutes.
package rolechain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/roles"
	"github.com/gonkalabs/gonka-agent/internal/tools"
)

const (
	cacheDirName = ".gonka-cache"
	cacheFileName = "context.json"
	cacheTTL      = 30 * time.Minute
)

type cacheEntry struct {
	Snapshot   roles.ContextSnapshot `json:"snapshot"`
	BuildInfo  tools.BuildInfo       `json:"build_info"`
	FileMtimes map[string]int64      `json:"file_mtimes"` // rel path → unix mtime
	CreatedAt  time.Time             `json:"created_at"`
}

func cachePath(workspace string) string {
	return filepath.Join(workspace, cacheDirName, cacheFileName)
}

// loadCache returns (snapshot, buildInfo, true) if the cache is valid
// (all tracked files unchanged, TTL not expired), otherwise (_, _, false).
func loadCache(workspace string, currentBuild tools.BuildInfo) (roles.ContextSnapshot, tools.BuildInfo, bool) {
	data, err := os.ReadFile(cachePath(workspace))
	if err != nil {
		return roles.ContextSnapshot{}, tools.BuildInfo{}, false
	}

	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return roles.ContextSnapshot{}, tools.BuildInfo{}, false
	}

	// TTL check.
	if time.Since(entry.CreatedAt) > cacheTTL {
		return roles.ContextSnapshot{}, tools.BuildInfo{}, false
	}

	// Build system changed → stale.
	if entry.BuildInfo.Language != currentBuild.Language ||
		entry.BuildInfo.Manifest != currentBuild.Manifest {
		return roles.ContextSnapshot{}, tools.BuildInfo{}, false
	}

	// Verify each tracked file's mtime.
	for relPath, cachedMtime := range entry.FileMtimes {
		abs := filepath.Join(workspace, relPath)
		info, err := os.Stat(abs)
		if err != nil || info.ModTime().Unix() != cachedMtime {
			return roles.ContextSnapshot{}, tools.BuildInfo{}, false
		}
	}

	return entry.Snapshot, entry.BuildInfo, true
}

// saveCache persists the ContextSnapshot to disk, tracking file mtimes
// for all files listed in RelevantFiles + the project manifest.
func saveCache(workspace string, snap roles.ContextSnapshot, buildInfo tools.BuildInfo) {
	mtimes := map[string]int64{}
	for _, relPath := range snap.RelevantFiles {
		abs := filepath.Join(workspace, relPath)
		if info, err := os.Stat(abs); err == nil {
			mtimes[relPath] = info.ModTime().Unix()
		}
	}
	if buildInfo.Manifest != "" {
		abs := filepath.Join(workspace, buildInfo.Manifest)
		if info, err := os.Stat(abs); err == nil {
			mtimes[buildInfo.Manifest] = info.ModTime().Unix()
		}
	}

	entry := cacheEntry{
		Snapshot:   snap,
		BuildInfo:  buildInfo,
		FileMtimes: mtimes,
		CreatedAt:  time.Now(),
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	cDir := filepath.Join(workspace, cacheDirName)
	_ = os.MkdirAll(cDir, 0o755)
	_ = os.WriteFile(cachePath(workspace), data, 0o644)
}

// InvalidateCache deletes the context cache for the given workspace.
func InvalidateCache(workspace string) {
	_ = os.Remove(cachePath(workspace))
}
