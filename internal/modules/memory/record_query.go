package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"memorize/internal/domain"
)

const recordSelectColumns = `m.id, m.scope, COALESCE(m.account, ''), m.category, COALESCE(m.sub_category, ''), m.tags, m.content, m.confidence, m.pinned, m.active, m.superseded_by, COALESCE(m.replaces_text, ''), COALESCE(m.created_at, '')`

// ListRecords returns all memory records matching scopeFilter (or all scopes if empty).
func (s *Service) ListRecords(ctx context.Context, scopeFilter string) ([]domain.MemoryRecord, error) {
	sc := strings.TrimSpace(scopeFilter)
	visSQL, visArgs := s.memoryVisibilitySQL(ctx, "m")
	args := append([]any{sc, sc}, visArgs...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordSelectColumns+`
		FROM memories m
		WHERE (? = '' OR m.scope = ?) AND `+visSQL+`
		ORDER BY active DESC, pinned DESC, id DESC`, args...)
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
	strictPersonal := boolToInt(s.enforceStrictPersonalVisibility(ctx))
	curAcc := s.CurrentAccount(ctx)
	placeholders := make([]string, 0, len(out))
	args := make([]any, 0, len(out)+2)
	args = append(args, strictPersonal, curAcc)
	for _, r := range out {
		placeholders = append(placeholders, "?")
		args = append(args, r.ID)
	}
	query := fmt.Sprintf(`SELECT superseded_by, content FROM memories WHERE active = 0 AND (? = 0 OR scope != 'personal' OR COALESCE(account, '') = ?) AND superseded_by IN (%s) ORDER BY id ASC`, strings.Join(placeholders, ","))
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
	visSQL, visArgs := s.memoryVisibilitySQL(ctx, "m")
	var totalAll int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories m WHERE `+visSQL, visArgs...).Scan(&totalAll); err != nil {
		return domain.RecordPage{}, err
	}

	whereSQL, whereArgs := buildRecordWhereClause(q)
	whereSQL = `(` + whereSQL + `) AND ` + visSQL
	whereArgs = append(whereArgs, visArgs...)
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
	visSQL, visArgs := s.memoryVisibilitySQL(ctx, "m")
	whereSQL, whereArgs := buildRecordWhereClause(q)
	whereSQL = `(` + whereSQL + `) AND ` + visSQL
	whereArgs = append(whereArgs, visArgs...)
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
	visSQL, visArgs := s.memoryVisibilitySQL(ctx, "m")
	args = append(args, visArgs...)
	selSQL := fmt.Sprintf(`
		SELECT `+recordSelectColumns+`
		FROM memories m
		WHERE m.id IN (%s) AND `+visSQL+`
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
