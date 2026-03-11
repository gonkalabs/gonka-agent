// Stub implementation when built without CGO/whisper.
// This file is always compiled; the voice_cgo.go file overrides
// these symbols when the "voice" build tag is active.

//go:build !voice

package voice

import "fmt"

var voiceCompiled = false

func transcribeImpl(modelPath, wavPath string) (string, error) {
	return "", fmt.Errorf("voice: not compiled (build with -tags voice and CGO_ENABLED=1)")
}
