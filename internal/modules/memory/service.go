package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"praxis/internal/domain"
	"praxis/internal/inference/router"
	"praxis/internal/infra/config"
	"praxis/internal/infra/eventbus"
	"praxis/internal/infra/sqlite"
)

const (
	maxCachedMemorySessions = 256
	memorySessionIdleTTL    = 6 * time.Hour
	transientSweepInterval  = 30 * time.Minute
)

type MutationEvent struct {
	Scope       string
	Category    string
	SubCategory string
	ActionKind  string
	MemoryID    int64
	TargetIDs   []int64
	Content     string
}

type MutationListener func(ctx context.Context, ev MutationEvent)

type ExtraFactsProvider interface {
	LoadExtraFacts(ctx context.Context, scopeA, scopeB, category string) ([]domain.Fact, error)
}

type Service struct {
	db               *sql.DB
	dbPath           string
	router           *router.Router
	bus              *eventbus.Bus
	mu               sync.Mutex
	sent             map[string]map[int64]bool
	sentTouch        map[string]time.Time
	syncSeen         map[string]string
	syncTouch        map[string]time.Time
	autoGitSync      bool
	mutListeners     []MutationListener
	extraProviders   []ExtraFactsProvider
	lastSessionSweep atomic.Int64
}

func New(db *sql.DB, dbPath string, rtr *router.Router, bus *eventbus.Bus) *Service {
	if rtr == nil {
		rtr = router.New(db, dbPath, nil, nil)
	}
	if bus == nil {
		bus = eventbus.New()
	}
	return &Service{
		db:        db,
		dbPath:    dbPath,
		router:    rtr,
		bus:       bus,
		sent:      make(map[string]map[int64]bool),
		sentTouch: make(map[string]time.Time),
		syncSeen:  make(map[string]string),
		syncTouch: make(map[string]time.Time),
	}
}

func (s *Service) touchSentLocked(sessionID string, now time.Time) {
	if s.sentTouch == nil {
		s.sentTouch = make(map[string]time.Time)
	}
	s.sentTouch[sessionID] = now
	if len(s.sent) <= maxCachedMemorySessions {
		return
	}
	for sid, ts := range s.sentTouch {
		if sid != sessionID && now.Sub(ts) > memorySessionIdleTTL {
			delete(s.sent, sid)
			delete(s.sentTouch, sid)
		}
	}
	for len(s.sent) > maxCachedMemorySessions {
		oldestKey := ""
		var oldestTime time.Time
		for sid := range s.sent {
			if sid == sessionID {
				continue
			}
			ts := s.sentTouch[sid]
			if oldestKey == "" || ts.Before(oldestTime) {
				oldestKey = sid
				oldestTime = ts
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.sent, oldestKey)
		delete(s.sentTouch, oldestKey)
	}
}

func (s *Service) touchSyncSeenLocked(key string, now time.Time) {
	if s.syncTouch == nil {
		s.syncTouch = make(map[string]time.Time)
	}
	s.syncTouch[key] = now
	if len(s.syncSeen) <= maxCachedMemorySessions {
		return
	}
	for k, ts := range s.syncTouch {
		if k != key && now.Sub(ts) > memorySessionIdleTTL {
			delete(s.syncSeen, k)
			delete(s.syncTouch, k)
		}
	}
	for len(s.syncSeen) > maxCachedMemorySessions {
		oldestKey := ""
		var oldestTime time.Time
		for k := range s.syncSeen {
			if k == key {
				continue
			}
			ts := s.syncTouch[k]
			if oldestKey == "" || ts.Before(oldestTime) {
				oldestKey = k
				oldestTime = ts
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.syncSeen, oldestKey)
		delete(s.syncTouch, oldestKey)
	}
}

func (s *Service) maybeSweepTransientTables(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	nowUnix := time.Now().Unix()
	last := s.lastSessionSweep.Load()
	if nowUnix-last < int64(transientSweepInterval.Seconds()) {
		return
	}
	if !s.lastSessionSweep.CompareAndSwap(last, nowUnix) {
		return
	}
	sqlite.SweepTransientSessionTables(ctx, s.db)
}

func Open(dbPath string, eval domain.Evaluator) (*Service, error) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, err
	}
	bus := eventbus.New()
	rtr := router.New(db, dbPath, eval, nil)
	return New(db, dbPath, rtr, bus), nil
}

func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	config.RestrictDBPermissions(s.dbPath)
	return err
}

func (s *Service) DB() *sql.DB {
	return s.db
}

func (s *Service) DBPath() string {
	return s.dbPath
}

func (s *Service) Router() *router.Router {
	return s.router
}

func (s *Service) Bus() *eventbus.Bus {
	return s.bus
}

func (s *Service) AddMutationListener(fn MutationListener) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	s.mutListeners = append(s.mutListeners, fn)
	s.mu.Unlock()
}

func (s *Service) emitMutation(ctx context.Context, ev MutationEvent) {
	s.mu.Lock()
	listeners := append([]MutationListener(nil), s.mutListeners...)
	s.mu.Unlock()
	for _, fn := range listeners {
		fn(ctx, ev)
	}
}

func (s *Service) AttachExtraFactsProvider(p ExtraFactsProvider) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.extraProviders = append(s.extraProviders, p)
	s.mu.Unlock()
}

func (s *Service) InvalidateSessionIDs(ctx context.Context, ids ...int64) {
	if len(ids) == 0 {
		return
	}
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 {
			idSet[id] = true
		}
	}
	if len(idSet) == 0 {
		return
	}
	s.mu.Lock()
	for _, seen := range s.sent {
		for id := range idSet {
			delete(seen, id)
		}
	}
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	for id := range idSet {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE memory_id = ?`, id)
	}
}

func (s *Service) SetAutoGitSync(enabled bool) {
	s.mu.Lock()
	s.autoGitSync = enabled
	s.mu.Unlock()
}

func (s *Service) IsAutoGitSyncEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoGitSync
}

func (s *Service) IncludePersonalInProject(ctx context.Context) bool {
	if os.Getenv("PRAXIS_INCLUDE_PERSONAL") == "1" {
		return true
	}
	return s.GetConfigMap(ctx)[config.CfgIncludePersonalInProject] == "1"
}

func (s *Service) CurrentAccount(ctx context.Context) string {
	if p, ok := domain.AuthPrincipalFromContext(ctx); ok && p.ServerMode && p.Authenticated && p.Account != "" {
		return p.Account
	}
	if acc := domain.AccountFromContext(ctx); acc != "" {
		return acc
	}
	for _, k := range []string{"PRAXIS_ACCOUNT", "AGY_ACCOUNT"} {
		if v := domain.NormalizeAccount(os.Getenv(k)); v != "" {
			return v
		}
	}
	if cfgAcc := domain.NormalizeAccount(s.GetConfigMap(ctx)[config.CfgDefaultAccount]); cfgAcc != "" {
		return cfgAcc
	}
	return domain.DefaultAccount()
}

func (s *Service) canBypassPersonal(ctx context.Context) bool {
	return !domain.IsServerMode(ctx) && domain.IsAdminContext(ctx)
}

func (s *Service) memoryVisibilitySQL(ctx context.Context, alias string) (string, []any) {
	if !domain.IsServerMode(ctx) {
		return "1 = 1", nil
	}
	scCol, accCol := "scope", "account"
	if alias != "" {
		scCol = alias + ".scope"
		accCol = alias + ".account"
	}
	return fmt.Sprintf("(%s NOT IN ('personal', 'global') OR COALESCE(%s, '') = ?)", scCol, accCol), []any{s.CurrentAccount(ctx)}
}

func (s *Service) privateScopeSQL(ctx context.Context, alias string, allowBypass bool) (string, []any) {
	if allowBypass && s.canBypassPersonal(ctx) {
		return "1 = 1", nil
	}
	scCol, accCol := "scope", "account"
	if alias != "" {
		scCol = alias + ".scope"
		accCol = alias + ".account"
	}
	curAcc := s.CurrentAccount(ctx)
	if domain.IsServerMode(ctx) {
		return fmt.Sprintf("(%s NOT IN ('personal', 'global') OR COALESCE(%s, '') = ?)", scCol, accCol), []any{curAcc}
	}
	return fmt.Sprintf("(%s != 'personal' OR COALESCE(%s, '') = ?)", scCol, accCol), []any{curAcc}
}

func (s *Service) ResolveEffectiveSessionID(ctx context.Context, rawScope, explicit string) string {
	if exp := strings.TrimSpace(explicit); exp != "" {
		return domain.ScopedSessionID(ctx, exp)
	}
	ctxSID := domain.SessionIDFromContext(ctx)
	if ctxSID != "" && !strings.HasPrefix(ctxSID, "stdio-") && !strings.HasPrefix(ctxSID, "bridge-") {
		return domain.ScopedSessionID(ctx, ctxSID)
	}
	sc := domain.NormalizeScope(rawScope)
	var turnSID string
	cutoff := time.Now().Add(-domain.TurnBaselineTTL).Unix()
	userPrefix := ""
	if p, ok := domain.AuthPrincipalFromContext(ctx); ok && p.ServerMode && p.Account != "" {
		userPrefix = "u:" + p.Account + ":%"
	}
	_ = s.db.QueryRowContext(ctx, `
		SELECT session_id FROM turn_state
		WHERE scope = ? AND updated_at >= ? AND COALESCE(session_id, '') != ''
		  AND (? = '' OR session_id LIKE ?)
		ORDER BY updated_at DESC LIMIT 1
	`, sc, cutoff, userPrefix, userPrefix).Scan(&turnSID)
	if strings.TrimSpace(turnSID) != "" {
		return domain.ScopedSessionID(ctx, strings.TrimSpace(turnSID))
	}
	return domain.ScopedSessionID(ctx, ctxSID)
}

func ScopesFor(primary string, includeGlobal bool) (string, string) {
	sc := domain.NormalizeScope(primary)
	if includeGlobal && strings.HasPrefix(sc, "project:") {
		return sc, "global"
	}
	return sc, sc
}

func parseTagStr(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Fields(raw)
}

const factSelectColumns = `id, scope, COALESCE(account, ''), category, COALESCE(sub_category, ''), tags, content, pinned`

func (s *Service) LoadActiveFacts(ctx context.Context, scopeA, scopeB, category string, extraScopes ...string) ([]domain.Fact, error) {
	scopeC := scopeA
	if len(extraScopes) > 0 && strings.TrimSpace(extraScopes[0]) != "" {
		scopeC = domain.NormalizeScope(extraScopes[0])
	}
	cat, subCat := "", ""
	if rawCat := strings.TrimSpace(category); rawCat != "" {
		if strings.Contains(rawCat, "/") {
			cat, subCat = domain.SplitCategoryAndSub(scopeA, rawCat, "")
		} else {
			cat = domain.NormalizeCategoryForScope(scopeA, rawCat)
		}
	}
	privSQL, privArgs := s.privateScopeSQL(ctx, "", false)
	args := make([]any, 0, 8+len(privArgs))
	args = append(args, scopeA, scopeB, scopeC)
	args = append(args, privArgs...)
	args = append(args, cat, cat, cat, subCat, subCat)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factSelectColumns+`
		 FROM memories
		 WHERE active = 1 AND scope IN (?, ?, ?)
		   AND `+privSQL+`
		   AND (? = '' OR category = ? OR sub_category = ?)
		   AND (? = '' OR sub_category = ?)
		 ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts, err := scanFactRows(rows)
	if err != nil {
		return nil, err
	}
	domain.SortFactsByPrefixTier(facts)
	return facts, nil
}

func scanFactRows(rows *sql.Rows) ([]domain.Fact, error) {
	var facts []domain.Fact
	for rows.Next() {
		var f domain.Fact
		var tagStr string
		var pin int
		if err := rows.Scan(&f.ID, &f.Scope, &f.Account, &f.Category, &f.SubCategory, &tagStr, &f.Content, &pin); err != nil {
			return nil, err
		}
		f.Pinned = pin == 1
		f.Tags = parseTagStr(tagStr)
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

func (s *Service) loadQueryFacts(ctx context.Context, scopeA, scopeB, category string, extraScopes ...string) ([]domain.Fact, error) {
	facts, err := s.LoadActiveFacts(ctx, scopeA, scopeB, category, extraScopes...)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	providers := append([]ExtraFactsProvider(nil), s.extraProviders...)
	s.mu.Unlock()
	for _, p := range providers {
		extra, err := p.LoadExtraFacts(ctx, scopeA, scopeB, category)
		if err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			facts = append(extra, facts...)
		}
	}
	domain.SortFactsByPrefixTier(facts)
	return facts, nil
}

func formatFactLine(f domain.Fact, suffix string, compact bool) string {
	tagPart := ""
	if !compact && len(f.Tags) > 0 {
		tagPart = fmt.Sprintf(" `[%s]`", strings.Join(f.Tags, ", "))
	}
	if f.Category == "preference" && !f.Pinned && !strings.Contains(suffix, "soft preference") {
		suffix += " *(soft preference)*"
	}
	catLabel := f.Category
	if f.SubCategory != "" {
		catLabel = f.Category + "/" + f.SubCategory
	}
	var line string
	if f.IsTeam() {
		if compact {
			line = fmt.Sprintf("- [TEAM %s/%s] %s%s", f.Scope, catLabel, f.Content, suffix)
		} else {
			line = fmt.Sprintf("- **[%s / %s TEAM]**%s %s%s", f.Scope, catLabel, tagPart, f.Content, suffix)
		}
	} else {
		if compact {
			line = fmt.Sprintf("- [#%d %s/%s] %s%s", f.ID, f.Scope, catLabel, f.Content, suffix)
		} else {
			line = fmt.Sprintf("- **[%s / %s #%d]**%s %s%s", f.Scope, catLabel, f.ID, tagPart, f.Content, suffix)
		}
	}
	if len(f.Superseded) > 0 {
		line += fmt.Sprintf(" *(Replaces: %s)*", strings.Join(f.Superseded, "; "))
	}
	return line + "\n"
}

func (s *Service) evaluateRelevance(ctx context.Context, rawScope, category, query string) (string, int, domain.Evaluator, []domain.Fact, []domain.ScoredFact, error) {
	scA, scB := ScopesFor(rawScope, true)
	var extraScopes []string
	if strings.HasPrefix(scA, "project:") && s.IncludePersonalInProject(ctx) {
		extraScopes = append(extraScopes, "personal")
	}
	facts, err := s.loadQueryFacts(ctx, scA, scB, category, extraScopes...)
	if err != nil {
		return scA, 0, nil, nil, nil, err
	}
	ev := s.router.EvalForTask(ctx, domain.TaskRelevance)
	if len(facts) == 0 {
		return scA, 0, ev, nil, nil, nil
	}
	var pinned, candidates []domain.Fact
	for _, f := range facts {
		if f.Pinned {
			pinned = append(pinned, f)
		} else {
			candidates = append(candidates, f)
		}
	}
	var scored []domain.ScoredFact
	if len(candidates) > 0 {
		if ev == nil {
			return scA, len(facts), nil, pinned, nil, fmt.Errorf("no active evaluation engine configured")
		}
		evalStart := time.Now()
		scored, err = ev.ScoreRelevance(ctx, query, candidates)
		s.router.RecordTelemetry(ctx, domain.TaskRelevance, ev, evalStart, len(candidates), err)
		if err != nil {
			return scA, len(facts), ev, nil, nil, err
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].Score != scored[j].Score {
				return scored[i].Score > scored[j].Score
			}
			return scored[i].ID < scored[j].ID
		})
	}
	return scA, len(facts), ev, pinned, scored, nil
}

func (s *Service) CommitMemory(ctx context.Context, candidateFact, category string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	return s.CommitMemoryFull(ctx, candidateFact, category, "", "", rawScope)
}

func (s *Service) CommitMemoryFull(ctx context.Context, candidateFact, category, subCategory, rawTags, rawScope string) (string, error) {
	candidateFact = strings.TrimSpace(candidateFact)
	if candidateFact == "" {
		return "", fmt.Errorf("candidate_fact cannot be empty")
	}
	targetScope := domain.NormalizeScope(rawScope)
	if err := domain.CanModifyScope(ctx, targetScope); err != nil {
		return "", err
	}
	if reason := domain.CheckSecurityGuard(candidateFact); reason != "" {
		return fmt.Sprintf("REJECTED (security guard: %s)", reason), nil
	}

	existing, err := s.LoadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		return "", err
	}
	evalCtx := domain.WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
	ev := s.router.EvalForTask(ctx, domain.TaskMutation)
	if ev == nil {
		return "", fmt.Errorf("no active evaluation engine configured")
	}
	evalStart := time.Now()
	dec, err := ev.EvaluateMutation(evalCtx, targetScope, candidateFact, existing)
	s.router.RecordTelemetry(ctx, domain.TaskMutation, ev, evalStart, len(existing), err)
	if err != nil {
		return "", err
	}
	return s.applyEvaluatedCommit(ctx, targetScope, candidateFact, category, subCategory, rawTags, dec)
}

func (s *Service) applyEvaluatedCommit(ctx context.Context, targetScope, candidateFact, category, subCategory, rawTags string, dec domain.MutationDecision) (string, error) {
	domain.ApplyDecisionSemantics(&dec)

	explicitCat := strings.TrimSpace(category)
	explicitSub := strings.TrimSpace(subCategory)
	if idx := strings.Index(explicitCat, "/"); idx > 0 {
		if explicitSub == "" {
			explicitSub = strings.TrimSpace(explicitCat[idx+1:])
		}
		explicitCat = strings.TrimSpace(explicitCat[:idx])
	}
	catInput := explicitCat
	if catInput == "" {
		catInput = dec.Category
	}
	subInput := explicitSub
	if subInput == "" {
		subInput = dec.SubCategory
	}
	cat, subCat := domain.SplitCategoryAndSub(targetScope, catInput, subInput)

	mergedTags := append([]string{}, dec.Tags...)
	if strings.TrimSpace(rawTags) != "" {
		parts := strings.FieldsFunc(rawTags, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '#'
		})
		mergedTags = append(mergedTags, parts...)
	}
	tags := domain.NormalizeTags(targetScope, mergedTags)
	tagStr := strings.Join(tags, " ")

	acc := s.CurrentAccount(ctx)
	targetIDs := dec.ConflictFactIDs
	if len(targetIDs) == 0 && dec.ConflictFactID > 0 {
		targetIDs = []int64{dec.ConflictFactID}
	}

	privSQL, privArgs := s.privateScopeSQL(ctx, "", false)
	if dec.Action == "IGNORE" && len(targetIDs) > 0 {
		_ = s.withTx(ctx, func(tx *sql.Tx) error {
			for _, id := range targetIDs {
				qArgs := append([]any{id, targetScope}, privArgs...)
				_, _ = tx.ExecContext(ctx,
					`UPDATE memories SET confidence = MIN(1.0, 1.0 - (1.0 - confidence) * 0.8)
					 WHERE id = ? AND scope = ? AND `+privSQL+` AND active = 1`,
					qArgs...)
			}
			return nil
		})
		s.RecordTurnScopeMutatedIDsCtx(ctx, targetScope, targetIDs...)
		return fmt.Sprintf("SKIPPED (duplicate confirmed in %s; boosted confidence of %v)", targetScope, targetIDs), nil
	}

	th := s.GetThresholds(ctx)
	if dec.IsPermanent < th.MinPermanent || dec.Confidence < th.MinConfidence || dec.Action == "IGNORE" {
		s.MarkTurnScopeMutatedCtx(ctx, targetScope)
		return fmt.Sprintf("SKIPPED (scope=%s, perm=%.2f, conf=%.2f, action=%s)", targetScope, dec.IsPermanent, dec.Confidence, dec.Action), nil
	}

	pinned := 0
	if dec.IsGlobalInvariant >= th.PinInvariant {
		pinned = 1
	}

	catDisplay := cat
	if subCat != "" {
		catDisplay = cat + "/" + subCat
	}
	var (
		newID      int64
		status     string
		actionKind = "commit"
	)
	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			targetScope, acc, cat, subCat, tagStr, strings.TrimSpace(candidateFact), dec.Confidence, pinned)
		if err != nil {
			return err
		}
		newID, _ = res.LastInsertId()
		if dec.Action == "OVERWRITE" && len(targetIDs) > 0 {
			actionKind = "overwrite"
			for _, oldID := range targetIDs {
				qArgs := append([]any{newID, oldID, targetScope}, privArgs...)
				_, err := tx.ExecContext(ctx,
					`UPDATE memories SET active = 0, superseded_by = ?
					 WHERE id = ? AND scope = ? AND `+privSQL+` AND active = 1`,
					qArgs...)
				if err != nil {
					return err
				}
			}
			status = fmt.Sprintf("OVERWRITTEN (id=%d superseded %v, scope=%s, category=%s, tags=%v, pinned=%v)", newID, targetIDs, targetScope, catDisplay, tags, pinned == 1)
		} else {
			status = fmt.Sprintf("COMMITTED (id=%d, scope=%s, category=%s, tags=%v, pinned=%v)", newID, targetScope, catDisplay, tags, pinned == 1)
		}
		return nil
	}); err != nil {
		return "", err
	}
	s.RecordTurnScopeMutatedIDsCtx(ctx, targetScope, newID)
	if dec.Action == "OVERWRITE" && len(targetIDs) > 0 {
		// Keep SQLite session_sent rows intact so sessions that previously saw targetIDs get a 1-time (Replaces: ...) notice.
		s.clearInMemorySessionIDs(targetIDs...)
	}
	s.RegisterTaxonomyAppendOnly(ctx, targetScope, cat, subCat)
	s.emitMutation(ctx, MutationEvent{
		Scope:       targetScope,
		Category:    cat,
		SubCategory: subCat,
		ActionKind:  actionKind,
		MemoryID:    newID,
		TargetIDs:   targetIDs,
		Content:     candidateFact,
	})
	return status, nil
}

func (s *Service) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return sqlite.WithTx(ctx, s.db, fn)
}
