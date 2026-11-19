package domain

import (
	"path/filepath"
	"strings"
)

type HookPhase string

const (
	HookPhaseSessionStart HookPhase = "start"
	HookPhaseTurn         HookPhase = "turn"
	HookPhasePreTool      HookPhase = "pretool"
	HookPhaseStop         HookPhase = "stop"
	HookPhasePreCommit    HookPhase = "precommit"
	HookPhaseSessionEnd   HookPhase = "end"
	HookPhaseCompact      HookPhase = "compact"
)

var validHookPhases = map[HookPhase]bool{
	HookPhaseSessionStart: true,
	HookPhaseTurn:         true,
	HookPhasePreTool:      true,
	HookPhaseStop:         true,
	HookPhasePreCommit:    true,
	HookPhaseSessionEnd:   true,
	HookPhaseCompact:      true,
}

type AgentSpec struct {
	ID                string
	DisplayName       string
	PluginDirRelPaths []string
	ManifestRelPaths  []string
	Phases            []HookPhase
}

var agentCatalog = []AgentSpec{
	{
		ID:          "claude",
		DisplayName: "Claude",
		ManifestRelPaths: []string{
			filepath.Join(".claude-plugin", "plugin.json"),
		},
		Phases: []HookPhase{
			HookPhaseSessionStart,
			HookPhaseTurn,
			HookPhasePreTool,
			HookPhaseStop,
			HookPhaseSessionEnd,
			HookPhaseCompact,
		},
	},
	{
		ID:          "antigravity",
		DisplayName: "Antigravity",
		PluginDirRelPaths: []string{
			filepath.Join(".gemini", "config", "plugins", "memorize"),
		},
		ManifestRelPaths: []string{
			"hooks.json",
		},
		Phases: []HookPhase{
			HookPhaseSessionStart,
			HookPhaseTurn,
			HookPhasePreTool,
			HookPhaseStop,
		},
	},
	{
		ID:          "git",
		DisplayName: "Git",
		Phases: []HookPhase{
			HookPhasePreCommit,
		},
	},
}

func AllAgentSpecs() []AgentSpec {
	out := make([]AgentSpec, len(agentCatalog))
	for i, spec := range agentCatalog {
		out[i] = spec
	}
	return out
}

func FindAgentSpec(id string) (AgentSpec, bool) {
	norm := strings.ToLower(strings.TrimSpace(id))
	for _, spec := range agentCatalog {
		if spec.ID == norm {
			return spec, true
		}
	}
	return AgentSpec{}, false
}

func ModeForAgentPhase(agentID string, phase HookPhase) string {
	return strings.ToLower(strings.TrimSpace(agentID)) + "-" + string(phase)
}

func ParseHookMode(mode string) (agentID string, phase HookPhase, ok bool) {
	m := strings.ToLower(strings.TrimSpace(mode))
	rawAgent, rawPhase, hasDash := strings.Cut(m, "-")
	if !hasDash {
		return "", "", false
	}
	spec, exists := FindAgentSpec(rawAgent)
	if !exists {
		return "", "", false
	}
	p := HookPhase(rawPhase)
	if !validHookPhases[p] {
		return "", "", false
	}
	for _, supported := range spec.Phases {
		if supported == p {
			return spec.ID, p, true
		}
	}
	return "", "", false
}

func ModesForPhase(phase HookPhase) []string {
	var modes []string
	for _, spec := range agentCatalog {
		for _, p := range spec.Phases {
			if p == phase {
				modes = append(modes, ModeForAgentPhase(spec.ID, p))
				break
			}
		}
	}
	return modes
}

func FormatHookSourceLabel(source string) string {
	agentID, phase, ok := ParseHookMode(source)
	if !ok {
		return source
	}
	if phase == HookPhasePreCommit {
		return "Git Pre-Commit"
	}
	spec, _ := FindAgentSpec(agentID)
	switch phase {
	case HookPhasePreTool:
		return "PreToolUse (" + spec.DisplayName + ")"
	case HookPhaseStop:
		return "Stop Batch (" + spec.DisplayName + ")"
	case HookPhaseSessionStart:
		return "SessionStart (" + spec.DisplayName + ")"
	case HookPhaseTurn:
		return "PreInvocation (" + spec.DisplayName + ")"
	default:
		return source
	}
}
