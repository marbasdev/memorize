package guardrail

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
	"memorize/internal/infra/sqlite"
)

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
	res := s.evaluateGuardrailPlanChunks(ctx, func(budget int) []diffPlanChunk {
		return packPlainTextChunks(plan, budget)
	}, ev, scope, minProb, false)
	return res.RuleCount, res.Blocking, res.VerifyErr
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
	var stagedSections []fileDiffSection
	if plan == "" && ev.Phase == domain.HookPhasePreCommit {
		if ev.ClientCtx != nil && len(ev.ClientCtx.StagedDiffs) > 0 {
			stagedSections = make([]fileDiffSection, 0, len(ev.ClientCtx.StagedDiffs))
			for _, sf := range ev.ClientCtx.StagedDiffs {
				stagedSections = append(stagedSections, fileDiffSection{Label: sf.Label, Body: sf.Body})
			}
		} else if repoDir != "" {
			if stagedFiles, err := git.StagedDiffFiles(ctx, repoDir); err == nil && len(stagedFiles) > 0 {
				stagedSections = make([]fileDiffSection, 0, len(stagedFiles))
				for _, sf := range stagedFiles {
					if sf.Binary || sf.Body == "(whitespace/formatting only)" {
						stagedSections = append(stagedSections, fileDiffSection{
							Label: sf.Path,
							Body:  fmt.Sprintf("File: %s %s", sf.Path, sf.Body),
						})
						continue
					}
					stagedSections = append(stagedSections, fileDiffSection{
						Label: sf.Path,
						Body:  fmt.Sprintf("File: %s\n%s", sf.Path, sf.Body),
					})
				}
			}
		}
	}
	if plan == "" && len(stagedSections) == 0 {
		return codec.FormatAllowPreTool()
	}
	hookStart := time.Now()
	toolName := ev.ToolName

	// Enforce the turn pre-mutate gate first so durable decisions in the current chat are
	// persisted before evaluating the tool call against active personal and team rules.
	if gateReason, gateIDs := s.checkTurnPreMutateGate(ctx, ev, scope); gateReason != "" {
		gateSnippet := plan
		if gateSnippet == "" && len(stagedSections) > 0 {
			gateSnippet = stagedSections[0].Body
		}
		_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       scope,
			Source:      ev.Mode,
			ToolName:    toolName,
			Verdict:     domain.GuardrailVerdictPreMutateGate,
			Blocked:     true,
			ViolatedIDs: gateIDs,
			PlanSnippet: gateSnippet,
			Details:     gateReason,
			ElapsedMs:   domain.ElapsedMsSince(hookStart),
		})
		return codec.FormatDenyPreTool(gateReason)
	}

	// File edits pass PreToolUse after the pre-mutate gate and collect their pre-turn baseline
	// so the Stop hook evaluates the net turn diff once and triggers self-correction on violations.
	if ev.Phase == domain.HookPhasePreTool && strings.TrimSpace(ev.Raw.ProposedPlan) == "" && isFileEditTool(toolName) {
		if ev.ClientCtx == nil || !ev.ClientCtx.RemoteManagedEdit {
			s.recordPendingTurnEdit(ctx, sid, scope, repoDir, toolName, ev, plan)
		}
		return codec.FormatAllowPreTool()
	}

	var (
		candidateCount int
		blocking       []domain.PlanViolation
		verifyErr      error
		logSnippet     = plan
	)
	if len(stagedSections) > 0 {
		res := s.evaluateGuardrailPlanChunks(ctx, func(budget int) []diffPlanChunk {
			return packDiffChunks(func(idx, total int, labels []string) string {
				if total <= 1 {
					return "Git staged commit diff:\n"
				}
				return fmt.Sprintf("Git staged commit diff (part %d/%d — %s):\n", idx+1, total, strings.Join(labels, ", "))
			}, stagedSections, budget)
		}, ev, scope, 0.80, false)
		candidateCount, blocking, verifyErr = res.RuleCount, res.Blocking, res.VerifyErr
		logSnippet = res.CombinedSnippet
	} else {
		candidateCount, blocking, verifyErr = s.evaluateGuardrailViolations(ctx, plan, ev, scope, 0.80)
	}
	if blocked := s.recordVerifyOutcomeLog(ctx, scope, ev.Mode, toolName, candidateCount, logSnippet, blocking, verifyErr, domain.ElapsedMsSince(hookStart)); !blocked {
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
		IsPrivate:   hasPersonalViolation(blocking),
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
