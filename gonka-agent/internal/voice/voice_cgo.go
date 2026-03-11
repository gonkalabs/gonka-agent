// CGO implementation that uses whisper.cpp via Go bindings.
// Only compiled when: go build -tags voice (requires whisper.cpp headers)
//
// Dependencies:
//   apt install libwhisper-dev  (or build whisper.cpp from source)
//   CGO_ENABLED=1

//go:build voice

package voice

import (
	"fmt"
	"os/exec"
	"strings"
)

var voiceCompiled = true

// transcribeImpl shells out to the whisper.cpp `main` binary.
// A pure CGO binding is preferred but this works as a portable fallback.
func transcribeImpl(modelPath, wavPath string) (string, error) {
	// Try the whisper CLI binary first (easiest portable option)
	for _, bin := range []string{"whisper-cpp", "whisper", "main"} {
		if path, err := exec.LookPath(bin); err == nil {
			out, err := exec.Command(path,
				"-m", modelPath,
				"-f", wavPath,
				"--no-timestamps",
				"--language", "auto",
			).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("voice: whisper exec: %w\n%s", err, string(out))
			}
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("voice: whisper binary not found in PATH (install whisper.cpp)")
}
