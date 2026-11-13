package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Service struct {
	db                 *sql.DB
	dbPath             string
	eval               Evaluator
	resolver           func(map[string]string) Evaluator
	taskResolver       func(map[string]string, EvalTask) (Evaluator, bool)
	readinessChecker   func(context.Context, map[string]string) (bool, string)
	mu                 sync.Mutex
	sent               map[string]map[int64]bool
	syncSeen           map[string]string
	autoGitSync        bool
	liveSessions       map[string]LiveSession
	lastRetentionSweep atomic.Int64
	eventMu            sync.Mutex
	eventSubs          map[chan uint64]struct{}
	domainEventSubs    map[chan ChangeEvent]struct{}
	eventSeq           atomic.Uint64
}

func (s *Service) DBPath() string {
	return s.dbPath
}

func (s *Service) SetAutoGitSync(enabled bool) {
	s.mu.Lock()
	s.autoGitSync = enabled
	s.mu.Unlock()
}

func (s *Service) isAutoGitSyncEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoGitSync
}

func restrictDBPermissions(dbPath string) {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file::memory:") {
		return
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(p+suf, 0o600)
	}
}

func (s *Service) SetEvaluatorResolver(fn func(map[string]string) Evaluator) {
	s.resolver = fn
}

// SetTaskEvaluatorResolver configures a task-aware resolver supporting per-feature
// model routing and safe generative fallback for non-generative decision engines.
func (s *Service) SetTaskEvaluatorResolver(fn func(map[string]string, EvalTask) (Evaluator, bool)) {
	s.taskResolver = fn
}

// SetReadinessChecker injects a deterministic readiness check reporting whether the
// active or default inference engine is ready to serve calls.
func (s *Service) SetReadinessChecker(fn func(context.Context, map[string]string) (bool, string)) {
	s.readinessChecker = fn
}

// CheckEngineReadiness reports whether an inference engine is configured and ready,
// plus a concise deterministic explanation when initial setup is needed.
func (s *Service) CheckEngineReadiness(ctx context.Context) (bool, string) {
	cfg := s.GetConfigMap(ctx)
	if s.readinessChecker != nil {
		return s.readinessChecker(ctx, cfg)
	}
	if strings.TrimSpace(cfg["active_provider"]) != "" || s.eval != nil {
		return true, ""
	}
	return false, "no inference engine configured"
}

func (s *Service) evalForTask(ctx context.Context, task EvalTask) Evaluator {
	if s.taskResolver != nil {
		cfg := s.GetConfigMap(ctx)
		if ev, handled := s.taskResolver(cfg, task); handled {
			return ev
		}
	}
	if s.resolver != nil {
		if cfg := s.GetConfigMap(ctx); cfg["active_provider"] != "" {
			if ev := s.resolver(cfg); ev != nil {
				return ev
			}
		}
	}
	return s.eval
}

func (s *Service) evalFor(ctx context.Context) Evaluator {
	return s.evalForTask(ctx, "")
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA busy_timeout=5000;
		CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			account TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL,
			sub_category TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			confidence REAL NOT NULL,
			pinned INTEGER NOT NULL DEFAULT 0,
			superseded_by INTEGER,
			active INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return err
	}
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN scope TEXT NOT NULL DEFAULT 'global'`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN sub_category TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN tags TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN replaces_text TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`UPDATE memories SET account = ? WHERE COALESCE(account, '') = ''`, DefaultAccount())
	_, _ = db.Exec(`
		DROP TRIGGER IF EXISTS memories_ai;
		DROP TRIGGER IF EXISTS memories_ad;
		DROP TRIGGER IF EXISTS memories_au;
		DROP TABLE IF EXISTS memories_fts;
		CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(scope, active, pinned);
		CREATE INDEX IF NOT EXISTS idx_memories_account ON memories(scope, account, active);
		CREATE INDEX IF NOT EXISTS idx_memories_superseded ON memories(scope, active, superseded_by);
		CREATE INDEX IF NOT EXISTS idx_memories_list_order ON memories(active DESC, pinned DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_memories_recent_order ON memories(active DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_memories_parent ON memories(superseded_by) WHERE superseded_by IS NOT NULL;
		CREATE TABLE IF NOT EXISTS repo_sync (
			scope TEXT PRIMARY KEY,
			last_commit TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			val REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			val TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_sent (
			session_id TEXT NOT NULL,
			memory_id INTEGER NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, memory_id)
		);
		CREATE TABLE IF NOT EXISTS turn_state (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			last_input TEXT NOT NULL DEFAULT '',
			mutated INTEGER NOT NULL DEFAULT 0,
			gate_checked INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, scope)
		);
		CREATE TABLE IF NOT EXISTS project_onboarding (
			scope TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS onboarding_prompts (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, scope)
		);
		CREATE TABLE IF NOT EXISTS session_updates (
			session_id TEXT NOT NULL,
			memory_id INTEGER NOT NULL,
			prev_content TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, memory_id)
		);
		CREATE TABLE IF NOT EXISTS project_progress (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			track TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL,
			sub_category TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'in_progress',
			summary TEXT NOT NULL,
			details TEXT NOT NULL DEFAULT '',
			next_step TEXT NOT NULL DEFAULT '',
			memory_ids TEXT NOT NULL DEFAULT '',
			commit_sha TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'agent',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_progress_scope_cat ON project_progress(scope, category, status, id DESC);
		CREATE INDEX IF NOT EXISTS idx_progress_scope_commit ON project_progress(scope, commit_sha);
		CREATE TABLE IF NOT EXISTS guardrail_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			source TEXT NOT NULL DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			verdict TEXT NOT NULL DEFAULT 'pass',
			blocked INTEGER NOT NULL DEFAULT 0,
			rule_count INTEGER NOT NULL DEFAULT 0,
			violated_ids TEXT NOT NULL DEFAULT '',
			plan_snippet TEXT NOT NULL DEFAULT '',
			details TEXT NOT NULL DEFAULT '',
			elapsed_ms REAL NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_guardrail_logs_scope_id ON guardrail_logs(scope, blocked, id DESC);
		CREATE TABLE IF NOT EXISTS eval_telemetry (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task TEXT NOT NULL,
			engine TEXT NOT NULL DEFAULT '',
			elapsed_ms REAL NOT NULL DEFAULT 0,
			item_count INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'ok',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_eval_telemetry_task_id ON eval_telemetry(task, id DESC);
		CREATE TABLE IF NOT EXISTS session_compacts (
			session_id TEXT PRIMARY KEY,
			last_offset INTEGER NOT NULL DEFAULT 0,
			last_marker TEXT NOT NULL DEFAULT '',
			pending_summary TEXT NOT NULL DEFAULT '',
			pending INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		);
	`)
	_, _ = db.Exec(`ALTER TABLE guardrail_logs ADD COLUMN elapsed_ms REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN track TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN details TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_progress_scope_track ON project_progress(scope, track, status, id DESC)`)
	_, _ = db.Exec(`ALTER TABLE repo_sync ADD COLUMN repo_path TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE session_sent ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP`)
	_, _ = db.Exec(`DELETE FROM session_sent WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.Exec(`DELETE FROM session_updates WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.Exec(`DELETE FROM turn_state WHERE updated_at < ?`, time.Now().Add(-2*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM onboarding_prompts WHERE updated_at < ?`, time.Now().Add(-6*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM session_compacts WHERE updated_at < ?`, time.Now().Add(-48*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM project_progress WHERE source = 'session_auto'`)
	return nil
}

func ensurePrivateDBFile(dbPath string) {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file:") {
		return
	}
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		_ = f.Close()
	}
	restrictDBPermissions(p)
}

func NewService(dbPath string, eval Evaluator) (*Service, error) {
	ensurePrivateDBFile(dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	restrictDBPermissions(dbPath)
	svc := &Service{
		db:       db,
		dbPath:   dbPath,
		eval:     eval,
		sent:     make(map[string]map[int64]bool),
		syncSeen: make(map[string]string),
	}
	svc.MaybeRunAutoRetention(context.Background())
	return svc, nil
}

func (s *Service) Close() error {
	err := s.db.Close()
	restrictDBPermissions(s.dbPath)
	return err
}

func (s *Service) includePersonalInProject(ctx context.Context) bool {
	if os.Getenv("MEMORY_INCLUDE_PERSONAL") == "1" {
		return true
	}
	return s.GetConfigMap(ctx)["include_personal_in_project"] == "1"
}

func (s *Service) CurrentAccount(ctx context.Context) string {
	if acc := AccountFromContext(ctx); acc != "" {
		return acc
	}
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT"} {
		if v := NormalizeAccount(os.Getenv(k)); v != "" {
			return v
		}
	}
	if cfgAcc := NormalizeAccount(s.GetConfigMap(ctx)["default_account"]); cfgAcc != "" {
		return cfgAcc
	}
	return DefaultAccount()
}

func scopesFor(primary string, includeGlobal bool) (string, string) {
	sc := NormalizeScope(primary)
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

func (s *Service) loadActiveFacts(ctx context.Context, scopeA, scopeB, category string, extraScopes ...string) ([]Fact, error) {
	scopeC := scopeA
	if len(extraScopes) > 0 && strings.TrimSpace(extraScopes[0]) != "" {
		scopeC = NormalizeScope(extraScopes[0])
	}
	cat, subCat := "", ""
	if rawCat := strings.TrimSpace(category); rawCat != "" {
		if strings.Contains(rawCat, "/") {
			cat, subCat = SplitCategoryAndSub(scopeA, rawCat, "")
		} else {
			cat = NormalizeCategoryForScope(scopeA, rawCat)
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

	var facts []Fact
	for rows.Next() {
		var f Fact
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

func formatFactLine(f Fact, suffix string, compact bool) string {
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

func (s *Service) QueryMemory(ctx context.Context, currentTask, category string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	return s.QuerySessionMemory(ctx, "", currentTask, category, rawScope, true)
}

func (s *Service) evaluateRelevance(ctx context.Context, rawScope, category, query string) (string, int, Evaluator, []Fact, []ScoredFact, error) {
	scA, scB := scopesFor(rawScope, true)
	var extraScopes []string
	if strings.HasPrefix(scA, "project:") && s.includePersonalInProject(ctx) {
		extraScopes = append(extraScopes, "personal")
	}
	facts, err := s.loadActiveFacts(ctx, scA, scB, category, extraScopes...)
	if err != nil {
		return scA, 0, nil, nil, nil, err
	}
	ev := s.evalForTask(ctx, TaskRelevance)
	if len(facts) == 0 {
		return scA, 0, ev, nil, nil, nil
	}
	var pinned, candidates []Fact
	for _, f := range facts {
		if f.Pinned {
			pinned = append(pinned, f)
		} else {
			candidates = append(candidates, f)
		}
	}
	var scored []ScoredFact
	if len(candidates) > 0 {
		if ev == nil {
			return scA, len(facts), nil, pinned, nil, fmt.Errorf("no active evaluation engine configured")
		}
		evalStart := time.Now()
		scored, err = ev.ScoreRelevance(ctx, query, candidates)
		s.recordEvalTelemetry(ctx, TaskRelevance, ev, evalStart, len(candidates), err)
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
	targetScope := NormalizeScope(rawScope)
	if reason := checkSecurityGuard(candidateFact); reason != "" {
		return fmt.Sprintf("REJECTED (security guard: %s)", reason), nil
	}

	existing, err := s.loadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		return "", err
	}
	evalCtx := WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
	ev := s.evalForTask(ctx, TaskMutation)
	if ev == nil {
		return "", fmt.Errorf("no active evaluation engine configured")
	}
	evalStart := time.Now()
	dec, err := ev.EvaluateMutation(evalCtx, targetScope, candidateFact, existing)
	s.recordEvalTelemetry(ctx, TaskMutation, ev, evalStart, len(existing), err)
	if err != nil {
		return "", err
	}
	return s.applyEvaluatedCommit(ctx, targetScope, candidateFact, category, subCategory, rawTags, dec)
}

func (s *Service) applyEvaluatedCommit(ctx context.Context, targetScope, candidateFact, category, subCategory, rawTags string, dec MutationDecision) (string, error) {
	ApplyDecisionSemantics(&dec)

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
	cat, subCat := SplitCategoryAndSub(targetScope, catInput, subInput)

	mergedTags := append([]string{}, dec.Tags...)
	if strings.TrimSpace(rawTags) != "" {
		parts := strings.FieldsFunc(rawTags, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '#'
		})
		mergedTags = append(mergedTags, parts...)
	}
	tags := NormalizeTags(targetScope, mergedTags)
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
		s.markTurnScopeMutated(targetScope)
		return fmt.Sprintf("SKIPPED (duplicate confirmed in %s; boosted confidence of %v)", targetScope, targetIDs), nil
	}

	th := s.GetThresholds(ctx)
	if dec.IsPermanent < th.MinPermanent || dec.Confidence < th.MinConfidence || dec.Action == "IGNORE" {
		s.markTurnScopeMutated(targetScope)
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
	s.markTurnScopeMutated(targetScope)
	if dec.Action == "OVERWRITE" && len(targetIDs) > 0 {
		// Keep SQLite session_sent rows intact so sessions that previously saw targetIDs get a 1-time (Replaces: ...) notice.
		s.clearInMemorySessionIDs(targetIDs...)
	}
	s.RegisterTaxonomyAppendOnly(ctx, targetScope, cat, subCat)
	s.RecordMemoryMutationProgress(ctx, targetScope, cat, subCat, actionKind, newID, targetIDs, candidateFact)
	return status, nil
}

// filterEnforceableRules excludes soft unpinned preferences, which act as tie-breaker hints rather than blocking constraints.
func filterEnforceableRules(loaded []Fact) []Fact {
	var rules []Fact
	for _, r := range loaded {
		if r.Category == "preference" && !r.Pinned {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

func filterAndSortViolations(violations []PlanViolation, minProb float64) []PlanViolation {
	var filtered []PlanViolation
	for _, v := range violations {
		if minProb <= 0 || v.Probability >= minProb {
			filtered = append(filtered, v)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Probability != filtered[j].Probability {
			return filtered[i].Probability > filtered[j].Probability
		}
		return filtered[i].ID < filtered[j].ID
	})
	return filtered
}

// EvaluatePlanViolations loads all active enforceable rules (excluding soft unpinned preferences),
// evaluates whether proposedPlan violates them, filters by minProb, and sorts descending by probability.
func (s *Service) EvaluatePlanViolations(ctx context.Context, proposedPlan, rawScope string, minProb float64) (string, int, []PlanViolation, error) {
	scA, scB := scopesFor(rawScope, true)
	loaded, err := s.loadActiveFacts(ctx, scA, scB, "")
	if err != nil {
		return scA, 0, nil, err
	}
	rules := filterEnforceableRules(loaded)
	if len(rules) == 0 {
		return scA, 0, nil, nil
	}
	ev := s.evalForTask(ctx, TaskVerify)
	if ev == nil {
		return scA, len(rules), nil, fmt.Errorf("no active evaluation engine configured")
	}
	evalStart := time.Now()
	violations, err := ev.VerifyPlan(ctx, proposedPlan, rules)
	s.recordEvalTelemetry(ctx, TaskVerify, ev, evalStart, len(rules), err)
	if err != nil {
		return scA, len(rules), nil, err
	}
	return scA, len(rules), filterAndSortViolations(violations, minProb), nil
}

func (s *Service) VerifyPlan(ctx context.Context, proposedPlan string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	t0 := time.Now()
	scA, ruleCount, violations, err := s.EvaluatePlanViolations(ctx, proposedPlan, rawScope, 0)
	if err != nil {
		return "", err
	}
	if ruleCount == 0 {
		return fmt.Sprintf("PASS: No active rules in scope [%s] to verify against.", scA), nil
	}
	elapsedMs := elapsedMsSince(t0)
	src := VerifySourceFromContext(ctx)
	if len(violations) == 0 {
		_, _ = s.RecordGuardrailLog(ctx, GuardrailLogInput{
			Scope:       scA,
			Source:      src,
			ToolName:    "verify_plan",
			Verdict:     GuardrailVerdictPass,
			Blocked:     false,
			RuleCount:   ruleCount,
			PlanSnippet: proposedPlan,
			ElapsedMs:   elapsedMs,
		})
		return "PASS: Proposed plan complies with all active memories.", nil
	}
	vIDs, vDetails := formatViolationDetails(violations)
	_, _ = s.RecordGuardrailLog(ctx, GuardrailLogInput{
		Scope:       scA,
		Source:      src,
		ToolName:    "verify_plan",
		Verdict:     GuardrailVerdictPlanViolation,
		Blocked:     true,
		RuleCount:   ruleCount,
		ViolatedIDs: vIDs,
		PlanSnippet: proposedPlan,
		Details:     vDetails,
		ElapsedMs:   elapsedMs,
	})
	var sb strings.Builder
	sb.WriteString("**Plan Memory Violations Detected:**\n")
	for _, v := range violations {
		fmt.Fprintf(&sb, "- **[%s / %s #%d]** %s *(violation prob: %.0f%%, conf: %.2f)*\n",
			v.Scope, v.Category, v.ID, v.Content, v.Probability*100, v.Confidence)
	}
	return sb.String(), nil
}
