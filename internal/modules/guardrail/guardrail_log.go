package guardrail

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"memorize/internal/domain"
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
		fmt.Fprintf(&sb, "[#%d %s/%s · %.0f%%] %s", v.ID, v.Scope, v.Category, v.Probability*100, v.Content)
	}
	return ids, sb.String()
}

// RecordGuardrailLog appends an audit entry for a guardrail or plan verification evaluation.
func (s *Service) RecordGuardrailLog(ctx context.Context, inp domain.GuardrailLogInput) (int64, error) {
	sc := domain.NormalizeScope(inp.Scope)
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
		INSERT INTO guardrail_logs (scope, source, tool_name, verdict, blocked, rule_count, violated_ids, plan_snippet, details, elapsed_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc, src, toolName, verdict, blockedInt, ruleCnt, idStr, planStr, detailStr, elapsedMs)
	if err != nil {
		return 0, err
	}
	newID, _ := res.LastInsertId()
	// Keep audit table bounded to the latest 5000 entries.
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM guardrail_logs
		WHERE id <= COALESCE((SELECT id FROM guardrail_logs ORDER BY id DESC LIMIT 1 OFFSET 5000), 0)`)
	s.notifyGuardrailsChange()
	return newID, nil
}

func buildGuardrailWhereClause(q domain.GuardrailLogQuery) (string, []any) {
	var conds []string
	var args []any

	if sc := strings.TrimSpace(q.Scope); sc != "" {
		conds = append(conds, `scope = ?`)
		args = append(args, sc)
	}
	switch strings.ToLower(strings.TrimSpace(q.State)) {
	case "blocked", "violation", "denied":
		conds = append(conds, `blocked = 1`)
	case "pass", "passed", "ok":
		conds = append(conds, `blocked = 0`)
	}
	switch strings.ToLower(strings.TrimSpace(q.Source)) {
	case "pretool":
		conds = append(conds, `source IN ('antigravity-pretool', 'claude-pretool')`)
	case "git-precommit", "git":
		conds = append(conds, `source = 'git-precommit'`)
	case "verify":
		conds = append(conds, `source IN ('mcp:verify_plan', 'ui:verify')`)
	default:
		if s := strings.TrimSpace(q.Source); s != "" && s != "all" {
			conds = append(conds, `source = ?`)
			args = append(args, s)
		}
	}

	if len(conds) == 0 {
		return "1 = 1", nil
	}
	return strings.Join(conds, " AND "), args
}

// QueryGuardrailLogsPage performs SQLite server-side filtering and LIMIT/OFFSET pagination over guardrail_logs.
func (s *Service) QueryGuardrailLogsPage(ctx context.Context, q domain.GuardrailLogQuery) (domain.GuardrailLogPage, error) {
	sc := strings.TrimSpace(q.Scope)
	var totalCnt, blockedCnt, passCnt int
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN blocked = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN blocked = 0 THEN 1 ELSE 0 END), 0)
		FROM guardrail_logs
		WHERE (? = '' OR scope = ?)`, sc, sc).Scan(&totalCnt, &blockedCnt, &passCnt); err != nil {
		return domain.GuardrailLogPage{}, err
	}

	whereSQL, whereArgs := buildGuardrailWhereClause(q)
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
		SELECT id, scope, source, COALESCE(tool_name, ''), verdict, blocked, rule_count,
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
		var blk int
		var rawIDs string
		if err := rows.Scan(&r.ID, &r.Scope, &r.Source, &r.ToolName, &r.Verdict, &blk, &r.RuleCount, &rawIDs, &r.PlanSnippet, &r.Details, &r.ElapsedMs, &r.CreatedAt); err != nil {
			return domain.GuardrailLogPage{}, err
		}
		r.Blocked = blk != 0
		r.ViolatedIDs = parseGuardrailIDs(rawIDs)
		out.Records = append(out.Records, r)
	}
	if err := rows.Err(); err != nil {
		return domain.GuardrailLogPage{}, err
	}
	return out, nil
}
