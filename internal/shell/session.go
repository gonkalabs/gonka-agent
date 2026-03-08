// Package shell provides a persistent bash session where env vars and
// cwd persist between commands (unlike exec.Command which spawns fresh shells).
package shell

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const endMarker = "___GONKA_END_"

// Session is a long-lived bash process. Commands are sent via stdin,
// output is read until a sentinel marker, so state persists between calls.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	cwd    string
	env    map[string]string
}

// New starts a bash session rooted at workspacePath.
func New(workspacePath string) (*Session, error) {
	cmd := exec.Command("/bin/bash", "--norc", "--noprofile", "-s")
	cmd.Dir = workspacePath

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bash start: %w", err)
	}

	s := &Session{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		cwd:    workspacePath,
		env:    map[string]string{},
	}
	// Increase scanner buffer for large outputs.
	s.stdout.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	// Init: disable prompt, set workspace.
	_, _, _ = s.run("cd " + shellEscape(workspacePath) + " && pwd", 10*time.Second)
	return s, nil
}

// Run executes cmd in the persistent session and returns stdout+stderr.
// The cwd and env vars persist from previous commands.
func (s *Session) Run(cmd string, timeout time.Duration) (output string, exitCode int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run(cmd, timeout)
}

func (s *Session) run(cmd string, timeout time.Duration) (string, int, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	markerID := fmt.Sprintf("%d", time.Now().UnixNano())
	endLine := endMarker + markerID

	// Write: command + exit code capture + sentinel.
	_, err := fmt.Fprintf(s.stdin,
		"%s\necho '%s'$?\n", cmd, endLine)
	if err != nil {
		return "", -1, fmt.Errorf("write to bash: %w", err)
	}

	// Read until sentinel line.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var sb strings.Builder
	exitCode := 0
	done := make(chan struct{})

	go func() {
		defer close(done)
		for s.stdout.Scan() {
			line := s.stdout.Text()
			if strings.HasPrefix(line, endLine) {
				// Extract exit code from suffix.
				suffix := strings.TrimPrefix(line, endLine)
				fmt.Sscanf(suffix, "%d", &exitCode)
				return
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}()

	select {
	case <-done:
		return strings.TrimRight(sb.String(), "\n"), exitCode, nil
	case <-ctx.Done():
		return sb.String(), -1, fmt.Errorf("command timed out after %s", timeout)
	}
}

// SetEnv sets an environment variable in the session.
func (s *Session) SetEnv(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.env[key] = val
	_, _, _ = s.run(fmt.Sprintf("export %s=%s", key, shellEscape(val)), 5*time.Second)
}

// Cwd returns the current working directory.
func (s *Session) Cwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, _, _ := s.run("pwd", 5*time.Second)
	return strings.TrimSpace(out)
}

// DumpState returns current session state: cwd, env vars, background processes.
// This is injected into agent context for reinforcement.
func (s *Session) DumpState() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	cwd, _, _ := s.run("pwd", 5*time.Second)
	envOut, _, _ := s.run("env | head -20", 5*time.Second)
	jobs, _, _ := s.run("jobs 2>/dev/null || true", 5*time.Second)

	var sb strings.Builder
	sb.WriteString("=== BASH SESSION STATE ===\n")
	sb.WriteString("cwd: " + strings.TrimSpace(cwd) + "\n")
	if jobs != "" {
		sb.WriteString("background jobs:\n" + jobs + "\n")
	}
	sb.WriteString("env (first 20):\n" + envOut + "\n")
	return sb.String()
}

// Close terminates the bash session.
func (s *Session) Close() {
	_, _ = fmt.Fprintln(s.stdin, "exit 0")
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
