package profile

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
)

//go:embed seeds/*.json
var seedFS embed.FS

// SeedPatterns holds the baked-in flow patterns for a role.
type SeedPatterns struct {
	Role         string                     `json:"role"`
	Version      string                     `json:"version"`
	Description  string                     `json:"description"`
	Flows        map[string]SeedFlow        `json:"flows"`
	AntiPatterns map[string]string          `json:"anti_patterns"`
}

type SeedFlow struct {
	Trigger []string `json:"trigger"`
	Steps   []string `json:"steps"`
}

// LoadSeeds reads the embedded seed patterns for a role.
func LoadSeeds(role Role) (*SeedPatterns, error) {
	filename := fmt.Sprintf("seeds/%s.json", role)
	data, err := seedFS.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("seeds: no patterns for role %s: %w", role, err)
	}
	var sp SeedPatterns
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, fmt.Errorf("seeds: parse %s: %w", filename, err)
	}
	slog.Info("profile: loaded seed patterns", "role", role, "flows", len(sp.Flows), "anti_patterns", len(sp.AntiPatterns))
	return &sp, nil
}

// SeedPromptAugmentation formats seed patterns as system prompt rules.
func SeedPromptAugmentation(role Role) string {
	sp, err := LoadSeeds(role)
	if err != nil {
		return ""
	}

	var out string
	out += "\n## Baked-in Flow Patterns (" + sp.Description + ")\n"
	out += "These flows are STANDARD. Execute them natively without re-deriving.\n\n"

	for name, flow := range sp.Flows {
		out += "### " + name + "\n"
		out += "Triggers: " + fmt.Sprintf("%v", flow.Trigger) + "\n"
		for i, step := range flow.Steps {
			out += fmt.Sprintf("%d. %s\n", i+1, step)
		}
		out += "\n"
	}

	if len(sp.AntiPatterns) > 0 {
		out += "## Anti-patterns (NEVER do these)\n"
		for name, desc := range sp.AntiPatterns {
			out += "- **" + name + "**: " + desc + "\n"
		}
	}

	return out
}
