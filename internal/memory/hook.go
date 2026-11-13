package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type hookToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type hookPayload struct {
	// Claude / generic snake_case fields
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

	// Antigravity official camelCase protojson fields
	ConversationIDCamel string        `json:"conversationId"`
	TranscriptPathCamel string        `json:"transcriptPath"`
	WorkspacePaths      []string      `json:"workspacePaths"`
	LastUserInput       string        `json:"lastUserInput"`
	ToolCall            *hookToolCall `json:"toolCall"`
	FullyIdle           *bool         `json:"fullyIdle"`
}

func isGuardrailExemptTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "read", "read_file", "view", "view_file", "glob", "find_by_name",
		"grep", "grep_search", "ls", "list_dir", "find", "search_web",
		"query_project_memory", "verify_plan_against_memory",
		"list_resources", "read_resource",
		"manage_task", "send_message", "ask_question", "list_plugin_accounts", "search_marketplace",
		"commit_memory", "manage_memory", "project_progress", "invoke_subagent", "define_subagent", "manage_subagents":
		return true
	}
	for _, memTool := range []string{
		"_query_project_memory", "_verify_plan_against_memory",
		"_commit_memory", "_manage_memory", "_project_progress",
	} {
		if strings.HasSuffix(lower, memTool) {
			return true
		}
	}
	return false
}

// isReadOnlyShellCommand deterministically checks if a command line consists solely of
// non-mutating inspection/test commands without env overrides or file redirections.
func isReadOnlyShellCommand(raw string) bool {
	cmdStr := strings.TrimSpace(raw)
	if cmdStr == "" {
		return true
	}
	cleaned := strings.ReplaceAll(cmdStr, "2>&1", "")
	cleaned = strings.ReplaceAll(cleaned, ">/dev/null", "")
	cleaned = strings.ReplaceAll(cleaned, "> /dev/null", "")
	if strings.ContainsAny(cleaned, ">`\n") || strings.Contains(cleaned, "$(") {
		return false
	}

	replacer := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n")
	for _, seg := range strings.Split(replacer.Replace(cleaned), "\n") {
		fields := strings.Fields(strings.TrimSpace(seg))
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(fields[0], "=") {
			return false
		}
		bin := filepath.Base(fields[0])
		switch bin {
		case "ls", "pwd", "cat", "head", "tail", "wc", "ps", "lsof", "which",
			"type", "uname", "date", "stat", "file", "shasum", "grep", "rg", "echo", "true":
			continue
		case "find":
			for _, f := range fields[1:] {
				if f == "-delete" || f == "-exec" || f == "-execdir" || f == "-ok" {
					return false
				}
			}
			continue
		case "go":
			if len(fields) >= 2 && (fields[1] == "test" || fields[1] == "vet" || fields[1] == "version" || fields[1] == "env" || fields[1] == "list") {
				continue
			}
			return false
		case "git":
			idx := 1
			if len(fields) >= 3 && fields[1] == "-C" {
				idx = 3
			}
			if len(fields) <= idx {
				return false
			}
			sub := fields[idx]
			rest := fields[idx+1:]
			for _, f := range rest {
				if f == "-o" || strings.HasPrefix(f, "--output") {
					return false
				}
			}
			switch sub {
			case "status", "diff", "log", "show", "rev-parse", "ls-files":
				continue
			case "branch":
				// Only read-only listing flags allowed; creating/deleting/renaming branches is mutating
				ok := true
				for _, f := range rest {
					switch f {
					case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--list", "--show-current":
					default:
						ok = false
					}
				}
				if ok {
					continue
				}
			case "remote":
				if len(rest) == 0 || (len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose")) || (len(rest) == 2 && rest[0] == "get-url") {
					continue
				}
			}
			return false
		default:
			return false
		}
	}
	return true
}

func extractPlanFromPayload(p hookPayload) string {
	if s := strings.TrimSpace(p.ProposedPlan); s != "" {
		return s
	}
	tName := firstNonEmpty(p.ToolName, p.Tool)
	if tName == "" && p.ToolCall != nil {
		tName = strings.TrimSpace(p.ToolCall.Name)
	}
	if isGuardrailExemptTool(tName) {
		return ""
	}
	in := p.ToolInput
	if len(in) == 0 {
		in = p.Input
	}
	if len(in) == 0 && p.ToolCall != nil {
		in = p.ToolCall.Args
	}
	if len(in) == 0 {
		return ""
	}
	if strings.EqualFold(tName, "call_mcp_tool") {
		if subTool, ok := in["ToolName"].(string); ok && isGuardrailExemptTool(subTool) {
			return ""
		}
	}
	for _, ck := range []string{"command", "CommandLine"} {
		if v, ok := in[ck]; ok {
			if cmdStr, ok := v.(string); ok && isReadOnlyShellCommand(cmdStr) {
				return ""
			}
		}
	}
	var parts []string
	if tName != "" {
		parts = append(parts, "Tool: "+tName)
	}
	for _, k := range []string{"file_path", "Path", "path", "command", "CommandLine", "content", "Content", "new_string", "Replacement", "description"} {
		if v, ok := in[k]; ok {
			if s, ok := v.(string); ok {
				if k == "Replacement" {
					if oldStr, okOld := in["Target"].(string); okOld {
						s = diffEditSnippet(oldStr, s)
					}
				} else if k == "new_string" {
					if oldStr, okOld := in["old_string"].(string); okOld {
						s = diffEditSnippet(oldStr, s)
					}
				}
				s = strings.TrimSpace(s)
				if s != "" {
					s = truncateUTF8Bytes(s, 1000)
					parts = append(parts, fmt.Sprintf("%s: %s", k, s))
				}
			}
		}
	}
	if len(parts) <= 1 && tName != "" {
		if raw, err := json.Marshal(in); err == nil && len(raw) > 2 {
			parts = append(parts, "Input: "+truncateUTF8Bytes(string(raw), 1000))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

type hookWorkspaceTarget struct {
	Scope   string
	RepoDir string
}

func scopeAndRepoFromPath(rawPath string) (string, string) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", ""
	}
	dir := p
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		dir = filepath.Dir(p)
	}
	repoDir := ResolveGitRoot(dir)
	if repoDir != "" {
		base := strings.ToLower(strings.TrimSpace(filepath.Base(repoDir)))
		if base != "" && base != "." && base != "/" {
			return "project:" + base, repoDir
		}
	}
	clean := filepath.Clean(dir)
	home, _ := os.UserHomeDir()
	if clean != "." && clean != filepath.Dir(clean) && filepath.VolumeName(clean) != clean && clean != filepath.Clean(home) {
		base := strings.ToLower(strings.TrimSpace(filepath.Base(clean)))
		if base != "" {
			return "project:" + base, repoDir
		}
	}
	return "", repoDir
}

func extractToolTargetPath(p hookPayload) string {
	in := p.ToolInput
	if len(in) == 0 {
		in = p.Input
	}
	if len(in) == 0 && p.ToolCall != nil {
		in = p.ToolCall.Args
	}
	for _, k := range []string{"file_path", "Path", "path"} {
		if v, ok := in[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func resolveHookWorkspaces(p hookPayload, fallbackCwd string) ([]hookWorkspaceTarget, string) {
	if explicit := strings.TrimSpace(p.Scope); explicit != "" {
		sc := NormalizeScope(explicit)
		cwd := firstNonEmpty(p.Cwd, p.Workspace, fallbackCwd)
		if cwd == "" && len(p.WorkspacePaths) > 0 {
			for _, wp := range p.WorkspacePaths {
				wsc, wrepo := scopeAndRepoFromPath(wp)
				if wsc == sc {
					return []hookWorkspaceTarget{{Scope: sc, RepoDir: wrepo}}, resolveHookSessionID(p, sc)
				}
			}
			cwd = p.WorkspacePaths[0]
		}
		return []hookWorkspaceTarget{{Scope: sc, RepoDir: ResolveGitRoot(cwd)}}, resolveHookSessionID(p, sc)
	}

	seen := make(map[string]bool)
	var targets []hookWorkspaceTarget
	addPath := func(raw string) {
		sc, repo := scopeAndRepoFromPath(raw)
		if sc == "" || seen[sc] {
			return
		}
		seen[sc] = true
		targets = append(targets, hookWorkspaceTarget{Scope: sc, RepoDir: repo})
	}

	for _, wp := range p.WorkspacePaths {
		addPath(wp)
	}
	if len(targets) == 0 {
		cwd := firstNonEmpty(p.Cwd, p.Workspace, fallbackCwd)
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		addPath(cwd)
	}
	if len(targets) == 0 {
		targets = append(targets, hookWorkspaceTarget{Scope: DefaultScope()})
	}
	return targets, resolveHookSessionID(p, targets[0].Scope)
}

func resolveGuardrailTarget(p hookPayload, targets []hookWorkspaceTarget) hookWorkspaceTarget {
	if toolPath := extractToolTargetPath(p); toolPath != "" {
		cleanTool := filepath.Clean(toolPath)
		for _, wp := range p.WorkspacePaths {
			cleanWP := filepath.Clean(wp)
			if cleanTool == cleanWP || strings.HasPrefix(cleanTool, cleanWP+string(filepath.Separator)) {
				if sc, repo := scopeAndRepoFromPath(cleanWP); sc != "" {
					return hookWorkspaceTarget{Scope: sc, RepoDir: repo}
				}
			}
		}
		if sc, repo := scopeAndRepoFromPath(filepath.Dir(cleanTool)); sc != "" {
			return hookWorkspaceTarget{Scope: sc, RepoDir: repo}
		}
	}
	return targets[0]
}

func resolveHookSessionID(p hookPayload, primaryScope string) string {
	sid := firstNonEmpty(p.SessionID, p.ConversationID, p.ConversationIDCamel)
	if sid == "" {
		sid = "hook-session:" + primaryScope
	}
	return sid
}

// ExecuteHook executes zero-touch context injection (start/turn) or PreToolUse plan verification
// directly from lifecycle hooks without requiring an extra MCP tool-call roundtrip.
func (s *Service) ExecuteHook(ctx context.Context, mode string, rawStdin []byte, fallbackCwd string) (string, int, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	var p hookPayload
	trimmed := strings.TrimSpace(string(rawStdin))
	if strings.HasPrefix(trimmed, "{") {
		_ = json.Unmarshal([]byte(trimmed), &p)
	} else if trimmed != "" {
		p.Prompt = trimmed
	}

	if strings.TrimSpace(p.Account) != "" {
		ctx = WithAccount(ctx, p.Account)
	}
	targets, sid := resolveHookWorkspaces(p, fallbackCwd)

	switch mode {
	case "claude-start", "claude-turn", "antigravity-start", "antigravity-turn":
		t0 := time.Now()
		out, code, err := s.runContextHook(ctx, mode, p, targets, sid)
		if explicitSID := explicitHookSessionID(p); explicitSID != "" {
			elapsed := time.Since(t0)
			for _, t := range targets {
				s.RecordLiveHookElapsed(t.Scope, explicitSID, elapsed)
			}
		}
		return out, code, err
	case "claude-pretool", "antigravity-pretool", "git-precommit":
		target := resolveGuardrailTarget(p, targets)
		t0 := time.Now()
		out, code, err := s.runGuardrailHook(ctx, mode, p, target.Scope, target.RepoDir)
		if mode != "git-precommit" {
			if explicitSID := explicitHookSessionID(p); explicitSID != "" {
				s.RecordLiveHookElapsed(target.Scope, explicitSID, time.Since(t0))
			}
		}
		return out, code, err
	case "antigravity-stop", "claude-stop":
		if explicitSID := explicitHookSessionID(p); explicitSID != "" && (p.FullyIdle == nil || *p.FullyIdle) {
			for _, t := range targets {
				s.ClearLiveSession(t.Scope, explicitSID)
			}
		}
		return emptyContextHookResult(strings.HasPrefix(mode, "antigravity-"))
	case "claude-end":
		if explicitSID := explicitHookSessionID(p); explicitSID != "" {
			for _, t := range targets {
				s.ClearLiveSession(t.Scope, explicitSID)
			}
		}
		return emptyContextHookResult(false)
	case "claude-compact":
		if explicitSID := explicitHookSessionID(p); explicitSID != "" {
			tPath := firstNonEmpty(p.TranscriptPath, p.TranscriptPathCamel)
			s.RecordPostCompact(ctx, explicitSID, p.CompactSummary, tPath)
		}
		return emptyContextHookResult(false)
	default:
		return "{}", 0, nil
	}
}

func explicitHookSessionID(p hookPayload) string {
	return firstNonEmpty(p.SessionID, p.ConversationID, p.ConversationIDCamel)
}

func emptyContextHookResult(isAntigravity bool) (string, int, error) {
	if isAntigravity {
		return "{}", 0, nil
	}
	return "", 0, nil
}

func (s *Service) runContextHook(ctx context.Context, mode string, p hookPayload, targets []hookWorkspaceTarget, sid string) (string, int, error) {
	isStart := strings.HasSuffix(mode, "-start")
	isAntigravity := strings.HasPrefix(mode, "antigravity-")
	if isStart {
		s.MaybeRunAutoRetention(ctx)
	}

	rawUserPrompt := firstNonEmpty(p.Prompt, p.UserPrompt, p.LastUserInput, p.Message)
	transcriptPath := firstNonEmpty(p.TranscriptPath, p.TranscriptPathCamel)
	explicitSID := explicitHookSessionID(p)
	if explicitSID != "" {
		ctx = WithSessionID(ctx, explicitSID)
	}
	compacted, compactSummary := s.inspectCompactionForHook(ctx, sid, transcriptPath, p.Source, isStart)
	combinedTask := rawUserPrompt
	if compacted && compactSummary != "" {
		if combinedTask != "" {
			combinedTask = compactSummary + "\n\nCurrent turn: " + combinedTask
		} else {
			combinedTask = compactSummary
		}
	}
	refreshSession := isStart || compacted

	var scopeNames []string
	var bodyParts []string
	anyNewFacts := false
	anySyncMsg := false
	engineReady, readinessReason := s.CheckEngineReadiness(ctx)
	setupMsg := ""
	if !engineReady {
		setupMsg = s.ConsumeEngineSetupOffer(ctx, sid, readinessReason)
	}

	for targetIdx, t := range targets {
		scopeNames = append(scopeNames, t.Scope)
		localCtx, localCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if t.RepoDir != "" {
			s.RememberRepoPath(localCtx, t.Scope, t.RepoDir)
		}

		// Read existing active progress BEFORE touching live session state on SessionStart (or compaction recovery)
		// so startup/recovery injection surfaces prior ongoing work rather than echoing the greeting back.
		progMsg := ""
		if isStart || compacted {
			progMsg = strings.TrimSpace(s.FormatActiveProgressForSession(localCtx, t.Scope))
		}

		// Record turn state locally before remote inference; mark ephemeral live session heartbeat.
		if !isStart && explicitSID != "" && rawUserPrompt != "" {
			s.recordTurnState(localCtx, explicitSID, t.Scope, rawUserPrompt)
		}
		localCancel()
		if !isStart && explicitSID != "" {
			s.TouchLiveSession(t.Scope, explicitSID, "turn", "")
		}

		syncMsg := ""
		onboardMsg := ""
		qTask := ""
		if engineReady {
			qTask = combinedTask
			if s.isAutoGitSyncEnabled() {
				syncMsg = strings.TrimSpace(s.AutoSyncRepo(ctx, sid, t.Scope, t.RepoDir, isStart))
				if !isStart && strings.Contains(syncMsg, "(0 rule changes)") {
					syncMsg = ""
				}
			} else if isStart && t.RepoDir != "" {
				_ = s.AutoSyncProgressCommits(ctx, t.Scope, t.RepoDir)
			}
		}
		qRefresh := refreshSession && targetIdx == 0
		qOut, err := s.QuerySessionMemory(ctx, sid, qTask, "", t.Scope, qRefresh)
		if engineReady {
			onboardMsg = s.ConsumeOnboardingOffer(ctx, sid, t.Scope)
		}
		if err != nil {
			if progMsg != "" {
				anySyncMsg = true
				bodyParts = append(bodyParts, progMsg)
			}
			continue
		}
		qOut = strings.TrimSpace(qOut)
		hasNew := !strings.HasPrefix(qOut, "No new memories") &&
			!strings.HasPrefix(qOut, "No active") &&
			!strings.HasPrefix(qOut, "No relevant memories")
		if hasNew {
			anyNewFacts = true
		}
		if syncMsg != "" || onboardMsg != "" || progMsg != "" {
			anySyncMsg = true
		}
		if hasNew || (isStart && len(targets) == 1) {
			bodyParts = append(bodyParts, qOut)
		} else if isStart && len(targets) > 1 && !strings.HasPrefix(qOut, "No active memories") {
			bodyParts = append(bodyParts, fmt.Sprintf("### Scope `%s`\n%s", t.Scope, qOut))
		}
		if syncMsg != "" {
			bodyParts = append(bodyParts, syncMsg)
		}
		if progMsg != "" {
			bodyParts = append(bodyParts, progMsg)
		}
		if onboardMsg != "" {
			bodyParts = append(bodyParts, onboardMsg)
		}
	}
	if setupMsg != "" {
		anySyncMsg = true
		bodyParts = append(bodyParts, setupMsg)
	}

	if !isStart && !anyNewFacts && !anySyncMsg {
		return emptyContextHookResult(isAntigravity)
	}

	scopeHeader := strings.Join(scopeNames, ", ")
	multiHint := ""
	if len(scopeNames) > 1 {
		multiHint = fmt.Sprintf(" Multiple workspaces are active (%s): pass matching scope (e.g. %q).", scopeHeader, scopeNames[0])
	}
	headerTag := ""
	if compacted {
		headerTag = " · Post-Compact Restored"
	}

	var sections []string
	sections = append(sections, fmt.Sprintf("[MEMORIZE SSOT (%s)%s%s]", scopeHeader, headerTag, multiHint))
	if len(bodyParts) > 0 {
		sections = append(sections, strings.Join(bodyParts, "\n"))
	} else if isStart {
		sections = append(sections, "No active memories found for "+scopeHeader+".")
	}
	body := strings.Join(sections, "\n")

	if !isAntigravity {
		return body, 0, nil
	}
	b, _ := json.Marshal(map[string]any{
		"injectSteps": []map[string]any{
			{"systemMessage": map[string]string{"systemMessage": body}},
		},
	})
	return string(b), 0, nil
}

func allowGuardrailResult(mode string) (string, int, error) {
	switch mode {
	case "git-precommit":
		return "PASS", 0, nil
	case "antigravity-pretool":
		return `{"decision":"allow"}`, 0, nil
	default:
		return "{}", 0, nil
	}
}

func denyGuardrailResult(mode, reason string) (string, int, error) {
	switch mode {
	case "claude-pretool":
		b, _ := json.Marshal(map[string]any{
			"decision": "block",
			"reason":   reason,
		})
		return string(b), 2, nil
	case "antigravity-pretool":
		b, _ := json.Marshal(map[string]any{
			"decision": "deny",
			"reason":   reason,
		})
		return string(b), 0, nil
	default: // git-precommit
		return reason, 1, nil
	}
}

func extractHookToolName(p hookPayload, mode string) string {
	tName := firstNonEmpty(p.ToolName, p.Tool)
	if tName == "" && p.ToolCall != nil {
		tName = strings.TrimSpace(p.ToolCall.Name)
	}
	if tName == "" && mode == "git-precommit" {
		return "git-commit"
	}
	return tName
}

func (s *Service) checkTurnPreMutateGate(ctx context.Context, mode string, p hookPayload, scope string) (string, []int64) {
	if mode != "claude-pretool" && mode != "antigravity-pretool" {
		return "", nil
	}
	explicitSID := explicitHookSessionID(p)
	if explicitSID == "" {
		return "", nil
	}
	rawTurnInput := firstNonEmpty(p.Prompt, p.UserPrompt, p.LastUserInput, p.Message)
	cand := s.consumeTurnPreMutateCandidate(ctx, explicitSID, scope, rawTurnInput)
	runes := []rune(cand)
	if len(runes) < 6 || len(runes) > 1200 {
		return "", nil
	}
	targetScope := NormalizeScope(scope)
	existing, err := s.loadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		return "", nil
	}
	evalCtx := WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
	ev := s.evalForTask(ctx, TaskMutation)
	if ev == nil {
		return "", nil
	}
	evalStart := time.Now()
	dec, err := ev.EvaluateMutation(evalCtx, targetScope, cand, existing)
	s.recordEvalTelemetry(ctx, TaskMutation, ev, evalStart, len(existing), err)
	if err != nil {
		return "", nil
	}
	ApplyDecisionSemantics(&dec)
	th := s.GetThresholds(ctx)
	if dec.Action == "IGNORE" || dec.IsPermanent < th.MinPermanent || dec.Confidence < th.MinConfidence {
		return "", nil
	}
	if s.isTurnMutated(ctx, explicitSID, targetScope) {
		return "", nil
	}
	targetIDs := dec.ConflictFactIDs
	if len(targetIDs) == 0 && dec.ConflictFactID > 0 {
		targetIDs = []int64{dec.ConflictFactID}
	}
	if dec.Action == "OVERWRITE" && len(targetIDs) > 0 {
		return fmt.Sprintf("[MEMORIZE PRE-MUTATE GATE] Current user input updates existing memory %v. Call manage_memory(action=\"update\") or commit_memory(candidate_facts=[...]) FIRST before editing files or running commands.", targetIDs), targetIDs
	}
	return "[MEMORIZE PRE-MUTATE GATE] Current user input defines a new durable rule/decision. Call commit_memory(candidate_facts=[...]) FIRST before editing files or running commands.", nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
