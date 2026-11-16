package memory

import (
	"context"
	"database/sql"
	"sort"
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

// SaveThresholds validates, clamps, and persists admission and retrieval thresholds into SQLite.
func (s *Service) SaveThresholds(ctx context.Context, th domain.Thresholds) (domain.Thresholds, error) {
	th.MinPermanent = domain.Clamp(th.MinPermanent, 0.10, 0.99)
	th.MinConfidence = domain.Clamp(th.MinConfidence, 0.10, 0.99)
	th.PinInvariant = domain.Clamp(th.PinInvariant, 0.50, 0.99)
	th.MinRelevance = domain.Clamp(th.MinRelevance, 0.50, 5.00)
	th.MinRelConfidence = domain.Clamp(th.MinRelConfidence, 0.10, 0.99)

	pairs := map[string]float64{
		"min_permanent":      th.MinPermanent,
		"min_confidence":     th.MinConfidence,
		"pin_invariant":      th.PinInvariant,
		"min_relevance":      th.MinRelevance,
		"min_rel_confidence": th.MinRelConfidence,
	}
	return th, s.withTx(ctx, func(tx *sql.Tx) error {
		for k, v := range pairs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings(key, val) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET val = excluded.val`,
				k, v); err != nil {
				return err
			}
		}
		return nil
	})
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
		SELECT DISTINCT scope FROM guardrail_logs WHERE COALESCE(scope, '') != ''
		UNION
		SELECT DISTINCT scope FROM team_rule_states WHERE COALESCE(scope, '') != ''
		UNION
		SELECT DISTINCT scope FROM team_rule_sync WHERE COALESCE(scope, '') != ''`); err == nil {
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
