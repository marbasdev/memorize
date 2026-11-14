package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

// ProviderName returns the default resolved engine identifier or "none".
func (s *Service) ProviderName() string {
	return s.ProviderNameForTask(context.Background(), "")
}

// ProviderNameForTask returns the effective resolved engine name for a specific feature task.
func (s *Service) ProviderNameForTask(ctx context.Context, task domain.EvalTask) string {
	return s.router.ProviderNameForTask(ctx, task)
}

// GetConfigMap returns all key/value entries from SQLite config.
func (s *Service) GetConfigMap(ctx context.Context) map[string]string {
	return config.GetConfigMap(ctx, s.db)
}

// SaveConfig persists configuration key/value entries into SQLite config with 0600 permissions.
func (s *Service) SaveConfig(ctx context.Context, pairs map[string]string) error {
	return config.SaveConfig(ctx, s.db, s.dbPath, pairs)
}

// GetThresholds reads the persistent admission and retrieval gate thresholds with safe defaults.
func (s *Service) GetThresholds(ctx context.Context) domain.Thresholds {
	th := domain.DefaultThresholds()
	rows, err := s.db.QueryContext(ctx, `SELECT key, val FROM settings`)
	if err != nil {
		return th
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v float64
		if rows.Scan(&k, &v) == nil {
			switch k {
			case "min_permanent":
				th.MinPermanent = v
			case "min_confidence":
				th.MinConfidence = v
			case "pin_invariant":
				th.PinInvariant = v
			case "min_relevance":
				th.MinRelevance = v
			case "min_rel_confidence":
				th.MinRelConfidence = v
			}
		}
	}
	return th
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SaveThresholds validates, clamps, and persists admission and retrieval thresholds into SQLite.
func (s *Service) SaveThresholds(ctx context.Context, th domain.Thresholds) (domain.Thresholds, error) {
	th.MinPermanent = clamp(th.MinPermanent, 0.10, 0.99)
	th.MinConfidence = clamp(th.MinConfidence, 0.10, 0.99)
	th.PinInvariant = clamp(th.PinInvariant, 0.50, 0.99)
	th.MinRelevance = clamp(th.MinRelevance, 0.50, 5.00)
	th.MinRelConfidence = clamp(th.MinRelConfidence, 0.10, 0.99)

	pairs := map[string]float64{
		"min_permanent":      th.MinPermanent,
		"min_confidence":     th.MinConfidence,
		"pin_invariant":      th.PinInvariant,
		"min_relevance":      th.MinRelevance,
		"min_rel_confidence": th.MinRelConfidence,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return th, err
	}
	defer tx.Rollback()
	for k, v := range pairs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key, val) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET val = excluded.val`,
			k, v); err != nil {
			return th, err
		}
	}
	return th, tx.Commit()
}

const recordSelectColumns = `m.id, m.scope, COALESCE(m.account, ''), m.category, COALESCE(m.sub_category, ''), m.tags, m.content, m.confidence, m.pinned, m.active, m.superseded_by, COALESCE(m.replaces_text, ''), COALESCE(m.created_at, '')`

// ListRecords returns all memory records matching scopeFilter (or all scopes if empty).
func (s *Service) ListRecords(ctx context.Context, scopeFilter string) ([]domain.MemoryRecord, error) {
	sc := strings.TrimSpace(scopeFilter)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordSelectColumns+`
		FROM memories m
		WHERE (? = '' OR m.scope = ?)
		ORDER BY active DESC, pinned DESC, id DESC`, sc, sc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, err := scanMemoryRecordRows(rows)
	if err != nil {
		return nil, err
	}
	s.attachRecordReplaces(ctx, out)
	return out, nil
}

// GetDashboardStats computes global active/pinned/tomb counts and confidence histogram bins.
func (s *Service) GetDashboardStats(ctx context.Context) (domain.DashboardStats, error) {
	return s.GetDashboardStatsForScope(ctx, "")
}

// GetDashboardStatsForScope computes memory KPI counts and confidence bins filtered by scope.
func (s *Service) GetDashboardStatsForScope(ctx context.Context, scopeFilter string) (domain.DashboardStats, error) {
	sc := strings.TrimSpace(scopeFilter)
	st := domain.DashboardStats{ConfidenceBins: make([]int, 5)}
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN active = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND pinned = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND confidence < 0.80 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND confidence >= 0.80 AND confidence < 0.85 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND confidence >= 0.85 AND confidence < 0.90 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND confidence >= 0.90 AND confidence < 0.95 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND confidence >= 0.95 THEN 1 ELSE 0 END), 0)
		FROM memories
		WHERE (? = '' OR scope = ?)
	`, sc, sc).Scan(
		&st.TotalCount, &st.ActiveCount, &st.PinnedCount, &st.TombCount,
		&st.ConfidenceBins[0], &st.ConfidenceBins[1], &st.ConfidenceBins[2], &st.ConfidenceBins[3], &st.ConfidenceBins[4],
	)
	return st, err
}

// GetDashboardFacets collects sorted distinct scopes, accounts, categories, sub-categories, and recent sessions.
func (s *Service) GetDashboardFacets(ctx context.Context) (domain.DashboardFacets, error) {
	scopeSet := map[string]bool{"global": true, "personal": true}
	if rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT scope FROM memories WHERE COALESCE(scope, '') != ''
		UNION
		SELECT DISTINCT scope FROM project_progress WHERE COALESCE(scope, '') != ''
		UNION
		SELECT DISTINCT scope FROM guardrail_logs WHERE COALESCE(scope, '') != ''`); err == nil {
		for rows.Next() {
			var sc string
			if rows.Scan(&sc) == nil && strings.TrimSpace(sc) != "" {
				scopeSet[sc] = true
			}
		}
		_ = rows.Close()
	}

	accSet := map[string]bool{}
	if curAcc := s.CurrentAccount(ctx); curAcc != "" {
		accSet[curAcc] = true
	}
	if rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account FROM memories WHERE COALESCE(account, '') != ''`); err == nil {
		for rows.Next() {
			var ac string
			if rows.Scan(&ac) == nil && strings.TrimSpace(ac) != "" {
				accSet[ac] = true
			}
		}
		_ = rows.Close()
	}

	catSet := map[string]bool{}
	subMap := map[string]map[string]bool{}
	if rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT category, COALESCE(sub_category, '') FROM memories WHERE COALESCE(category, '') != ''`); err == nil {
		for rows.Next() {
			var cat, sub string
			if rows.Scan(&cat, &sub) == nil {
				c := strings.TrimSpace(cat)
				sc := strings.TrimSpace(sub)
				if c != "" {
					catSet[c] = true
					cKey := strings.ToLower(c)
					if sc != "" {
						if subMap[cKey] == nil {
							subMap[cKey] = map[string]bool{}
						}
						subMap[cKey][sc] = true
					}
				}
			}
		}
		_ = rows.Close()
	}

	scopes := make([]string, 0, len(scopeSet))
	for k := range scopeSet {
		scopes = append(scopes, k)
	}
	sort.Strings(scopes)

	accounts := make([]string, 0, len(accSet))
	for k := range accSet {
		accounts = append(accounts, k)
	}
	sort.Strings(accounts)

	categories := make([]string, 0, len(catSet))
	for k := range catSet {
		categories = append(categories, k)
	}
	sort.Strings(categories)

	subOut := make(map[string][]string, len(subMap))
	for cKey, m := range subMap {
		list := make([]string, 0, len(m))
		for sVal := range m {
			list = append(list, sVal)
		}
		sort.Strings(list)
		subOut[cKey] = list
	}

	sessions := make([]domain.SessionSummary, 0)
	if rows, err := s.db.QueryContext(ctx, `
		SELECT
			ss.session_id,
			COALESCE(
				NULLIF((SELECT ts.scope FROM turn_state ts WHERE ts.session_id = ss.session_id ORDER BY ts.updated_at DESC LIMIT 1), ''),
				MAX(CASE WHEN m.scope LIKE 'project:%' THEN m.scope END),
				MAX(m.scope),
				''
			),
			COUNT(DISTINCT CASE WHEN m.active = 1 THEN ss.memory_id END),
			COUNT(DISTINCT ss.memory_id),
			COALESCE(MAX(ss.updated_at), '')
		FROM session_sent ss
		JOIN memories m ON m.id = ss.memory_id
		WHERE COALESCE(ss.session_id, '') != ''
		GROUP BY ss.session_id
		ORDER BY MAX(ss.updated_at) DESC, ss.session_id ASC
		LIMIT 50
	`); err == nil {
		for rows.Next() {
			var sid, sc, updatedAt string
			var activeCnt, totalCnt int
			if rows.Scan(&sid, &sc, &activeCnt, &totalCnt, &updatedAt) == nil && strings.TrimSpace(sid) != "" && totalCnt > 0 {
				cnt := activeCnt
				if cnt == 0 {
					cnt = totalCnt
				}
				sessions = append(sessions, domain.SessionSummary{
					SessionID: sid,
					Scope:     sc,
					Count:     cnt,
					UpdatedAt: updatedAt,
				})
			}
		}
		_ = rows.Close()
	}

	return domain.DashboardFacets{
		Scopes:        scopes,
		Accounts:      accounts,
		Categories:    categories,
		SubCategories: subOut,
		Sessions:      sessions,
	}, nil
}

func escapeSQLLike(raw string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(raw)
}

func buildRecordWhereClause(q domain.RecordQuery) (string, []any) {
	var conds []string
	var args []any

	if sc := strings.TrimSpace(q.Scope); sc != "" {
		conds = append(conds, `m.scope = ?`)
		args = append(args, sc)
	}
	if sid := strings.TrimSpace(q.SessionID); sid != "" {
		conds = append(conds, `EXISTS (SELECT 1 FROM session_sent ss WHERE ss.session_id = ? AND ss.memory_id = m.id)`)
		args = append(args, sid)
	}
	if acc := strings.TrimSpace(q.Account); acc != "" {
		conds = append(conds, `LOWER(COALESCE(m.account, '')) = LOWER(?)`)
		args = append(args, acc)
	}
	if cat := strings.TrimSpace(q.Category); cat != "" {
		conds = append(conds, `LOWER(m.category) = LOWER(?)`)
		args = append(args, cat)
	}
	if sub := strings.TrimSpace(q.SubCategory); sub != "" {
		if strings.EqualFold(sub, domain.UncategorizedSubKey) {
			conds = append(conds, `TRIM(COALESCE(m.sub_category, '')) = ''`)
		} else {
			conds = append(conds, `LOWER(COALESCE(m.sub_category, '')) = LOWER(?)`)
			args = append(args, sub)
		}
	}
	switch strings.ToLower(strings.TrimSpace(q.State)) {
	case "active":
		conds = append(conds, `m.active = 1`)
	case "pinned":
		conds = append(conds, `m.active = 1 AND m.pinned = 1`)
	case "general", "unpinned":
		conds = append(conds, `m.active = 1 AND m.pinned = 0`)
	case "tomb":
		conds = append(conds, `m.active = 0`)
	}

	for _, rawTok := range strings.Fields(strings.TrimSpace(q.Search)) {
		tok := strings.ToLower(rawTok)
		if strings.HasPrefix(tok, "#") && len(tok) > 1 {
			if idVal, err := strconv.ParseInt(tok[1:], 10, 64); err == nil {
				conds = append(conds, `(m.id = ? OR m.superseded_by = ?)`)
				args = append(args, idVal, idVal)
				continue
			}
			tagPat := "%" + escapeSQLLike(tok[1:]) + "%"
			litPat := "%" + escapeSQLLike(tok) + "%"
			conds = append(conds, `(LOWER(COALESCE(m.tags, '')) LIKE ? ESCAPE '\' OR LOWER(m.content) LIKE ? ESCAPE '\')`)
			args = append(args, tagPat, litPat)
			continue
		}
		if strings.HasPrefix(tok, "id:") {
			if idVal, err := strconv.ParseInt(strings.TrimPrefix(tok, "id:"), 10, 64); err == nil {
				conds = append(conds, `(m.id = ? OR m.superseded_by = ?)`)
				args = append(args, idVal, idVal)
			} else {
				conds = append(conds, `1 = 0`)
			}
			continue
		}
		if strings.HasPrefix(tok, "scope:") {
			if val := strings.TrimPrefix(tok, "scope:"); val != "" {
				conds = append(conds, `LOWER(m.scope) LIKE ? ESCAPE '\'`)
				args = append(args, "%"+escapeSQLLike(val)+"%")
			}
			continue
		}
		if strings.HasPrefix(tok, "session:") {
			if val := strings.TrimPrefix(tok, "session:"); val != "" {
				conds = append(conds, `EXISTS (SELECT 1 FROM session_sent ss WHERE LOWER(ss.session_id) LIKE ? ESCAPE '\' AND ss.memory_id = m.id)`)
				args = append(args, "%"+escapeSQLLike(val)+"%")
			}
			continue
		}
		if strings.HasPrefix(tok, "account:") || strings.HasPrefix(tok, "user:") || (strings.HasPrefix(tok, "@") && len(tok) > 1) {
			val := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(tok, "account:"), "user:"), "@")
			if val != "" {
				conds = append(conds, `LOWER(COALESCE(m.account, '')) LIKE ? ESCAPE '\'`)
				args = append(args, "%"+escapeSQLLike(val)+"%")
			}
			continue
		}
		if strings.HasPrefix(tok, "cat:") || strings.HasPrefix(tok, "category:") {
			val := strings.TrimPrefix(strings.TrimPrefix(tok, "category:"), "cat:")
			if val != "" {
				pat := "%" + escapeSQLLike(val) + "%"
				conds = append(conds, `(LOWER(m.category) LIKE ? ESCAPE '\' OR LOWER(m.category || '/' || COALESCE(m.sub_category, '')) LIKE ? ESCAPE '\')`)
				args = append(args, pat, pat)
			}
			continue
		}
		if strings.HasPrefix(tok, "sub:") || strings.HasPrefix(tok, "subcat:") {
			val := strings.TrimPrefix(strings.TrimPrefix(tok, "subcat:"), "sub:")
			if val != "" {
				conds = append(conds, `LOWER(COALESCE(m.sub_category, '')) LIKE ? ESCAPE '\'`)
				args = append(args, "%"+escapeSQLLike(val)+"%")
			}
			continue
		}
		if strings.HasPrefix(tok, "tag:") {
			if val := strings.TrimPrefix(tok, "tag:"); val != "" {
				conds = append(conds, `LOWER(COALESCE(m.tags, '')) LIKE ? ESCAPE '\'`)
				args = append(args, "%"+escapeSQLLike(val)+"%")
			}
			continue
		}
		pat := "%" + escapeSQLLike(tok) + "%"
		conds = append(conds, `(
			CAST(m.id AS TEXT) LIKE ? ESCAPE '\'
			OR LOWER(m.scope) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(m.account, '')) LIKE ? ESCAPE '\'
			OR LOWER(m.category) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(m.sub_category, '')) LIKE ? ESCAPE '\'
			OR LOWER(m.category || '/' || COALESCE(m.sub_category, '')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(m.tags, '')) LIKE ? ESCAPE '\'
			OR LOWER(m.content) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(m.replaces_text, '')) LIKE ? ESCAPE '\'
			OR EXISTS (
				SELECT 1 FROM memories child
				WHERE child.active = 0 AND child.superseded_by = m.id
				  AND LOWER(child.content) LIKE ? ESCAPE '\'
			)
		)`)
		args = append(args, pat, pat, pat, pat, pat, pat, pat, pat, pat, pat)
	}

	if len(conds) == 0 {
		return "1 = 1", nil
	}
	return strings.Join(conds, " AND "), args
}

func scanMemoryRecordRows(rows *sql.Rows) ([]domain.MemoryRecord, error) {
	out := []domain.MemoryRecord{}
	for rows.Next() {
		var r domain.MemoryRecord
		var tagStr, repText string
		var pin, act int
		var sup sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Scope, &r.Account, &r.Category, &r.SubCategory, &tagStr, &r.Content, &r.Confidence, &pin, &act, &sup, &repText, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Tags = parseTagStr(tagStr)
		r.Pinned = pin == 1
		r.Active = act == 1
		if sup.Valid {
			v := sup.Int64
			r.SupersededBy = &v
		}
		for _, line := range strings.Split(repText, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				r.Replaces = append(r.Replaces, line)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) attachRecordReplaces(ctx context.Context, out []domain.MemoryRecord) {
	if len(out) == 0 {
		return
	}
	placeholders := make([]string, 0, len(out))
	args := make([]any, 0, len(out))
	for _, r := range out {
		placeholders = append(placeholders, "?")
		args = append(args, r.ID)
	}
	query := fmt.Sprintf(`SELECT superseded_by, content FROM memories WHERE active = 0 AND superseded_by IN (%s) ORDER BY id ASC`, strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	childByParent := make(map[int64][]string)
	for rows.Next() {
		var pid int64
		var c string
		if rows.Scan(&pid, &c) == nil {
			childByParent[pid] = append(childByParent[pid], c)
		}
	}
	for i := range out {
		seen := make(map[string]bool, len(out[i].Replaces))
		for _, ex := range out[i].Replaces {
			seen[ex] = true
		}
		for _, c := range childByParent[out[i].ID] {
			if !seen[c] {
				seen[c] = true
				out[i].Replaces = append(out[i].Replaces, c)
			}
		}
	}
}

// QueryRecordsPage returns a paginated, filtered slice of memory records for the dashboard table.
func (s *Service) QueryRecordsPage(ctx context.Context, q domain.RecordQuery) (domain.RecordPage, error) {
	var totalAll int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&totalAll); err != nil {
		return domain.RecordPage{}, err
	}

	whereSQL, whereArgs := buildRecordWhereClause(q)
	var totalFiltered, totalFilteredActive int
	countSQL := `SELECT COUNT(*), COALESCE(SUM(CASE WHEN m.active = 1 THEN 1 ELSE 0 END), 0) FROM memories m WHERE ` + whereSQL
	if err := s.db.QueryRowContext(ctx, countSQL, whereArgs...).Scan(&totalFiltered, &totalFilteredActive); err != nil {
		return domain.RecordPage{}, err
	}

	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 500 {
		pageSize = 500
	}
	totalPages := 1
	if totalFiltered > 0 {
		totalPages = (totalFiltered + pageSize - 1) / pageSize
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize

	if totalFiltered == 0 || offset >= totalFiltered {
		return domain.RecordPage{
			Records:             []domain.MemoryRecord{},
			TotalRecords:        totalAll,
			FilteredRecords:     totalFiltered,
			FilteredActiveCount: totalFilteredActive,
			Page:                page,
			PageSize:            pageSize,
			TotalPages:          totalPages,
		}, nil
	}

	selSQL := `
		SELECT ` + recordSelectColumns + `
		FROM memories m
		WHERE ` + whereSQL + `
		ORDER BY m.id DESC
		LIMIT ? OFFSET ?`
	pageArgs := append(append([]any{}, whereArgs...), pageSize, offset)
	rows, err := s.db.QueryContext(ctx, selSQL, pageArgs...)
	if err != nil {
		return domain.RecordPage{}, err
	}
	defer rows.Close()

	out, err := scanMemoryRecordRows(rows)
	if err != nil {
		return domain.RecordPage{}, err
	}
	s.attachRecordReplaces(ctx, out)

	return domain.RecordPage{
		Records:             out,
		TotalRecords:        totalAll,
		FilteredRecords:     totalFiltered,
		FilteredActiveCount: totalFilteredActive,
		Page:                page,
		PageSize:            pageSize,
		TotalPages:          totalPages,
	}, nil
}

// QueryMatchingActiveIDs returns active memory IDs matching q up to limit.
func (s *Service) QueryMatchingActiveIDs(ctx context.Context, q domain.RecordQuery, limit int) ([]int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	whereSQL, whereArgs := buildRecordWhereClause(q)
	selSQL := `SELECT m.id FROM memories m WHERE (` + whereSQL + `) AND m.active = 1 ORDER BY m.id DESC LIMIT ?`
	args := append(append([]any{}, whereArgs...), limit)
	rows, err := s.db.QueryContext(ctx, selSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// GetRecordsByIDs fetches full MemoryRecord details for up to 500 memory IDs.
func (s *Service) GetRecordsByIDs(ctx context.Context, ids []int64) ([]domain.MemoryRecord, error) {
	if len(ids) == 0 {
		return []domain.MemoryRecord{}, nil
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			ph = append(ph, "?")
			args = append(args, id)
		}
	}
	if len(ph) == 0 {
		return []domain.MemoryRecord{}, nil
	}
	selSQL := fmt.Sprintf(`
		SELECT `+recordSelectColumns+`
		FROM memories m
		WHERE m.id IN (%s)
		ORDER BY m.id ASC`, strings.Join(ph, ","))
	rows, err := s.db.QueryContext(ctx, selSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMemoryRecordRows(rows)
	if err != nil {
		return nil, err
	}
	s.attachRecordReplaces(ctx, out)
	return out, nil
}

// DiagnoseRelevance runs a non-session diagnostic relevance probe against active memories.
func (s *Service) DiagnoseRelevance(ctx context.Context, rawScope, query string) (domain.ProbeResult, error) {
	t0 := time.Now()
	_, _, ev, pinned, scored, err := s.evaluateRelevance(ctx, rawScope, "", query)
	if err != nil {
		return domain.ProbeResult{}, err
	}
	provName := "none"
	if ev != nil {
		provName = ev.Name()
	}
	return domain.ProbeResult{
		Provider:   provName,
		ElapsedMs:  domain.ElapsedMsSince(t0),
		Thresholds: s.GetThresholds(ctx),
		Pinned:     pinned,
		Scored:     scored,
	}, nil
}
