package domain

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
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
			filepath.Join(".gemini", "config", "plugins", "praxis"),
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
	copy(out, agentCatalog)
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

const maxTranscriptAssistantTailBytes = 600

func extractTranscriptVisibleText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		var parts []string
		for _, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if tp, ok := m["type"].(string); ok && tp != "" && !strings.EqualFold(tp, "text") {
				continue
			}
			if thought, _ := m["thought"].(bool); thought {
				continue
			}
			if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
				parts = append(parts, strings.TrimSpace(txt))
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func isHumanTranscriptUserContent(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		hasText := false
		for _, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			tp, _ := m["type"].(string)
			if strings.EqualFold(tp, "tool_result") {
				return false
			}
			if (tp == "" || strings.EqualFold(tp, "text")) && strings.TrimSpace(stringVal(m["text"])) != "" {
				hasText = true
			}
		}
		return hasText
	default:
		return false
	}
}

func stringVal(v any) string {
	s, _ := v.(string)
	return s
}

func tailUTF8String(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes || maxBytes <= 0 {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	if nl := strings.IndexByte(s[start:], '\n'); nl >= 0 && nl <= 100 && start+nl+1 < len(s) {
		start += nl + 1
	}
	return "..." + strings.TrimSpace(s[start:])
}

// ParseTranscriptAssistantTail extracts the trailing portion of the Assistant response immediately preceding
// the latest explicit User turn from a JSONL transcript tail (supporting both Antigravity and Claude Code).
func ParseTranscriptAssistantTail(rawTail []byte, snappedMidLine bool) string {
	if len(rawTail) == 0 {
		return ""
	}
	chunk := rawTail
	if snappedMidLine {
		nl := bytes.IndexByte(chunk, '\n')
		if nl < 0 {
			return ""
		}
		chunk = chunk[nl+1:]
	}

	var latestAssistant, beforeLastUser string
	seenUser := false

	for _, rawLine := range bytes.Split(chunk, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(rawLine)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Source  string `json:"source"`
			Content any    `json:"content"`
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(trimmed, &entry) != nil {
			continue
		}
		tp := strings.ToLower(strings.TrimSpace(entry.Type))
		src := strings.ToLower(strings.TrimSpace(entry.Source))
		role := strings.ToLower(strings.TrimSpace(entry.Message.Role))

		switch {
		case tp == "planner_response":
			if txt := extractTranscriptVisibleText(entry.Content); txt != "" {
				latestAssistant = txt
			}
		case tp == "user_input":
			if (src == "" || src == "user_explicit") && isHumanTranscriptUserContent(entry.Content) {
				seenUser = true
				beforeLastUser = latestAssistant
			}
		case tp == "assistant" || role == "assistant":
			msgContent := entry.Message.Content
			if msgContent == nil {
				msgContent = entry.Content
			}
			if txt := extractTranscriptVisibleText(msgContent); txt != "" {
				latestAssistant = txt
			}
		case tp == "user" || role == "user":
			msgContent := entry.Message.Content
			if msgContent == nil {
				msgContent = entry.Content
			}
			if isHumanTranscriptUserContent(msgContent) {
				seenUser = true
				beforeLastUser = latestAssistant
			}
		}
	}

	chosen := latestAssistant
	if seenUser {
		chosen = beforeLastUser
	}
	if chosen == "" {
		return ""
	}
	return tailUTF8String(RedactSecrets(chosen), maxTranscriptAssistantTailBytes)
}
