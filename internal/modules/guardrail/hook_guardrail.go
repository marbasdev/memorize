package guardrail

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/memory"
)

func formatDiffLines(prefix string, lines []string) string {
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(prefix)
		sb.WriteString(l)
	}
	return sb.String()
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b] + "..."
}

func trimChangedLineEdges(oldRaw, newRaw string) (oldMid, newMid []string) {
	oldLines := strings.Split(oldRaw, "\n")
	newLines := strings.Split(newRaw, "\n")
	p := 0
	for p < len(oldLines) && p < len(newLines) && oldLines[p] == newLines[p] {
		p++
	}
	oldRem := oldLines[p:]
	newRem := newLines[p:]
	s := 0
	for s < len(oldRem) && s < len(newRem) && oldRem[len(oldRem)-1-s] == newRem[len(newRem)-1-s] {
		s++
	}
	return oldRem[:len(oldRem)-s], newRem[:len(newRem)-s]
}

func formatEdgeDiff(oldMid, newMid []string, addPrefix string, maxBytes int) string {
	if len(oldMid) == 0 && len(newMid) == 0 {
		return ""
	}
	if len(oldMid) > 0 && len(newMid) > 0 {
		removed := formatDiffLines("- ", oldMid)
		added := formatDiffLines("+ ", newMid)
		combined := removed + "\n" + added
		if maxBytes > 0 && len(combined) > maxBytes {
			half := max(64, (maxBytes-50)/2)
			combined = truncateUTF8Bytes(removed, half) + "\n" + truncateUTF8Bytes(added, half)
		}
		return combined
	}
	if len(newMid) > 0 {
		return truncateUTF8Bytes(formatDiffLines(addPrefix, newMid), maxBytes)
	}
	return truncateUTF8Bytes("Removed:\n"+strings.Join(oldMid, "\n"), maxBytes)
}

func diffEditSnippet(oldRaw, newRaw string) string {
	oldMid, newMid := trimChangedLineEdges(oldRaw, newRaw)
	return formatEdgeDiff(oldMid, newMid, "", 950)
}

func (s *Service) activeOverrideTurnInput(ctx context.Context, ev NormalizedHookEvent, scopeKey string) string {
	if ev.SessionID == "" || s.mem.IsTurnMutated(ctx, ev.SessionID, scopeKey) {
		return ""
	}
	turnInp := strings.TrimSpace(domain.FirstNonEmpty(s.mem.ActiveTurnInput(ctx, ev.SessionID, scopeKey), ev.TurnPrompt))
	if turnInp == "" || len(turnInp) > 1200 {
		return ""
	}
	return turnInp
}

func (s *Service) evaluateGuardrailViolations(ctx context.Context, plan string, ev NormalizedHookEvent, scope string, minProb float64) (int, []domain.PlanViolation, error) {
	scA, scB := memory.ScopesFor(scope, true)
	rules, err := s.loadEnforceableFacts(ctx, scA, scB)
	if err != nil || len(rules) == 0 {
		return 0, nil, err
	}

	eval := s.router.EvalForTask(ctx, domain.TaskVerify)
	if eval == nil {
		return len(rules), nil, fmt.Errorf("no active evaluation engine configured")
	}
	// Phase 1: evaluate strictly on concrete action/diff without user turn prose so benign
	// commands cannot trigger rules from chat wording while prefix caches stay stable.
	evalStart := time.Now()
	violations, err := eval.VerifyPlan(ctx, plan, rules)
	s.router.RecordTelemetry(ctx, domain.TaskVerify, eval, evalStart, len(rules), err)
	if err != nil {
		return len(rules), nil, err
	}
	blocking := filterAndSortViolations(violations, minProb)
	if len(blocking) == 0 {
		return len(rules), nil, nil
	}

	// Phase 2 (lazy override): only re-check flagged rules with Turn Context when Phase 1
	// hits and sufficient hook deadline budget remains.
	turnInp := s.activeOverrideTurnInput(ctx, ev, scA)
	if turnInp == "" {
		return len(rules), blocking, nil
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 1200*time.Millisecond {
		return len(rules), blocking, nil
	}
	flagged := make([]domain.Fact, 0, len(blocking))
	for _, b := range blocking {
		flagged = append(flagged, b.Fact)
	}
	overridePlan := plan + "\nTurn Context (User Prompt - check ONLY whether this instruction explicitly overrides the flagged rules): " + turnInp
	ovStart := time.Now()
	ovViolations, ovErr := eval.VerifyPlan(ctx, overridePlan, flagged)
	s.router.RecordTelemetry(ctx, domain.TaskVerify, eval, ovStart, len(flagged), ovErr)
	if ovErr != nil {
		return len(rules), blocking, nil
	}
	return len(rules), filterAndSortViolations(ovViolations, minProb), nil
}

func (s *Service) rememberRepo(ctx context.Context, scope, repoDir string) {
	if repoDir == "" {
		return
	}
	if s.prog != nil {
		s.prog.RememberRepoPath(ctx, scope, repoDir)
		return
	}
	sqlite.RememberRepoPath(ctx, s.db, scope, repoDir)
}

func (s *Service) runGuardrailHook(ctx context.Context, ev NormalizedHookEvent, codec AgentCodec, scope, repoDir, sid string) (string, int, error) {
	s.rememberRepo(ctx, scope, repoDir)
	if s.rp != nil && strings.HasPrefix(scope, "project:") {
		_, _ = s.rp.SyncTurnNotice(ctx, scope, repoDir)
	}
	plan := extractPlanFromEvent(ev)
	if ev.Phase == domain.HookPhasePreTool && ev.SessionID != "" {
		s.mem.TouchTurnSession(ctx, ev.SessionID, scope)
	}
	if plan == "" && ev.Phase == domain.HookPhasePreCommit && repoDir != "" {
		if diffOut, err := git.Run(ctx, repoDir, "diff", "--cached", "--unified=0"); err == nil && diffOut != "" {
			if len(diffOut) > 2000 {
				diffOut = diffOut[:2000] + "..."
			}
			plan = "Git staged commit diff:\n" + diffOut
		}
	}
	if plan == "" {
		return codec.FormatAllowPreTool()
	}
	hookStart := time.Now()
	toolName := ev.ToolName

	// Enforce the turn pre-mutate gate first so durable decisions in the current chat are
	// persisted before evaluating the tool call against active personal and team rules.
	if gateReason, gateIDs := s.checkTurnPreMutateGate(ctx, ev, scope); gateReason != "" {
		_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       scope,
			Source:      ev.Mode,
			ToolName:    toolName,
			Verdict:     domain.GuardrailVerdictPreMutateGate,
			Blocked:     true,
			ViolatedIDs: gateIDs,
			PlanSnippet: plan,
			Details:     gateReason,
			ElapsedMs:   domain.ElapsedMsSince(hookStart),
		})
		return codec.FormatDenyPreTool(gateReason)
	}

	// File edits pass PreToolUse after the pre-mutate gate and collect their pre-turn baseline
	// so the Stop hook evaluates the net turn diff once and triggers self-correction on violations.
	if ev.Phase == domain.HookPhasePreTool && strings.TrimSpace(ev.Raw.ProposedPlan) == "" && isFileEditTool(toolName) {
		s.recordPendingTurnEdit(ctx, sid, scope, repoDir, toolName, ev, plan)
		return codec.FormatAllowPreTool()
	}

	candidateCount, blocking, verifyErr := s.evaluateGuardrailViolations(ctx, plan, ev, scope, 0.80)
	if blocked := s.recordVerifyOutcomeLog(ctx, scope, ev.Mode, toolName, candidateCount, plan, blocking, verifyErr, domain.ElapsedMsSince(hookStart)); !blocked {
		return codec.FormatAllowPreTool()
	}
	if verifyErr != nil {
		return codec.FormatDenyPreTool("guardrail verification failed under fail-closed policy")
	}

	var sb strings.Builder
	sb.WriteString("[MEMORIZE GUARDRAIL BLOCKED] Proposed action violates active project rules:\n")
	for _, v := range blocking {
		fmt.Fprintf(&sb, "- [%s / %s %s] %s (violation prob: %.0f%%)\n",
			v.Scope, v.Category, formatViolationRef(v.Fact), v.Content, v.Probability*100)
	}
	return codec.FormatDenyPreTool(strings.TrimSpace(sb.String()))
}

func (s *Service) recordVerifyOutcomeLog(ctx context.Context, scope, source, toolName string, ruleCount int, planSnippet string, blocking []domain.PlanViolation, verifyErr error, elapsedMs float64) bool {
	if verifyErr != nil {
		failClosed := os.Getenv("MEMORY_GUARDRAIL_FAIL_CLOSED") == "1"
		_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       scope,
			Source:      source,
			ToolName:    toolName,
			Verdict:     domain.GuardrailVerdictError,
			Blocked:     failClosed,
			RuleCount:   ruleCount,
			PlanSnippet: planSnippet,
			Details:     verifyErr.Error(),
			ElapsedMs:   elapsedMs,
		})
		return failClosed
	}
	if len(blocking) == 0 {
		if ruleCount > 0 {
			_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
				Scope:       scope,
				Source:      source,
				ToolName:    toolName,
				Verdict:     domain.GuardrailVerdictPass,
				Blocked:     false,
				RuleCount:   ruleCount,
				PlanSnippet: planSnippet,
				ElapsedMs:   elapsedMs,
			})
		}
		return false
	}
	vIDs, vDetails := formatViolationDetails(blocking)
	_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
		Scope:       scope,
		Source:      source,
		ToolName:    toolName,
		Verdict:     domain.GuardrailVerdictRuleViolation,
		Blocked:     true,
		RuleCount:   ruleCount,
		ViolatedIDs: vIDs,
		PlanSnippet: planSnippet,
		Details:     vDetails,
		ElapsedMs:   elapsedMs,
	})
	return true
}
