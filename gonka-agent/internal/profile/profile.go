// Package profile manages role-aware bootstrapping. Each role (developer,
// researcher, bot) gets tailored seed patterns, skill packs, service
// requirements, and system prompt augmentations.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Role identifies the agent's operating mode.
type Role string

const (
	RoleDeveloper  Role = "developer"
	RoleResearcher Role = "researcher"
	RoleBot        Role = "bot"
)

// Profile holds the user's configuration from `gonka init`.
type Profile struct {
	Role   Role   `json:"role"`
	UIMode string `json:"ui"` // "1"=TUI, "2"=n8n, "3"=CLI
}

// ServiceSpec defines an external service that must be running.
type ServiceSpec struct {
	Name      string // human-readable name
	CheckURL  string // HTTP endpoint to probe
	DockerImg string // Docker image to auto-start
	DockerRun []string // extra docker run args
}

// RequiredServices returns the services needed for a given role.
func RequiredServices(role Role) []ServiceSpec {
	base := []ServiceSpec{
		{
			Name:      "SearXNG",
			CheckURL:  "http://localhost:8888/search?q=test&format=json",
			DockerImg: "searxng/searxng:latest",
			DockerRun: []string{"-p", "8888:8080", "--name", "gonka-searxng"},
		},
	}

	switch role {
	case RoleDeveloper:
		return base
	case RoleResearcher:
		return base
	case RoleBot:
		return append(base, ServiceSpec{
			Name:      "n8n",
			CheckURL:  "http://localhost:5678/healthz",
			DockerImg: "n8nio/n8n:latest",
			DockerRun: []string{"-p", "5678:5678", "--name", "gonka-n8n"},
		})
	}
	return base
}

// PromptPreamble returns role-specific system prompt augmentations.
func PromptPreamble(role Role) string {
	switch role {
	case RoleDeveloper:
		return `You are an expert software developer. You write production-quality code.
You think carefully about edge cases, performance, and maintainability.
You prefer working implementations over theoretical discussions.
When uncertain, you investigate the codebase before making changes.`

	case RoleResearcher:
		return `You are a research assistant with deep analytical skills.
You approach problems scientifically: form hypotheses, gather evidence, verify.
You document findings thoroughly with data and metrics.
You prioritize reproducibility and statistical rigor.`

	case RoleBot:
		return `You are an automated agent optimized for throughput and reliability.
You complete tasks efficiently with minimal interaction.
You handle errors gracefully and retry when appropriate.
You report structured outcomes (success/failure/metrics) for pipeline consumption.`
	}
	return ""
}

// SkillPacks returns the skill pack names to load for a role.
func SkillPacks(role Role) []string {
	switch role {
	case RoleDeveloper:
		return []string{"git_ops", "dev_ops", "data_ops"}
	case RoleResearcher:
		return []string{"git_ops", "data_ops"}
	case RoleBot:
		return []string{"git_ops", "dev_ops"}
	}
	return []string{"git_ops"}
}

// Load reads the profile from ~/.gonka-cache/profile.json.
func Load(workspace string) (*Profile, error) {
	path := filepath.Join(workspace, ".gonka-cache", "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Profile{Role: RoleDeveloper, UIMode: "3"}, nil
		}
		return nil, fmt.Errorf("profile: read: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("profile: parse: %w", err)
	}
	if p.Role == "" {
		p.Role = RoleDeveloper
	}
	return &p, nil
}

// Save writes the profile to disk.
func Save(workspace string, p *Profile) error {
	dir := filepath.Join(workspace, ".gonka-cache")
	os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "profile.json"), data, 0644)
}
