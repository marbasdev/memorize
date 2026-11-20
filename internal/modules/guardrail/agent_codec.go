package guardrail

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"praxis/internal/domain"
)

type hookToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type hookPayload struct {
	SessionID      string         `json:"session_id"`
	ConversationID string         `json:"conversation_id"`
	Account        string         `json:"account"`
	Cwd            string         `json:"cwd"`
	Workspace      string         `json:"workspace"`
	Scope          string         `json:"scope"`
	HookEventName  string         `json:"hook_event_name"`
	Source         string         `json:"source"`
	CompactSummary string         `json:"compact_summary"`
	TranscriptPath string         `json:"transcript_path"`
	Prompt         string         `json:"prompt"`
	UserPrompt     string         `json:"user_prompt"`
	Message        string         `json:"message"`
	ToolName       string         `json:"tool_name"`
	Tool           string         `json:"tool"`
	ProposedPlan   string         `json:"proposed_plan"`
	ToolInput      map[string]any `json:"tool_input"`
	Input          map[string]any `json:"input"`

	ConversationIDCamel string        `json:"conversationId"`
	TranscriptPathCamel string        `json:"transcriptPath"`
	WorkspacePaths      []string      `json:"workspacePaths"`
	LastUserInput       string        `json:"lastUserInput"`
	ToolCall            *hookToolCall `json:"toolCall"`
	FullyIdle           *bool         `json:"fullyIdle"`
	TerminationReason   string        `json:"terminationReason"`
	ContinuationActive  bool          `json:"continuationActive"`

	StopHookActive bool `json:"stop_hook_active"`

	ClientCtx *domain.ClientHookContext `json:"client_ctx,omitempty"`
}

type NormalizedHookEvent struct {
	Mode       string
	Agent      string
	Phase      domain.HookPhase
	Valid      bool
	Raw        hookPayload
	SessionID  string
	Account    string
	Cwd        string
	Scope      string
	Source     string
	Transcript string
	Prompt     string
	TurnPrompt string
	ToolName   string
	ToolInput  map[string]any
	TargetPath string
	ClientCtx  *domain.ClientHookContext
}

func decodeHookEvent(mode string, rawStdin []byte) NormalizedHookEvent {
	m := strings.ToLower(strings.TrimSpace(mode))
	agentID, phase, ok := domain.ParseHookMode(m)

	var p hookPayload
	trimmed := strings.TrimSpace(string(rawStdin))
	if strings.HasPrefix(trimmed, "{") {
		_ = json.Unmarshal([]byte(trimmed), &p)
	} else if trimmed != "" {
		p.Prompt = trimmed
	}

	toolIn := p.ToolInput
	if len(toolIn) == 0 {
		toolIn = p.Input
	}
	if len(toolIn) == 0 && p.ToolCall != nil {
		toolIn = p.ToolCall.Args
	}

	toolName := domain.FirstNonEmpty(p.ToolName, p.Tool)
	if toolName == "" && p.ToolCall != nil {
		toolName = strings.TrimSpace(p.ToolCall.Name)
	}
	if toolName == "" && phase == domain.HookPhasePreCommit {
		toolName = "git-commit"
	}

	targetPath := domain.ExtractTargetFilePath(toolIn)

	return NormalizedHookEvent{
		Mode:       m,
		Agent:      agentID,
		Phase:      phase,
		Valid:      ok,
		Raw:        p,
		SessionID:  domain.FirstNonEmpty(p.SessionID, p.ConversationID, p.ConversationIDCamel),
		Account:    strings.TrimSpace(p.Account),
		Cwd:        domain.FirstNonEmpty(p.Cwd, p.Workspace),
		Scope:      strings.TrimSpace(p.Scope),
		Source:     strings.TrimSpace(p.Source),
		Transcript: domain.FirstNonEmpty(p.TranscriptPath, p.TranscriptPathCamel),
		Prompt:     domain.FirstNonEmpty(p.Prompt, p.UserPrompt, p.LastUserInput, p.Message),
		TurnPrompt: domain.FirstNonEmpty(p.Prompt, p.UserPrompt, p.LastUserInput),
		ToolName:   toolName,
		ToolInput:  toolIn,
		TargetPath: targetPath,
		ClientCtx:  p.ClientCtx,
	}
}

type AgentCodec interface {
	FormatEmpty() (string, int, error)
	FormatContext(body string) (string, int, error)
	FormatAllowPreTool() (string, int, error)
	FormatDenyPreTool(reason string) (string, int, error)
	FormatStopContinue(reason string) (string, int, error)
}

var (
	agentCodecsMu sync.RWMutex
	agentCodecs   = map[string]AgentCodec{}
)

func RegisterAgentCodec(agentID string, codec AgentCodec) {
	id := strings.ToLower(strings.TrimSpace(agentID))
	if id == "" || codec == nil {
		return
	}
	agentCodecsMu.Lock()
	agentCodecs[id] = codec
	agentCodecsMu.Unlock()
}

func codecForAgent(agentID string) AgentCodec {
	id := strings.ToLower(strings.TrimSpace(agentID))
	agentCodecsMu.RLock()
	codec, ok := agentCodecs[id]
	agentCodecsMu.RUnlock()
	if ok {
		return codec
	}
	return claudeAgentCodec{}
}

func init() {
	RegisterAgentCodec("claude", claudeAgentCodec{})
	RegisterAgentCodec("antigravity", antigravityAgentCodec{})
	RegisterAgentCodec("git", gitAgentCodec{})
}

type claudeAgentCodec struct{}

func (claudeAgentCodec) FormatEmpty() (string, int, error) { return "", 0, nil }
func (claudeAgentCodec) FormatContext(body string) (string, int, error) {
	return body, 0, nil
}
func (claudeAgentCodec) FormatAllowPreTool() (string, int, error) { return "{}", 0, nil }
func (claudeAgentCodec) FormatDenyPreTool(reason string) (string, int, error) {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": reason})
	return string(b), 2, nil
}
func (claudeAgentCodec) FormatStopContinue(reason string) (string, int, error) {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": reason})
	return string(b), 0, nil
}

type antigravityAgentCodec struct{}

func (antigravityAgentCodec) FormatEmpty() (string, int, error) { return "{}", 0, nil }
func (antigravityAgentCodec) FormatContext(body string) (string, int, error) {
	b, _ := json.Marshal(map[string]any{
		"injectSteps": []map[string]any{
			{"systemMessage": map[string]string{"systemMessage": body}},
		},
	})
	return string(b), 0, nil
}
func (antigravityAgentCodec) FormatAllowPreTool() (string, int, error) {
	return `{"decision":"allow"}`, 0, nil
}
func (antigravityAgentCodec) FormatDenyPreTool(reason string) (string, int, error) {
	b, _ := json.Marshal(map[string]any{"decision": "deny", "reason": reason})
	return string(b), 0, nil
}
func (antigravityAgentCodec) FormatStopContinue(reason string) (string, int, error) {
	b, _ := json.Marshal(map[string]any{"decision": "continue", "reason": reason})
	return string(b), 0, nil
}

type gitAgentCodec struct{}

func (gitAgentCodec) FormatEmpty() (string, int, error)                     { return "", 0, nil }
func (gitAgentCodec) FormatContext(body string) (string, int, error)        { return body, 0, nil }
func (gitAgentCodec) FormatAllowPreTool() (string, int, error)              { return "PASS", 0, nil }
func (gitAgentCodec) FormatDenyPreTool(reason string) (string, int, error)  { return reason, 1, nil }
func (gitAgentCodec) FormatStopContinue(reason string) (string, int, error) { return reason, 1, nil }

func formatContextBody(isStart, compacted bool, scopeNames, bodyParts []string) string {
	scopeHeader := strings.Join(scopeNames, ", ")
	multiHint := ""
	if len(scopeNames) > 1 {
		multiHint = fmt.Sprintf(" Multiple workspaces are active (%s): pass matching scope (e.g. %q).", scopeHeader, scopeNames[0])
	}
	headerTag := ""
	if compacted {
		headerTag = " · Post-Compact Restored"
	}

	sections := []string{fmt.Sprintf("[PRAXIS SSOT (%s)%s%s]", scopeHeader, headerTag, multiHint)}
	if len(bodyParts) > 0 {
		sections = append(sections, strings.Join(bodyParts, "\n"))
	} else if isStart {
		sections = append(sections, "No active memories found for "+scopeHeader+".")
	}
	return strings.Join(sections, "\n")
}
