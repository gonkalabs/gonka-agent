// Package updater implements self-update for the gonka binary.
// It checks GitHub Releases for newer versions, downloads the
// matching platform binary, verifies checksum, and replaces itself.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	ghAPI      = "https://api.github.com"
	repoOwner  = "gonkalabs"
	repoName   = "gonka-agent"
	assetFmt   = "gonka-%s-%s"
)

// Release represents a GitHub release.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Check queries GitHub for the latest release. Returns nil if already up-to-date.
func Check(ctx context.Context, currentVersion string) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", ghAPI, repoOwner, repoName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil // no releases yet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("updater: GitHub API HTTP %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("updater: decode: %w", err)
	}

	if rel.TagName == currentVersion || rel.TagName == "v"+currentVersion {
		return nil, nil
	}

	return &rel, nil
}

// Download fetches the appropriate binary for this platform from a release.
func Download(ctx context.Context, rel *Release, destPath string) error {
	assetName := fmt.Sprintf(assetFmt, runtime.GOOS, runtime.GOARCH)

	var downloadURL string
	var checksumURL string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
		}
		if a.Name == assetName+".sha256" {
			checksumURL = a.BrowserDownloadURL
		}
	}

	if downloadURL == "" {
		return fmt.Errorf("updater: no asset for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, rel.TagName)
	}

	slog.Info("updater: downloading", "url", downloadURL, "tag", rel.TagName)

	client := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("updater: download: %w", err)
	}
	defer resp.Body.Close()

	tmpPath := destPath + ".update.tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("updater: create tmp: %w", err)
	}

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("updater: write: %w", err)
	}

	slog.Info("updater: downloaded", "bytes", written)

	// Verify checksum if available
	if checksumURL != "" {
		expected, err := fetchChecksum(ctx, checksumURL)
		if err != nil {
			slog.Warn("updater: checksum fetch failed, skipping verification", "err", err)
		} else {
			actual := hex.EncodeToString(h.Sum(nil))
			if actual != expected {
				os.Remove(tmpPath)
				return fmt.Errorf("updater: checksum mismatch: expected %s, got %s", expected, actual)
			}
			slog.Info("updater: checksum verified")
		}
	}

	return os.Rename(tmpPath, destPath)
}

// Apply replaces the currently running binary with the downloaded one.
func Apply(downloadedPath string) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("updater: resolve self: %w", err)
	}

	// Make the new binary executable
	if err := os.Chmod(downloadedPath, 0755); err != nil {
		return fmt.Errorf("updater: chmod: %w", err)
	}

	// Atomic replace: rename old, rename new, remove old
	backupPath := selfPath + ".bak"
	os.Remove(backupPath) // clean up any previous backup

	if err := os.Rename(selfPath, backupPath); err != nil {
		return fmt.Errorf("updater: backup current binary: %w", err)
	}

	if err := os.Rename(downloadedPath, selfPath); err != nil {
		os.Rename(backupPath, selfPath) // rollback
		return fmt.Errorf("updater: replace binary: %w", err)
	}

	os.Remove(backupPath)
	slog.Info("updater: binary updated", "path", selfPath)
	return nil
}

func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Format: "sha256hash  filename\n" or just the hash
	parts := strings.Fields(strings.TrimSpace(string(data)))
	if len(parts) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	return parts[0], nil
}
