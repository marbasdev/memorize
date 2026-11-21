package guardrail

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"praxis/internal/domain"
)

func redactGuardrailText(raw string, maxRunes int) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = domain.RedactSecrets(s)
	runes := []rune(s)
	if maxRunes > 0 && len(runes) > maxRunes {
		s = string(runes[:maxRunes])
	}
	return s
}

func formatGuardrailIDs(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	seen := make(map[int64]bool, len(ids))
	var uniq []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i] < uniq[j] })
	parts := make([]string, len(uniq))
	for i, id := range uniq {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, " ")
}

func parseGuardrailIDs(raw string) []int64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []int64
	seen := map[int64]bool{}
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' }) {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func formatViolationDetails(violations []domain.PlanViolation) ([]int64, string) {
	if len(violations) == 0 {
		return nil, ""
	}
	ids := make([]int64, 0, len(violations))
	var sb strings.Builder
	for i, v := range violations {
		if i > 0 {
			sb.WriteByte('\n')
		}
		ids = append(ids, v.ID)
		fmt.Fprintf(&sb, "[%s %s/%s · %.0f%%] %s", formatViolationRef(v.Fact), v.Scope, v.Category, v.Probability*100, v.Content)
	}
	return ids, sb.String()
}

func (s *Service) RecordGuardrailLog(ctx context.Context, inp domain.GuardrailLogInput) (int64, error) {
	sc := domain.NormalizeScope(inp.Scope)
	acc := domain.NormalizeAccount(inp.Account)
	if domain.IsServerMode(ctx) || acc == "" {
		if s.mem != nil {
			acc = s.mem.CurrentAccount(ctx)
		} else {
			acc = domain.EffectivePrincipalAccount(ctx)
		}
	}
	privInt := 0
	if inp.IsPrivate || domain.IsUserPrivateScope(ctx, sc) {
		privInt = 1
	}
	src := strings.TrimSpace(inp.Source)
	if src == "" {
		src = "guardrail"
	}
	toolName := strings.TrimSpace(inp.ToolName)
	verdict := strings.TrimSpace(inp.Verdict)
	if verdict == "" {
		if inp.Blocked {
			verdict = domain.GuardrailVerdictRuleViolation
		} else {
			verdict = domain.GuardrailVerdictPass
		}
	}
	blockedInt := 0
	if inp.Blocked {
		blockedInt = 1
	}
	ruleCnt := max(0, inp.RuleCount)
	idStr := formatGuardrailIDs(inp.ViolatedIDs)
	planStr := redactGuardrailText(inp.PlanSnippet, 1500)
	detailStr := redactGuardrailText(inp.Details, 2500)
	elapsedMs := inp.ElapsedMs
	if elapsedMs < 0 {
		elapsedMs = 0
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO guardrail_logs (scope, account, is_private, source, tool_name, verdict, blocked, rule_count, violated_ids, plan_snippet, details, elapsed_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc, acc, privInt, src, toolName, verdict, blockedInt, ruleCnt, idStr, planStr, detailStr, elapsedMs)
	if err != nil {
		return 0, err
	}
	newID, _ := res.LastInsertId()
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM guardrail_logs
		WHERE id <= COALESCE((SELECT id FROM guardrail_logs ORDER BY id DESC LIMIT 1 OFFSET 5000), 0)`)
	s.notifyGuardrailsChange()
	return newID, nil
}

func guardrailVisibilitySQL(ctx context.Context, curAcc string) (string, []any) {
	if !domain.IsServerMode(ctx) {
		return "1 = 1", nil
	}
	if !domain.CanAccessAdminFeatures(ctx) {
		return `account = ?`, []any{curAcc}
	}
	return `(is_private = 0 OR account = ?)`, []any{curAcc}
}

func buildGuardrailWhereClause(q domain.GuardrailLogQuery) (string, []any) {
	var conds []string
	var args []any

	if sc := strings.TrimSpace(q.Scope); sc != "" {
		conds = append(conds, `scope = ?`)
		args = append(args, sc)
	}
	if acc := strings.TrimSpace(q.Account); acc != "" {
		conds = append(conds, `LOWER(COALESCE(account, '')) = LOWER(?)`)
		args = append(args, acc)
	}
	switch strings.ToLower(strings.TrimSpace(q.State)) {
	case "blocked", "violation", "denied":
		conds = append(conds, `blocked = 1`)
	case "pass", "passed", "ok":
		conds = append(conds, `blocked = 0`)
	}
	if s := strings.ToLower(strings.TrimSpace(q.Source)); s != "" && s != "all" {
		switch s {
		case "pretool", "stop":
			phase := domain.HookPhasePreTool
			if s == "stop" {
				phase = domain.HookPhaseStop
			}
			modes := domain.ModesForPhase(phase)
			ph := make([]string, len(modes))
			for i, m := range modes {
				ph[i] = "?"
				args = append(args, m)
			}
			conds = append(conds, `source IN (`+strings.Join(ph, ", ")+`)`)
		case "git-precommit", "git":
			conds = append(conds, `source = ?`)
			args = append(args, domain.ModeForAgentPhase("git", domain.HookPhasePreCommit))
		case "verify":
			conds = append(conds, `source IN ('mcp:verify_plan', 'ui:verify')`)
		default:
			conds = append(conds, `source = ?`)
			args = append(args, strings.TrimSpace(q.Source))
		}
	}

	if len(conds) == 0 {
		return "1 = 1", nil
	}
	return strings.Join(conds, " AND "), args
}

func (s *Service) QueryGuardrailLogsPage(ctx context.Context, q domain.GuardrailLogQuery) (domain.GuardrailLogPage, error) {
	curAcc := ""
	if s.mem != nil {
		curAcc = s.mem.CurrentAccount(ctx)
	} else {
		curAcc = domain.EffectivePrincipalAccount(ctx)
	}
	visSQL, visArgs := guardrailVisibilitySQL(ctx, curAcc)
	sc := strings.TrimSpace(q.Scope)
	var totalCnt, blockedCnt, passCnt int
	totArgs := append([]any{sc, sc}, visArgs...)
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN blocked = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN blocked = 0 THEN 1 ELSE 0 END), 0)
		FROM guardrail_logs
		WHERE (? = '' OR scope = ?) AND `+visSQL, totArgs...).Scan(&totalCnt, &blockedCnt, &passCnt); err != nil {
		return domain.GuardrailLogPage{}, err
	}

	whereSQL, whereArgs := buildGuardrailWhereClause(q)
	whereSQL = `(` + whereSQL + `) AND ` + visSQL
	whereArgs = append(whereArgs, visArgs...)
	var filteredCnt int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM guardrail_logs WHERE `+whereSQL, whereArgs...).Scan(&filteredCnt); err != nil {
		return domain.GuardrailLogPage{}, err
	}

	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > 200 {
		pageSize = 200
	}
	totalPages := 1
	if filteredCnt > 0 {
		totalPages = (filteredCnt + pageSize - 1) / pageSize
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize

	out := domain.GuardrailLogPage{
		Records:       []domain.GuardrailLogRecord{},
		TotalCount:    totalCnt,
		BlockedCount:  blockedCnt,
		PassCount:     passCnt,
		FilteredCount: filteredCnt,
		Page:          page,
		PageSize:      pageSize,
		TotalPages:    totalPages,
	}
	if filteredCnt == 0 || offset >= filteredCnt {
		return out, nil
	}

	selArgs := append(append([]any{}, whereArgs...), pageSize, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, COALESCE(account, ''), COALESCE(is_private, 0), source, COALESCE(tool_name, ''), verdict, blocked, rule_count,
		       COALESCE(violated_ids, ''), COALESCE(plan_snippet, ''), COALESCE(details, ''),
		       COALESCE(elapsed_ms, 0), COALESCE(created_at, '')
		FROM guardrail_logs
		WHERE `+whereSQL+`
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, selArgs...)
	if err != nil {
		return domain.GuardrailLogPage{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var r domain.GuardrailLogRecord
		var blk, privInt int
		var rawIDs string
		if err := rows.Scan(&r.ID, &r.Scope, &r.Account, &privInt, &r.Source, &r.ToolName, &r.Verdict, &blk, &r.RuleCount, &rawIDs, &r.PlanSnippet, &r.Details, &r.ElapsedMs, &r.CreatedAt); err != nil {
			return domain.GuardrailLogPage{}, err
		}
		r.Blocked = blk != 0
		r.IsPrivate = privInt != 0
		r.ViolatedIDs = parseGuardrailIDs(rawIDs)
		out.Records = append(out.Records, r)
	}
	if err := rows.Err(); err != nil {
		return domain.GuardrailLogPage{}, err
	}
	return out, nil
}

func (s *Service) SuggestRuleRefinements(ctx context.Context, rawScope string) ([]domain.GuardrailRuleRefinement, string, error) {
	if s == nil || s.db == nil || s.mem == nil {
		return nil, "none", fmt.Errorf("guardrail service unavailable")
	}
	sc := strings.TrimSpace(rawScope)
	if sc != "" {
		sc = domain.NormalizeScope(sc)
	}
	curAcc := s.mem.CurrentAccount(ctx)
	visSQL, visArgs := guardrailVisibilitySQL(ctx, curAcc)
	qArgs := append([]any{sc, sc}, visArgs...)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, violated_ids, COALESCE(plan_snippet, ''), COALESCE(created_at, '')
		FROM guardrail_logs
		WHERE blocked = 1 AND COALESCE(violated_ids, '') != '' AND (? = '' OR scope = ?) AND `+visSQL+`
		ORDER BY id DESC
		LIMIT 300`, qArgs...)
	if err != nil {
		return nil, "none", err
	}
	defer rows.Close()

	type frictionAgg struct {
		hitCount    int
		lastLogID   int64
		lastBlocked string
		snippets    []string
		seenSnip    map[string]bool
	}
	aggByID := make(map[int64]*frictionAgg)
	for rows.Next() {
		var logID int64
		var rawIDs, snippet, createdAt string
		if err := rows.Scan(&logID, &rawIDs, &snippet, &createdAt); err != nil {
			return nil, "none", err
		}
		snip := strings.TrimSpace(domain.TruncateUTF8Bytes(strings.TrimSpace(snippet), 320))
		for _, rid := range parseGuardrailIDs(rawIDs) {
			a := aggByID[rid]
			if a == nil {
				a = &frictionAgg{
					lastLogID:   logID,
					lastBlocked: createdAt,
					seenSnip:    make(map[string]bool),
				}
				aggByID[rid] = a
			}
			a.hitCount++
			if snip != "" && len(a.snippets) < 4 && !a.seenSnip[snip] {
				a.seenSnip[snip] = true
				a.snippets = append(a.snippets, snip)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "none", err
	}

	engineLabel := "none"
	if s.router != nil {
		engineLabel = s.router.ProviderNameForTask(ctx, domain.TaskConsolidate)
	}
	if len(aggByID) == 0 {
		return nil, engineLabel, nil
	}

	allIDs := make([]int64, 0, len(aggByID))
	for id := range aggByID {
		allIDs = append(allIDs, id)
	}
	recs, err := s.mem.GetRecordsByIDs(ctx, allIDs)
	if err != nil {
		return nil, engineLabel, err
	}

	type rankedFriction struct {
		item      domain.GuardrailRuleFriction
		lastLogID int64
	}
	var ranked []rankedFriction
	for _, r := range recs {
		if !r.Active || domain.CanModifyScope(ctx, r.Scope) != nil {
			continue
		}
		a := aggByID[r.ID]
		if a == nil || a.hitCount <= 0 {
			continue
		}
		ranked = append(ranked, rankedFriction{
			lastLogID: a.lastLogID,
			item: domain.GuardrailRuleFriction{
				Rule: domain.Fact{
					ID:          r.ID,
					Scope:       r.Scope,
					Category:    r.Category,
					SubCategory: r.SubCategory,
					Content:     r.Content,
					Pinned:      r.Pinned,
				},
				HitCount:    a.hitCount,
				Snippets:    a.snippets,
				LastBlocked: a.lastBlocked,
			},
		})
	}
	if len(ranked) == 0 {
		return nil, engineLabel, nil
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].item.HitCount != ranked[j].item.HitCount {
			return ranked[i].item.HitCount > ranked[j].item.HitCount
		}
		return ranked[i].lastLogID > ranked[j].lastLogID
	})
	if len(ranked) > 12 {
		ranked = ranked[:12]
	}
	frictions := make([]domain.GuardrailRuleFriction, len(ranked))
	for i, rf := range ranked {
		frictions[i] = rf.item
	}

	if s.router == nil {
		return nil, "none", fmt.Errorf("no inference router configured")
	}
	ev := s.router.EvalForTask(ctx, domain.TaskConsolidate)
	if ev == nil {
		return nil, "none", fmt.Errorf("no active inference engine configured")
	}
	refiner, ok := ev.(domain.GuardrailRuleRefiner)
	if !ok {
		return nil, ev.Name(), fmt.Errorf("active engine (%s) does not support generative rule refinement", ev.Name())
	}

	scopeLabel := sc
	if scopeLabel == "" {
		scopeLabel = "all-scopes"
	}
	evalStart := time.Now()
	suggested, evalErr := refiner.SuggestGuardrailRefinements(ctx, scopeLabel, frictions)
	s.router.RecordTelemetry(ctx, domain.TaskConsolidate, ev, evalStart, len(frictions), evalErr)
	if evalErr != nil {
		return nil, ev.Name(), evalErr
	}

	out := make([]domain.GuardrailRuleRefinement, 0, len(suggested))
	for _, it := range suggested {
		if domain.CheckSecurityGuard(it.RefinedContent) != "" {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HitCount != out[j].HitCount {
			return out[i].HitCount > out[j].HitCount
		}
		return out[i].ID < out[j].ID
	})
	return out, ev.Name(), nil
}

func (s *Service) ApplyRuleRefinements(ctx context.Context, items []domain.GuardrailRuleRefinement) (int, error) {
	if s == nil || s.mem == nil {
		return 0, fmt.Errorf("guardrail service unavailable")
	}
	updated := 0
	for _, it := range items {
		txt := strings.TrimSpace(it.RefinedContent)
		if it.ID <= 0 || txt == "" {
			continue
		}
		if err := s.mem.UpdateMemoryFull(ctx, it.ID, "", "", nil, txt, nil); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}
