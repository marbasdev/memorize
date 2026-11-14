package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/router"
	"memorize/internal/infra/config"
	"memorize/internal/infra/eventbus"
	"memorize/internal/infra/sqlite"
)

// MutationEvent describes a committed, updated, or consolidated memory change for downstream observers.
type MutationEvent struct {
	Scope       string
	Category    string
	SubCategory string
	ActionKind  string
	MemoryID    int64
	TargetIDs   []int64
	Content     string
}

// MutationListener receives notifications whenever active memory facts are created, updated, or consolidated.
type MutationListener func(ctx context.Context, ev MutationEvent)

// Service owns persistent long-term memory storage, relevance retrieval, taxonomy, consolidation, and Git rule sync.
type Service struct {
	db           *sql.DB
	dbPath       string
	router       *router.Router
	bus          *eventbus.Bus
	mu           sync.Mutex
	sent         map[string]map[int64]bool
	syncSeen     map[string]string
	autoGitSync  bool
	mutListeners []MutationListener
}

// New constructs a memory Service from shared infrastructure components.
func New(db *sql.DB, dbPath string, rtr *router.Router, bus *eventbus.Bus) *Service {
	if rtr == nil {
		rtr = router.New(db, dbPath, nil, nil)
	}
	if bus == nil {
		bus = eventbus.New()
	}
	return &Service{
		db:       db,
		dbPath:   dbPath,
		router:   rtr,
		bus:      bus,
		sent:     make(map[string]map[int64]bool),
		syncSeen: make(map[string]string),
	}
}

// Open opens a private SQLite database and returns a standalone memory Service using eval as fallback Evaluator.
func Open(dbPath string, eval domain.Evaluator) (*Service, error) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, err
	}
	bus := eventbus.New()
	rtr := router.New(db, dbPath, eval, nil)
	return New(db, dbPath, rtr, bus), nil
}

// Close closes the underlying database handle and enforces 0600 file permissions on disk-backed DBs.
func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	config.RestrictDBPermissions(s.dbPath)
	return err
}

// DB returns the underlying SQLite connection handle.
func (s *Service) DB() *sql.DB {
	return s.db
}

// DBPath returns the filesystem path (or :memory:) configured for this service.
func (s *Service) DBPath() string {
	return s.dbPath
}

// Router returns the inference router used for task-specific evaluator selection and telemetry.
func (s *Service) Router() *router.Router {
	return s.router
}

// Bus returns the monotonic state change event bus.
func (s *Service) Bus() *eventbus.Bus {
	return s.bus
}

// AddMutationListener registers a callback invoked after CommitMemoryFull, UpdateMemoryFull, or ConsolidateMemories.
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

// SetAutoGitSync toggles autonomous HEAD/commit extraction during session/turn hooks.
func (s *Service) SetAutoGitSync(enabled bool) {
	s.mu.Lock()
	s.autoGitSync = enabled
	s.mu.Unlock()
}

// IsAutoGitSyncEnabled reports whether autonomous Git sync is enabled.
func (s *Service) IsAutoGitSyncEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoGitSync
}

// IncludePersonalInProject reports whether project:* queries should also load matching account personal facts.
func (s *Service) IncludePersonalInProject(ctx context.Context) bool {
	if os.Getenv("MEMORY_INCLUDE_PERSONAL") == "1" {
		return true
	}
	return s.GetConfigMap(ctx)["include_personal_in_project"] == "1"
}

// CurrentAccount resolves the effective user account identifier for personal scope isolation.
func (s *Service) CurrentAccount(ctx context.Context) string {
	if acc := domain.AccountFromContext(ctx); acc != "" {
		return acc
	}
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT"} {
		if v := domain.NormalizeAccount(os.Getenv(k)); v != "" {
			return v
		}
	}
	if cfgAcc := domain.NormalizeAccount(s.GetConfigMap(ctx)["default_account"]); cfgAcc != "" {
		return cfgAcc
	}
	return domain.DefaultAccount()
}

// ResolveEffectiveSessionID resolves the active conversation/session ID:
// explicit input wins, followed by non-generic context session IDs, active IDE hook turn_state
// conversation IDs for the scope, and finally generic MCP stdio/bridge session IDs.
func (s *Service) ResolveEffectiveSessionID(ctx context.Context, rawScope, explicit string) string {
	if exp := strings.TrimSpace(explicit); exp != "" {
		return exp
	}
	ctxSID := domain.SessionIDFromContext(ctx)
	if ctxSID != "" && !strings.HasPrefix(ctxSID, "stdio-") && !strings.HasPrefix(ctxSID, "bridge-") {
		return ctxSID
	}
	sc := domain.NormalizeScope(rawScope)
	var turnSID string
	cutoff := time.Now().Add(-2 * time.Hour).Unix()
	_ = s.db.QueryRowContext(ctx, `
		SELECT session_id FROM turn_state
		WHERE scope = ? AND updated_at >= ? AND COALESCE(session_id, '') != ''
		ORDER BY updated_at DESC LIMIT 1
	`, sc, cutoff).Scan(&turnSID)
	if strings.TrimSpace(turnSID) != "" {
		return strings.TrimSpace(turnSID)
	}
	return ctxSID
}

// ScopesFor expands project:* scopes to include global when includeGlobal is true.
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

// LoadActiveFacts loads active facts matching up to three scopes and an optional category filter.
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
	acc := s.CurrentAccount(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, COALESCE(account, ''), category, COALESCE(sub_category, ''), tags, content, pinned
		 FROM memories
		 WHERE active = 1 AND scope IN (?, ?, ?)
		   AND (scope != 'personal' OR account = ?)
		   AND (? = '' OR category = ? OR sub_category = ?)
		   AND (? = '' OR sub_category = ?)
		 ORDER BY id ASC`, scopeA, scopeB, scopeC, acc, cat, cat, cat, subCat, subCat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

// FactsByIDs returns a map of facts keyed by ID regardless of active state, used for audit/progress correlation.
func (s *Service) FactsByIDs(ctx context.Context, ids []int64) (map[int64]domain.Fact, error) {
	out := make(map[int64]domain.Fact)
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ph = append(ph, "?")
		args = append(args, id)
	}
	if len(ph) == 0 {
		return out, nil
	}
	q := fmt.Sprintf(`SELECT id, scope, COALESCE(account, ''), category, COALESCE(sub_category, ''), tags, content, pinned FROM memories WHERE id IN (%s)`, strings.Join(ph, ","))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.Fact
		var tagStr string
		var pin int
		if err := rows.Scan(&f.ID, &f.Scope, &f.Account, &f.Category, &f.SubCategory, &tagStr, &f.Content, &pin); err == nil {
			f.Pinned = pin == 1
			f.Tags = parseTagStr(tagStr)
			out[f.ID] = f
		}
	}
	return out, rows.Err()
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
	if compact {
		line = fmt.Sprintf("- [#%d %s/%s] %s%s", f.ID, f.Scope, catLabel, f.Content, suffix)
	} else {
		line = fmt.Sprintf("- **[%s / %s #%d]**%s %s%s", f.Scope, catLabel, f.ID, tagPart, f.Content, suffix)
	}
	if len(f.Superseded) > 0 {
		line += fmt.Sprintf(" *(Replaces: %s)*", strings.Join(f.Superseded, "; "))
	}
	return line + "\n"
}

// QueryMemory retrieves all relevant facts for currentTask across primary scope + global in stateless mode.
func (s *Service) QueryMemory(ctx context.Context, currentTask, category string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	return s.QuerySessionMemory(ctx, "", currentTask, category, rawScope, true)
}

func (s *Service) evaluateRelevance(ctx context.Context, rawScope, category, query string) (string, int, domain.Evaluator, []domain.Fact, []domain.ScoredFact, error) {
	scA, scB := ScopesFor(rawScope, true)
	var extraScopes []string
	if strings.HasPrefix(scA, "project:") && s.IncludePersonalInProject(ctx) {
		extraScopes = append(extraScopes, "personal")
	}
	facts, err := s.LoadActiveFacts(ctx, scA, scB, category, extraScopes...)
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

// CommitMemory evaluates and persists candidateFact into SQLite for category and optional scope.
func (s *Service) CommitMemory(ctx context.Context, candidateFact, category string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	return s.CommitMemoryFull(ctx, candidateFact, category, "", "", rawScope)
}

// CommitMemoryFull evaluates and persists candidateFact with explicit optional category, subCategory, tags, and scope.
func (s *Service) CommitMemoryFull(ctx context.Context, candidateFact, category, subCategory, rawTags, rawScope string) (string, error) {
	candidateFact = strings.TrimSpace(candidateFact)
	if candidateFact == "" {
		return "", fmt.Errorf("candidate_fact cannot be empty")
	}
	targetScope := domain.NormalizeScope(rawScope)
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

	if dec.Action == "IGNORE" && len(targetIDs) > 0 {
		for _, id := range targetIDs {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE memories SET confidence = MIN(1.0, 1.0 - (1.0 - confidence) * 0.8)
				 WHERE id = ? AND scope = ? AND (scope != 'personal' OR account = ?) AND active = 1`,
				id, targetScope, acc)
		}
		s.MarkTurnScopeMutated(targetScope)
		return fmt.Sprintf("SKIPPED (duplicate confirmed in %s; boosted confidence of %v)", targetScope, targetIDs), nil
	}

	th := s.GetThresholds(ctx)
	if dec.IsPermanent < th.MinPermanent || dec.Confidence < th.MinConfidence || dec.Action == "IGNORE" {
		s.MarkTurnScopeMutated(targetScope)
		return fmt.Sprintf("SKIPPED (scope=%s, perm=%.2f, conf=%.2f, action=%s)", targetScope, dec.IsPermanent, dec.Confidence, dec.Action), nil
	}

	pinned := 0
	if dec.IsGlobalInvariant >= th.PinInvariant {
		pinned = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		targetScope, acc, cat, subCat, tagStr, strings.TrimSpace(candidateFact), dec.Confidence, pinned)
	if err != nil {
		return "", err
	}
	newID, _ := res.LastInsertId()

	catDisplay := cat
	if subCat != "" {
		catDisplay = cat + "/" + subCat
	}
	var status string
	actionKind := "commit"
	if dec.Action == "OVERWRITE" && len(targetIDs) > 0 {
		actionKind = "overwrite"
		for _, oldID := range targetIDs {
			_, err := tx.ExecContext(ctx,
				`UPDATE memories SET active = 0, superseded_by = ?
				 WHERE id = ? AND scope = ? AND (scope != 'personal' OR account = ?) AND active = 1`,
				newID, oldID, targetScope, acc)
			if err != nil {
				return "", err
			}
		}
		status = fmt.Sprintf("OVERWRITTEN (id=%d superseded %v, scope=%s, category=%s, tags=%v, pinned=%v)", newID, targetIDs, targetScope, catDisplay, tags, pinned == 1)
	} else {
		status = fmt.Sprintf("COMMITTED (id=%d, scope=%s, category=%s, tags=%v, pinned=%v)", newID, targetScope, catDisplay, tags, pinned == 1)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.MarkTurnScopeMutated(targetScope)
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
