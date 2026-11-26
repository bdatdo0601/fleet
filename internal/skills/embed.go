// Package skills embeds the Fleet agent skills distributed to Claude, Codex, and Antigravity.
package skills

import _ "embed"

// Skill describes one embedded SKILL.md bundle.
type Skill struct {
	Name    string
	Content []byte
}

// Names lists all embedded Fleet skill directory names.
var Names = []string{"fleet-agents", "fleet-memory", "fleet-slack"}

//go:embed fleet-agents/SKILL.md
var fleetAgentsSkill []byte

//go:embed fleet-memory/SKILL.md
var fleetMemorySkill []byte

//go:embed fleet-slack/SKILL.md
var fleetSlackSkill []byte

// All returns all embedded Fleet skills in deterministic order.
func All() []Skill {
	return []Skill{
		{Name: "fleet-agents", Content: fleetAgentsSkill},
		{Name: "fleet-memory", Content: fleetMemorySkill},
		{Name: "fleet-slack", Content: fleetSlackSkill},
	}
}
