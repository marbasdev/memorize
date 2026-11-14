package memory

import (
	_ "embed"
	"strings"
)

// PlaybookURI is the canonical MCP resource URI exposing the embedded curator playbook.
const PlaybookURI = "memorize://playbook/memory-curator"

//go:embed playbook/SKILL.md
var PlaybookSkillMD string

// PlaybookBodyMD returns the markdown body of the embedded playbook without YAML frontmatter.
func PlaybookBodyMD() string {
	s := strings.TrimSpace(PlaybookSkillMD)
	if strings.HasPrefix(s, "---") {
		if idx := strings.Index(s[3:], "\n---"); idx >= 0 {
			return strings.TrimSpace(s[3+idx+4:])
		}
	}
	return s
}
