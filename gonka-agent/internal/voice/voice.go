// Package voice provides speech-to-text via whisper.cpp bindings.
// The tiny multilingual model (~39MB) is downloaded on first use and
// cached in ~/.gonka-cache/models/. Voice input is activated via
// `gonka --voice` or a TUI hotkey.
//
// Build with: go build -tags voice (requires CGO + whisper.cpp headers)
// Without the voice tag, this package provides stub implementations
// that return a clear "voice not compiled" error.
package voice

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const (
	modelName = "ggml-tiny.bin"
	modelURL  = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-tiny.bin"
	modelSize = 39_000_000 // ~39MB
)

// Engine manages the whisper model lifecycle.
type Engine struct {
	modelPath string
	cacheDir  string
	ready     bool
}

// NewEngine creates a voice engine. cacheDir is typically ~/.gonka-cache.
func NewEngine(cacheDir string) *Engine {
	modelDir := filepath.Join(cacheDir, "models")
	os.MkdirAll(modelDir, 0755)
	return &Engine{
		modelPath: filepath.Join(modelDir, modelName),
		cacheDir:  cacheDir,
	}
}

// EnsureModel downloads the tiny multilingual model if not cached.
func (e *Engine) EnsureModel() error {
	if info, err := os.Stat(e.modelPath); err == nil && info.Size() > modelSize/2 {
		e.ready = true
		return nil
	}

	slog.Info("voice: downloading whisper model", "url", modelURL, "target", e.modelPath)
	resp, err := http.Get(modelURL)
	if err != nil {
		return fmt.Errorf("voice: download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("voice: download HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(e.modelPath + ".tmp")
	if err != nil {
		return fmt.Errorf("voice: create temp: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(e.modelPath + ".tmp")
		return fmt.Errorf("voice: write: %w", err)
	}

	if err := os.Rename(e.modelPath+".tmp", e.modelPath); err != nil {
		return fmt.Errorf("voice: rename: %w", err)
	}

	slog.Info("voice: model downloaded", "size", written)
	e.ready = true
	return nil
}

// Transcribe converts a WAV file to text. Requires CGO build with voice tag.
func (e *Engine) Transcribe(wavPath string) (string, error) {
	if !e.ready {
		if err := e.EnsureModel(); err != nil {
			return "", err
		}
	}
	return transcribeImpl(e.modelPath, wavPath)
}

// IsAvailable reports whether voice support was compiled in.
func IsAvailable() bool {
	return voiceCompiled
}

// RuntimeInfo returns build/platform info for diagnostics.
func RuntimeInfo() string {
	compiled := "no"
	if voiceCompiled {
		compiled = "yes"
	}
	return fmt.Sprintf("voice=%s os=%s arch=%s", compiled, runtime.GOOS, runtime.GOARCH)
}
