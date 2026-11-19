package memory

import (
	_ "embed"
	"strings"
)

const PlaybookURI = "memorize://playbook/memory-curator"

//go:embed playbook/SKILL.md
var PlaybookSkillMD string

func PlaybookBodyMD() string {
	s := strings.TrimSpace(PlaybookSkillMD)
	if strings.HasPrefix(s, "---") {
		if idx := strings.Index(s[3:], "\n---"); idx >= 0 {
			return strings.TrimSpace(s[3+idx+4:])
		}
	}
	return s
}
