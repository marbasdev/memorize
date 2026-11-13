package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Han, r)
}

func containsCJKRune(s string) bool {
	for _, r := range s {
		if isCJKRune(r) {
			return true
		}
	}
	return false
}

func extractMatchTokens(text string, ignored map[string]bool) map[string]bool {
	out := make(map[string]bool)
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		raw := string(cur)
		tok := strings.ToLower(raw)
		cur = cur[:0]
		if ignored[tok] {
			return
		}
		runes := []rune(tok)
		hasCJK := false
		hasDigit := false
		for _, r := range runes {
			if isCJKRune(r) {
				hasCJK = true
			}
			if unicode.IsDigit(r) {
				hasDigit = true
			}
		}
		hasUpper := raw != tok
		if !hasCJK && !hasDigit && !hasUpper && len(runes) < 4 {
			return
		}
		if len(runes) < 2 {
			return
		}
		out[tok] = true
		if hasCJK && len(runes) >= 2 {
			for i := 0; i <= len(runes)-2; i++ {
				bg := string(runes[i : i+2])
				if !ignored[bg] {
					out[bg] = true
				}
			}
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// FilterOverlappingRules selects only rules that share significant technical tokens or CJK 2-grams
// with plan, avoiding unnecessary LLM VerifyPlan inference when a tool call is unrelated to stored rules.
func FilterOverlappingRules(plan string, rules []Fact, ignoreTokens ...string) []Fact {
	if len(rules) == 0 || strings.TrimSpace(plan) == "" {
		return nil
	}
	ignored := make(map[string]bool, len(ignoreTokens))
	for _, t := range ignoreTokens {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			ignored[t] = true
		}
	}
	planTokens := extractMatchTokens(plan, ignored)
	if len(planTokens) == 0 {
		return nil
	}
	var matched []Fact
	for _, r := range rules {
		rTokens := extractMatchTokens(r.Content, ignored)
		if len(rTokens) == 0 {
			continue
		}
		overlap := 0
		asciiHit := false
		for tk := range rTokens {
			if planTokens[tk] {
				overlap++
				if !containsCJKRune(tk) {
					asciiHit = true
				}
			}
		}
		minReq := 2
		if len(rTokens) <= 4 || (asciiHit && containsCJKRune(r.Content)) {
			minReq = 1
		}
		if overlap >= minReq {
			matched = append(matched, r)
		}
	}
	return matched
}

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

func diffEditSnippet(oldRaw, newRaw string) string {
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
	oldMid := oldRem[:len(oldRem)-s]
	newMid := newRem[:len(newRem)-s]
	if len(oldMid) > 0 && len(newMid) > 0 {
		removed := formatDiffLines("- ", oldMid)
		added := formatDiffLines("+ ", newMid)
		combined := removed + "\n" + added
		if len(combined) > 950 {
			combined = truncateUTF8Bytes(removed, 450) + "\n" + truncateUTF8Bytes(added, 450)
		}
		return combined
	}
	if len(newMid) > 0 {
		return strings.Join(newMid, "\n")
	}
	if len(oldMid) > 0 {
		return "Removed:\n" + strings.Join(oldMid, "\n")
	}
	return ""
}

func (s *Service) evaluateGuardrailViolations(ctx context.Context, plan string, p hookPayload, scope, repoDir string, minProb float64) (int, []PlanViolation, error) {
	scA, scB := scopesFor(scope, true)
	loaded, err := s.loadActiveFacts(ctx, scA, scB, "")
	if err != nil || len(loaded) == 0 {
		return 0, nil, err
	}
	rules := filterEnforceableRules(loaded)
	if len(rules) == 0 {
		return 0, nil, nil
	}

	// Strip workspace root path and ignore project name token so every file inside the project doesn't match by path alone
	planForMatch := plan
	if repoDir != "" {
		planForMatch = strings.ReplaceAll(planForMatch, repoDir, "")
	}
	for _, wp := range p.WorkspacePaths {
		if wp != "" {
			planForMatch = strings.ReplaceAll(planForMatch, wp, "")
		}
	}
	projName := strings.TrimPrefix(scA, "project:")
	candidates := FilterOverlappingRules(planForMatch, rules,
		projName, "tool", "path", "file", "content", "replacement", "commandline", "command", "input", "description")
	if len(candidates) == 0 {
		return 0, nil, nil
	}

	verifyInput := plan
	if explicitSID := explicitHookSessionID(p); explicitSID != "" {
		turnInp := firstNonEmpty(s.activeTurnInput(ctx, explicitSID, scA), p.Prompt, p.UserPrompt, p.LastUserInput)
		if turnInp != "" && len(turnInp) <= 1200 {
			verifyInput = plan + "\nCurrent turn user instruction (authoritative if overriding an older rule): " + turnInp
		}
	}

	ev := s.evalForTask(ctx, TaskVerify)
	if ev == nil {
		return len(candidates), nil, fmt.Errorf("no active evaluation engine configured")
	}
	evalStart := time.Now()
	violations, err := ev.VerifyPlan(ctx, verifyInput, candidates)
	s.recordEvalTelemetry(ctx, TaskVerify, ev, evalStart, len(candidates), err)
	if err != nil {
		return len(candidates), nil, err
	}
	return len(candidates), filterAndSortViolations(violations, minProb), nil
}

func (s *Service) runGuardrailHook(ctx context.Context, mode string, p hookPayload, scope, repoDir string) (string, int, error) {
	if repoDir != "" {
		s.RememberRepoPath(ctx, scope, repoDir)
	}
	plan := extractPlanFromPayload(p)
	if mode == "claude-pretool" || mode == "antigravity-pretool" {
		if explicitSID := explicitHookSessionID(p); explicitSID != "" {
			s.TouchLiveSession(scope, explicitSID, extractHookToolName(p, mode), extractToolTargetPath(p))
		}
	}
	if plan == "" && mode == "git-precommit" && repoDir != "" {
		if diffOut, err := runGit(ctx, repoDir, "diff", "--cached", "--unified=0"); err == nil && diffOut != "" {
			if len(diffOut) > 2000 {
				diffOut = diffOut[:2000] + "..."
			}
			plan = "Git staged commit diff:\n" + diffOut
		}
	}
	if plan == "" {
		return allowGuardrailResult(mode)
	}
	hookStart := time.Now()

	var (
		wg             sync.WaitGroup
		gateReason     string
		gateIDs        []int64
		candidateCount int
		blocking       []PlanViolation
		verifyErr      error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		gateReason, gateIDs = s.checkTurnPreMutateGate(ctx, mode, p, scope)
	}()
	go func() {
		defer wg.Done()
		candidateCount, blocking, verifyErr = s.evaluateGuardrailViolations(ctx, plan, p, scope, repoDir, 0.80)
	}()
	wg.Wait()
	elapsedMs := elapsedMsSince(hookStart)

	toolName := extractHookToolName(p, mode)
	if gateReason != "" {
		_, _ = s.RecordGuardrailLog(ctx, GuardrailLogInput{
			Scope:       scope,
			Source:      mode,
			ToolName:    toolName,
			Verdict:     GuardrailVerdictPreMutateGate,
			Blocked:     true,
			RuleCount:   candidateCount,
			ViolatedIDs: gateIDs,
			PlanSnippet: plan,
			Details:     gateReason,
			ElapsedMs:   elapsedMs,
		})
		return denyGuardrailResult(mode, gateReason)
	}
	if verifyErr != nil {
		if os.Getenv("MEMORY_GUARDRAIL_FAIL_CLOSED") == "1" {
			return denyGuardrailResult(mode, "guardrail verification failed under fail-closed policy")
		}
		return allowGuardrailResult(mode)
	}
	if len(blocking) == 0 {
		if candidateCount > 0 {
			_, _ = s.RecordGuardrailLog(ctx, GuardrailLogInput{
				Scope:       scope,
				Source:      mode,
				ToolName:    toolName,
				Verdict:     GuardrailVerdictPass,
				Blocked:     false,
				RuleCount:   candidateCount,
				PlanSnippet: plan,
				ElapsedMs:   elapsedMs,
			})
		}
		return allowGuardrailResult(mode)
	}

	vIDs, vDetails := formatViolationDetails(blocking)
	_, _ = s.RecordGuardrailLog(ctx, GuardrailLogInput{
		Scope:       scope,
		Source:      mode,
		ToolName:    toolName,
		Verdict:     GuardrailVerdictRuleViolation,
		Blocked:     true,
		RuleCount:   candidateCount,
		ViolatedIDs: vIDs,
		PlanSnippet: plan,
		Details:     vDetails,
		ElapsedMs:   elapsedMs,
	})

	var sb strings.Builder
	sb.WriteString("[MEMORIZE GUARDRAIL BLOCKED] Proposed action violates active project rules:\n")
	for _, v := range blocking {
		fmt.Fprintf(&sb, "- [%s / %s #%d] %s (violation prob: %.0f%%)\n",
			v.Scope, v.Category, v.ID, v.Content, v.Probability*100)
	}
	return denyGuardrailResult(mode, strings.TrimSpace(sb.String()))
}
