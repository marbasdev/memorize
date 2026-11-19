package guardrail

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

const (
	defaultUnknownDiffChunkBytes = 3600
	minDiffChunkBytes            = 1800
	maxDiffChunkBytes            = 24576
	maxTurnFallbackSnippetBytes  = 8000
	stopChunkMinRemainingTime    = 1500 * time.Millisecond
)

type fileDiffSection struct {
	Label string
	Body  string
}

type diffPlanChunk struct {
	Plan   string
	Labels []string
}

type chunkedVerifyOutcome struct {
	RuleCount            int
	TotalChunks          int
	EvaluatedChunks      int
	Blocking             []domain.PlanViolation
	HasPersonalViolation bool
	Partial              bool
	UnverifiedFiles      []string
	CombinedSnippet      string
	VerifyErr            error
}

func estimateMaxRuleBatchBytes(rules []domain.Fact, batchSize int) int {
	if len(rules) == 0 {
		return 0
	}
	if batchSize <= 0 || batchSize > len(rules) {
		batchSize = len(rules)
	}
	maxBytes := 0
	for start := 0; start < len(rules); start += batchSize {
		end := min(start+batchSize, len(rules))
		cur := 0
		for _, r := range rules[start:end] {
			cur += len(r.Category) + len(r.Content) + 14
		}
		if cur > maxBytes {
			maxBytes = cur
		}
	}
	return maxBytes
}

func resolveVerifyDiffBudget(ctx context.Context, eval domain.Evaluator, rules []domain.Fact) int {
	batchSize := domain.EvaluatorRuleBatchSize(eval)
	if batchSize <= 0 {
		batchSize = 20
	}
	maxRuleBytes := estimateMaxRuleBatchBytes(rules, batchSize)
	tokLimit := domain.EvaluatorInputTokenLimit(ctx, eval)
	if tokLimit <= 0 {
		if maxRuleBytes <= 3500 {
			return defaultUnknownDiffChunkBytes
		}
		adjusted := defaultUnknownDiffChunkBytes - (maxRuleBytes-3500)/2
		return max(minDiffChunkBytes, adjusted)
	}

	const reservedOverheadTokens = 1150
	ruleTokens := (maxRuleBytes*2 + 4) / 5 // conservative ~2.5 bytes/token for mixed prose/rules
	availTokens := tokLimit - reservedOverheadTokens - ruleTokens
	if availTokens <= 0 {
		return minDiffChunkBytes
	}
	budget := (availTokens * 11) / 5
	if budget < minDiffChunkBytes {
		return minDiffChunkBytes
	}
	if budget > maxDiffChunkBytes {
		return maxDiffChunkBytes
	}
	return budget
}

func splitOversizedFileSection(label, rawSection string, maxSectionBytes int) []fileDiffSection {
	if len(rawSection) <= maxSectionBytes || maxSectionBytes <= 256 {
		return []fileDiffSection{{Label: label, Body: rawSection}}
	}
	body := rawSection
	headerPrefix := "File: " + label + "\n"
	if strings.HasPrefix(body, headerPrefix) {
		body = strings.TrimPrefix(body, headerPrefix)
	}
	lines := strings.Split(body, "\n")
	partCap := max(256, maxSectionBytes-len(label)-28)

	var groups [][]string
	var cur []string
	curBytes := 0
	for _, line := range lines {
		line = domain.CompactDiffLine(line, partCap)
		addLen := len(line)
		if len(cur) > 0 {
			addLen++
		}
		if len(cur) > 0 && curBytes+addLen > partCap {
			groups = append(groups, cur)
			cur = []string{line}
			curBytes = len(line)
			continue
		}
		cur = append(cur, line)
		curBytes += addLen
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	if len(groups) <= 1 {
		return []fileDiffSection{{Label: label, Body: rawSection}}
	}

	out := make([]fileDiffSection, 0, len(groups))
	total := len(groups)
	for i, g := range groups {
		out = append(out, fileDiffSection{
			Label: label,
			Body:  fmt.Sprintf("File: %s (part %d/%d)\n%s", label, i+1, total, strings.Join(g, "\n")),
		})
	}
	return out
}

func appendUniqueLabel(labels []string, seen map[string]bool, label string) []string {
	if label == "" || seen[label] {
		return labels
	}
	seen[label] = true
	return append(labels, label)
}

func packDiffChunks(headerFn func(chunkIdx, totalChunks int, labels []string) string, sections []fileDiffSection, chunkBudget int) []diffPlanChunk {
	if len(sections) == 0 {
		return nil
	}
	if chunkBudget < minDiffChunkBytes {
		chunkBudget = minDiffChunkBytes
	}
	maxSectionBytes := max(512, chunkBudget-128)
	var units []fileDiffSection
	for _, sec := range sections {
		units = append(units, splitOversizedFileSection(sec.Label, sec.Body, maxSectionBytes)...)
	}

	type rawGroup struct {
		labels []string
		bodies []string
	}
	var groups []rawGroup
	var curBodies []string
	var curLabels []string
	curSeen := make(map[string]bool)
	curBodyBytes := 0

	flush := func() {
		if len(curBodies) == 0 {
			return
		}
		groups = append(groups, rawGroup{labels: curLabels, bodies: curBodies})
		curBodies = nil
		curLabels = nil
		curSeen = make(map[string]bool)
		curBodyBytes = 0
	}

	for _, u := range units {
		bodyCost := len(u.Body)
		if len(curBodies) > 0 {
			bodyCost += 2 // "\n\n"
		}
		estLabelsLen := len(strings.Join(curLabels, ", ")) + len(u.Label) + 48
		if len(curBodies) > 0 && curBodyBytes+bodyCost+estLabelsLen > chunkBudget {
			flush()
		}
		curBodies = append(curBodies, u.Body)
		curLabels = appendUniqueLabel(curLabels, curSeen, u.Label)
		if len(curBodies) == 1 {
			curBodyBytes = len(u.Body)
		} else {
			curBodyBytes += 2 + len(u.Body)
		}
	}
	flush()

	out := make([]diffPlanChunk, 0, len(groups))
	total := len(groups)
	for i, g := range groups {
		hdr := headerFn(i, total, g.labels)
		out = append(out, diffPlanChunk{
			Plan:   hdr + strings.Join(g.bodies, "\n\n"),
			Labels: g.labels,
		})
	}
	return out
}

func packPlainTextChunks(text string, chunkBudget int) []diffPlanChunk {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if chunkBudget < minDiffChunkBytes {
		chunkBudget = minDiffChunkBytes
	}
	if len(trimmed) <= chunkBudget {
		return []diffPlanChunk{{Plan: trimmed}}
	}

	lines := strings.Split(trimmed, "\n")
	var parts []string
	var cur []string
	curBytes := 0
	partCap := max(512, chunkBudget-48)

	for _, line := range lines {
		for len(line) > partCap {
			if len(cur) > 0 {
				parts = append(parts, strings.Join(cur, "\n"))
				cur = nil
				curBytes = 0
			}
			piece := domain.TruncateUTF8Bytes(line, partCap)
			piece = strings.TrimSuffix(piece, "...")
			if piece == "" {
				break
			}
			parts = append(parts, piece)
			line = line[len(piece):]
		}
		if line == "" && len(cur) > 0 {
			continue
		}
		add := len(line)
		if len(cur) > 0 {
			add++
		}
		if len(cur) > 0 && curBytes+add > partCap {
			parts = append(parts, strings.Join(cur, "\n"))
			cur = []string{line}
			curBytes = len(line)
			continue
		}
		cur = append(cur, line)
		curBytes += add
	}
	if len(cur) > 0 {
		parts = append(parts, strings.Join(cur, "\n"))
	}
	if len(parts) <= 1 {
		return []diffPlanChunk{{Plan: trimmed}}
	}

	out := make([]diffPlanChunk, 0, len(parts))
	for i, p := range parts {
		out = append(out, diffPlanChunk{
			Plan: fmt.Sprintf("[Plan part %d/%d]\n%s", i+1, len(parts), p),
		})
	}
	return out
}

func collectUnverifiedLabels(chunks []diffPlanChunk, startIdx int) []string {
	seen := make(map[string]bool)
	var out []string
	for i := startIdx; i < len(chunks); i++ {
		for _, lbl := range chunks[i].Labels {
			out = appendUniqueLabel(out, seen, lbl)
		}
	}
	return out
}

func combineChunkSnippets(chunks []diffPlanChunk, evaluated int) string {
	if len(chunks) == 0 {
		return ""
	}
	if len(chunks) == 1 {
		return chunks[0].Plan
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d/%d diff chunks evaluated]\n", evaluated, len(chunks))
	for i, ch := range chunks {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(ch.Plan)
		if sb.Len() >= 2000 {
			break
		}
	}
	return sb.String()
}

func (s *Service) evaluateGuardrailPlanChunks(
	ctx context.Context,
	buildChunks func(budget int) []diffPlanChunk,
	ev NormalizedHookEvent,
	scope string,
	minProb float64,
	enforceDeadline bool,
) chunkedVerifyOutcome {
	scA, scB := memory.ScopesFor(scope, true)
	rules, err := s.loadEnforceableFacts(ctx, scA, scB)
	if err != nil || len(rules) == 0 {
		return chunkedVerifyOutcome{VerifyErr: err}
	}

	eval := s.router.EvalForTask(ctx, domain.TaskVerify)
	if eval == nil {
		return chunkedVerifyOutcome{
			RuleCount: len(rules),
			VerifyErr: fmt.Errorf("no active evaluation engine configured"),
		}
	}

	budget := resolveVerifyDiffBudget(ctx, eval, rules)
	chunks := buildChunks(budget)
	if len(chunks) == 0 {
		return chunkedVerifyOutcome{RuleCount: len(rules)}
	}

	out := chunkedVerifyOutcome{
		RuleCount:   len(rules),
		TotalChunks: len(chunks),
	}

	flaggedByID := make(map[int64]domain.PlanViolation)
	chunkFlagged := make(map[int][]domain.Fact)

	for i, ch := range chunks {
		if enforceDeadline && i > 0 {
			if ctx.Err() != nil {
				out.Partial = true
				out.UnverifiedFiles = collectUnverifiedLabels(chunks, i)
				break
			}
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) < stopChunkMinRemainingTime {
				out.Partial = true
				out.UnverifiedFiles = collectUnverifiedLabels(chunks, i)
				break
			}
		}

		evalStart := time.Now()
		viols, verr := eval.VerifyPlan(ctx, ch.Plan, rules)
		s.router.RecordTelemetry(ctx, domain.TaskVerify, eval, evalStart, len(rules), verr)
		if verr != nil {
			isDeadlineErr := errors.Is(verr, context.DeadlineExceeded) || errors.Is(verr, context.Canceled) || ctx.Err() != nil
			if enforceDeadline && i > 0 && isDeadlineErr {
				out.Partial = true
				out.UnverifiedFiles = collectUnverifiedLabels(chunks, i)
				break
			}
			out.CombinedSnippet = combineChunkSnippets(chunks, out.EvaluatedChunks)
			out.VerifyErr = verr
			return out
		}

		out.EvaluatedChunks++
		blocking := filterAndSortViolations(viols, minProb)
		if len(blocking) > 0 {
			chunkRules := make([]domain.Fact, 0, len(blocking))
			for _, b := range blocking {
				chunkRules = append(chunkRules, b.Fact)
				if prev, ok := flaggedByID[b.ID]; !ok || b.Probability > prev.Probability {
					flaggedByID[b.ID] = b
				}
			}
			chunkFlagged[i] = chunkRules
		}
	}

	out.CombinedSnippet = combineChunkSnippets(chunks, out.EvaluatedChunks)
	if len(flaggedByID) == 0 {
		return out
	}

	turnInp := s.activeOverrideTurnInput(ctx, ev, scA)
	if turnInp != "" {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) >= 1200*time.Millisecond {
			ovByID := make(map[int64]domain.PlanViolation)
			ovAllOK := true

			chunkIndices := make([]int, 0, len(chunkFlagged))
			for idx := range chunkFlagged {
				chunkIndices = append(chunkIndices, idx)
			}
			sort.Ints(chunkIndices)

			for _, idx := range chunkIndices {
				if dl, ok := ctx.Deadline(); ok && time.Until(dl) < 1200*time.Millisecond {
					ovAllOK = false
					break
				}
				overridePlan := chunks[idx].Plan + "\nTurn Context (User Prompt - check ONLY whether this instruction explicitly overrides the flagged rules): " + turnInp
				ovStart := time.Now()
				ovViols, ovErr := eval.VerifyPlan(ctx, overridePlan, chunkFlagged[idx])
				s.router.RecordTelemetry(ctx, domain.TaskVerify, eval, ovStart, len(chunkFlagged[idx]), ovErr)
				if ovErr != nil {
					ovAllOK = false
					break
				}
				for _, b := range filterAndSortViolations(ovViols, minProb) {
					if prev, ok := ovByID[b.ID]; !ok || b.Probability > prev.Probability {
						ovByID[b.ID] = b
					}
				}
			}
			if ovAllOK {
				flaggedByID = ovByID
			}
		}
	}

	merged := make([]domain.PlanViolation, 0, len(flaggedByID))
	for _, v := range flaggedByID {
		merged = append(merged, v)
	}
	out.Blocking = filterAndSortViolations(merged, minProb)
	for _, v := range out.Blocking {
		if domain.IsUserPrivateScope(ctx, v.Scope) {
			out.HasPersonalViolation = true
			break
		}
	}
	return out
}
