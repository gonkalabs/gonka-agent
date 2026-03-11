// Package skills provides pluggable skill packs that augment the agent's
// tool set and system prompt based on the active role. Each pack is
// defined via a YAML manifest embedded at build time.
package skills

import (
	"embed"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

// SkillPack defines a loadable pack of tools and prompt augmentations.
type SkillPack struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Roles       []string `yaml:"roles"`
	Tools       []Tool   `yaml:"tools"`
	PromptRules []string `yaml:"prompt_rules"`
}

// Tool defines one executable tool within a skill pack.
type Tool struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Parameters  map[string]Param  `yaml:"parameters"`
	Command     string            `yaml:"command"` // shell command template
}

type Param struct {
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// Registry holds all loaded skill packs indexed by name.
type Registry struct {
	mu    sync.RWMutex
	packs map[string]*SkillPack
}

var globalRegistry = &Registry{packs: make(map[string]*SkillPack)}

// Load reads all embedded YAML manifests and registers them.
// Safe to call multiple times.
func Load() error {
	entries, err := manifestFS.ReadDir("manifests")
	if err != nil {
		return fmt.Errorf("skills: read manifests: %w", err)
	}

	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()

	for _, e := range entries {
		data, err := manifestFS.ReadFile("manifests/" + e.Name())
		if err != nil {
			return fmt.Errorf("skills: read %s: %w", e.Name(), err)
		}
		var pack SkillPack
		if err := yaml.Unmarshal(data, &pack); err != nil {
			return fmt.Errorf("skills: parse %s: %w", e.Name(), err)
		}
		globalRegistry.packs[pack.Name] = &pack
	}
	return nil
}

// ForRole returns all skill packs applicable to the given role.
func ForRole(role string) []*SkillPack {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	var result []*SkillPack
	for _, p := range globalRegistry.packs {
		for _, r := range p.Roles {
			if r == role || r == "*" {
				result = append(result, p)
				break
			}
		}
	}
	return result
}

// Get returns a specific skill pack by name.
func Get(name string) (*SkillPack, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	p, ok := globalRegistry.packs[name]
	return p, ok
}

// All returns every registered skill pack.
func All() []*SkillPack {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	result := make([]*SkillPack, 0, len(globalRegistry.packs))
	for _, p := range globalRegistry.packs {
		result = append(result, p)
	}
	return result
}

// PromptAugmentation returns combined prompt rules from all packs for a role.
func PromptAugmentation(role string) string {
	packs := ForRole(role)
	var rules string
	for _, p := range packs {
		for _, r := range p.PromptRules {
			rules += "- " + r + "\n"
		}
	}
	return rules
}
