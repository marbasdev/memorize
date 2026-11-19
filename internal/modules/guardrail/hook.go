package guardrail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
)

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
	if sc, repoDir := git.CanonicalScope(context.Background(), dir); sc != "" {
		return sc, repoDir
	}
	clean := filepath.Clean(dir)
	home, _ := os.UserHomeDir()
	if clean != "." && clean != filepath.Dir(clean) && filepath.VolumeName(clean) != clean && clean != filepath.Clean(home) {
		base := strings.ToLower(domain.CanonicalPathBase(p))
		if base != "" {
			return "project:" + base, ""
		}
	}
	return "", ""
}

func resolveHookSessionID(ctx context.Context, ev NormalizedHookEvent, primaryScope string) string {
	raw := ev.SessionID
	if raw == "" {
		raw = "hook-session:" + primaryScope
	}
	return domain.ScopedSessionID(ctx, raw)
}

func resolveHookWorkspaces(ctx context.Context, ev NormalizedHookEvent, fallbackCwd string) ([]hookWorkspaceTarget, string) {
	if ev.ClientCtx != nil && len(ev.ClientCtx.CanonicalScopes) > 0 {
		seen := make(map[string]bool)
		var targets []hookWorkspaceTarget
		for _, raw := range ev.ClientCtx.CanonicalScopes {
			sc := domain.NormalizeScope(raw)
			if sc != "" && !seen[sc] {
				seen[sc] = true
				targets = append(targets, hookWorkspaceTarget{Scope: sc})
			}
		}
		if len(targets) > 0 {
			return targets, resolveHookSessionID(ctx, ev, targets[0].Scope)
		}
	}
	if ev.Scope != "" {
		sc := domain.NormalizeScope(ev.Scope)
		cwd := domain.FirstNonEmpty(ev.Cwd, fallbackCwd)
		if cwd == "" && len(ev.Raw.WorkspacePaths) > 0 {
			for _, wp := range ev.Raw.WorkspacePaths {
				wsc, wrepo := scopeAndRepoFromPath(wp)
				if wsc == sc {
					return []hookWorkspaceTarget{{Scope: sc, RepoDir: wrepo}}, resolveHookSessionID(ctx, ev, sc)
				}
			}
			cwd = ev.Raw.WorkspacePaths[0]
		}
		return []hookWorkspaceTarget{{Scope: sc, RepoDir: git.ResolveRoot(cwd)}}, resolveHookSessionID(ctx, ev, sc)
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

	for _, wp := range ev.Raw.WorkspacePaths {
		addPath(wp)
	}
	if len(targets) == 0 {
		cwd := domain.FirstNonEmpty(ev.Cwd, fallbackCwd)
		if cwd == "" && !domain.IsServerMode(ctx) {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		if cwd != "" {
			addPath(cwd)
		}
	}
	if len(targets) == 0 {
		targets = append(targets, hookWorkspaceTarget{Scope: domain.DefaultScope()})
	}
	return targets, resolveHookSessionID(ctx, ev, targets[0].Scope)
}

func resolveGuardrailTarget(ev NormalizedHookEvent, targets []hookWorkspaceTarget) hookWorkspaceTarget {
	if ev.ClientCtx != nil && strings.TrimSpace(ev.ClientCtx.TargetScope) != "" {
		return hookWorkspaceTarget{Scope: domain.NormalizeScope(ev.ClientCtx.TargetScope)}
	}
	if ev.TargetPath != "" {
		cleanTool := filepath.Clean(ev.TargetPath)
		for _, wp := range ev.Raw.WorkspacePaths {
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

type teamRuleDocIngester interface {
	IngestTeamRuleDoc(ctx context.Context, rawScope, rawMarkdown string) error
}

func (s *Service) ExecuteHook(ctx context.Context, mode string, rawStdin []byte, fallbackCwd string) (string, int, error) {
	ev := decodeHookEvent(mode, rawStdin)
	if !ev.Valid {
		return "{}", 0, nil
	}
	if ev.ClientCtx == nil {
		if cc, ok := domain.ClientHookContextFromContext(ctx); ok {
			ev.ClientCtx = cc
		}
	}
	if ev.Account != "" && !domain.IsServerMode(ctx) {
		ctx = domain.WithAccount(ctx, ev.Account)
	}
	if ev.ClientCtx != nil && len(ev.ClientCtx.TeamRuleDocs) > 0 && s.rp != nil {
		if ing, ok := s.rp.(teamRuleDocIngester); ok {
			for sc, md := range ev.ClientCtx.TeamRuleDocs {
				_ = ing.IngestTeamRuleDoc(ctx, sc, md)
			}
		}
	}
	targets, sid := resolveHookWorkspaces(ctx, ev, fallbackCwd)
	codec := codecForAgent(ev.Agent)

	switch ev.Phase {
	case domain.HookPhaseSessionStart, domain.HookPhaseTurn:
		return s.runContextHook(ctx, ev, codec, targets, sid)
	case domain.HookPhasePreTool, domain.HookPhasePreCommit:
		target := resolveGuardrailTarget(ev, targets)
		return s.runGuardrailHook(ctx, ev, codec, target.Scope, target.RepoDir, sid)
	case domain.HookPhaseStop:
		return s.runStopHook(ctx, ev, codec, sid, fallbackCwd)
	case domain.HookPhaseSessionEnd:
		return codec.FormatEmpty()
	case domain.HookPhaseCompact:
		if ev.SessionID != "" {
			s.mem.RecordPostCompact(ctx, ev.SessionID, ev.Raw.CompactSummary, ev.Transcript)
		}
		return codec.FormatEmpty()
	default:
		return "{}", 0, nil
	}
}

func (s *Service) runContextHook(ctx context.Context, ev NormalizedHookEvent, codec AgentCodec, targets []hookWorkspaceTarget, sid string) (string, int, error) {
	isStart := ev.Phase == domain.HookPhaseSessionStart
	if isStart {
		s.MaybeRunAutoRetention(ctx)
	}

	ctx, combinedTask, compacted := s.prepareContextTurn(ctx, ev, sid, isStart)
	refreshSession := isStart || compacted

	engineReady, readinessReason := s.CheckEngineReadiness(ctx)
	setupMsg := ""
	if !engineReady {
		setupMsg = s.ConsumeEngineSetupOffer(ctx, sid, readinessReason)
	}

	scopeNames := make([]string, 0, len(targets))
	var bodyParts []string
	anyNewFacts := false
	anySyncMsg := false

	for idx, t := range targets {
		scopeNames = append(scopeNames, t.Scope)
		parts, hasNew, hasSync := s.collectTargetContext(ctx, sid, idx, len(targets), t, ev.SessionID, ev.Prompt, combinedTask, isStart, engineReady, refreshSession, ev.ClientCtx)
		if hasNew {
			anyNewFacts = true
		}
		if hasSync {
			anySyncMsg = true
		}
		bodyParts = append(bodyParts, parts...)
	}
	if setupMsg != "" {
		anySyncMsg = true
		bodyParts = append(bodyParts, setupMsg)
	}

	if !isStart && !anyNewFacts && !anySyncMsg {
		return codec.FormatEmpty()
	}
	return codec.FormatContext(formatContextBody(isStart, compacted, scopeNames, bodyParts))
}

func (s *Service) prepareContextTurn(ctx context.Context, ev NormalizedHookEvent, sid string, isStart bool) (context.Context, string, bool) {
	if ev.SessionID != "" {
		ctx = domain.WithSessionID(ctx, ev.SessionID)
	}
	compacted, compactSummary := s.mem.InspectCompactionForHook(ctx, sid, ev.Transcript, ev.Source, isStart)
	combinedTask := ev.Prompt
	if compacted && compactSummary != "" {
		if combinedTask != "" {
			combinedTask = compactSummary + "\n\nCurrent turn: " + combinedTask
		} else {
			combinedTask = compactSummary
		}
	}
	return ctx, combinedTask, compacted
}

func (s *Service) collectTargetContext(
	ctx context.Context,
	sid string,
	targetIdx, totalTargets int,
	t hookWorkspaceTarget,
	explicitSID, rawUserPrompt, combinedTask string,
	isStart, engineReady, refreshSession bool,
	clientCtx *domain.ClientHookContext,
) ([]string, bool, bool) {
	localCtx, localCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	s.rememberRepo(localCtx, t.Scope, t.RepoDir)
	if explicitSID != "" {
		if !isStart && rawUserPrompt != "" {
			s.mem.RecordTurnState(localCtx, explicitSID, t.Scope, rawUserPrompt)
			s.clearStalePendingTurnEdits(localCtx, explicitSID, t.Scope, rawUserPrompt)
		} else {
			s.mem.TouchTurnSession(localCtx, explicitSID, t.Scope)
		}
	}
	localCancel()

	var rpConflictMsg, syncMsg, onboardMsg string
	if s.rp != nil && strings.HasPrefix(t.Scope, "project:") {
		rpConflictMsg, _ = s.rp.SyncTurnNotice(ctx, t.Scope, t.RepoDir)
		rpConflictMsg = strings.TrimSpace(rpConflictMsg)
	}
	autoGitSync := s.mem.IsAutoGitSyncEnabled()
	if !autoGitSync && s.prog != nil {
		if clientCtx != nil && len(clientCtx.GitCommitsByScope[t.Scope]) > 0 {
			_ = s.prog.AutoSyncProgressCommitsFromPayload(ctx, t.Scope, clientCtx.GitHeadByScope[t.Scope], clientCtx.GitCommitsByScope[t.Scope])
		} else if t.RepoDir != "" {
			_ = s.prog.AutoSyncProgressCommits(ctx, t.Scope, t.RepoDir)
		}
	}
	qTask := ""
	if engineReady {
		qTask = combinedTask
		if autoGitSync {
			syncMsg = strings.TrimSpace(s.mem.AutoSyncRepo(ctx, sid, t.Scope, t.RepoDir, isStart))
			if !isStart && strings.Contains(syncMsg, "(0 rule changes)") {
				syncMsg = ""
			}
		}
	}

	qRefresh := refreshSession && targetIdx == 0
	qOut, err := s.mem.QuerySessionMemory(ctx, sid, qTask, "", t.Scope, qRefresh)
	partialMsg := ""
	if !isStart {
		partialMsg = strings.TrimSpace(s.mem.ConsumeTurnPartialNotice(ctx, sid, t.Scope))
	}
	if engineReady {
		onboardMsg = s.ConsumeOnboardingOffer(ctx, sid, t.Scope)
	}
	if err != nil {
		var fallbackParts []string
		for _, m := range []string{rpConflictMsg, partialMsg} {
			if m != "" {
				fallbackParts = append(fallbackParts, m)
			}
		}
		if len(fallbackParts) > 0 {
			return fallbackParts, false, true
		}
		return nil, false, false
	}

	qOut = strings.TrimSpace(qOut)
	hasNew := !strings.HasPrefix(qOut, "No new memories") &&
		!strings.HasPrefix(qOut, "No active") &&
		!strings.HasPrefix(qOut, "No relevant memories")
	hasSync := syncMsg != "" || rpConflictMsg != "" || onboardMsg != "" || partialMsg != ""

	var parts []string
	if hasNew || (isStart && totalTargets == 1) {
		parts = append(parts, qOut)
	} else if isStart && totalTargets > 1 && !strings.HasPrefix(qOut, "No active memories") {
		parts = append(parts, fmt.Sprintf("### Scope `%s`\n%s", t.Scope, qOut))
	}
	for _, msg := range []string{rpConflictMsg, syncMsg, onboardMsg, partialMsg} {
		if msg != "" {
			parts = append(parts, msg)
		}
	}
	return parts, hasNew, hasSync
}

func (s *Service) checkTurnPreMutateGate(ctx context.Context, ev NormalizedHookEvent, scope string) (string, []int64) {
	if ev.Phase != domain.HookPhasePreTool || ev.SessionID == "" {
		return "", nil
	}
	cand := s.mem.ConsumeTurnPreMutateCandidate(ctx, ev.SessionID, scope, ev.Prompt)
	if cand != "" {
		s.clearStalePendingTurnEdits(ctx, ev.SessionID, scope, cand)
	}
	runes := []rune(cand)
	if len(runes) < 6 || len(runes) > 1200 {
		return "", nil
	}
	targetScope := domain.NormalizeScope(scope)
	existing, err := s.mem.LoadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		s.mem.ResetTurnGateChecked(ctx, ev.SessionID, targetScope)
		return "", nil
	}
	evalCtx := domain.WithPresetCategories(ctx, s.mem.EffectiveTaxonomyJSON(ctx))
	e := s.router.EvalForTask(ctx, domain.TaskMutation)
	if e == nil {
		s.mem.ResetTurnGateChecked(ctx, ev.SessionID, targetScope)
		return "", nil
	}
	evalStart := time.Now()
	dec, err := e.EvaluateMutation(evalCtx, targetScope, cand, existing)
	s.router.RecordTelemetry(ctx, domain.TaskMutation, e, evalStart, len(existing), err)
	if err != nil {
		s.mem.ResetTurnGateChecked(ctx, ev.SessionID, targetScope)
		return "", nil
	}
	domain.ApplyDecisionSemantics(&dec)
	th := s.mem.GetThresholds(ctx)
	if dec.Action == "IGNORE" || dec.IsPermanent < th.MinPermanent || dec.Confidence < th.MinConfidence {
		return "", nil
	}
	if s.mem.IsTurnMutated(ctx, ev.SessionID, targetScope) {
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
