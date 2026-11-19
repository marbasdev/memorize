package rulepack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/router"
	"memorize/internal/infra/config"
	"memorize/internal/infra/eventbus"
	"memorize/internal/infra/git"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/memory"
)

// TeamFactIDOffset separates in-memory projected TeamRule IDs from local SQLite memories.id (>= 1)
// so relevance/guardrail evaluators never hit ID map collisions across Team Rules and Local Memories.
const TeamFactIDOffset int64 = 1_000_000_000

type SyncStatus struct {
	Scope            string
	RepoRoot         string
	TargetBranch     string
	FileHash         string
	RuleCount        int
	PendingConflicts int
	SyncedFrom       string // "disk" | "remote" | "cached" | "empty"
	Changed          bool
	ChangedKeys      []string
	LastError        string
}

const (
	maxRulePackCacheEntries = 256
	rulePackCacheIdleTTL    = 6 * time.Hour
)

type cachedRulePack struct {
	rules            []domain.TeamRule
	hash             string
	remoteBranch     string
	diskBaselineHash string
	lastAccess       time.Time
}

type ruleStateRow struct {
	status         string
	localMemoryID  int64
	localContent   string
	conflictReason string
	overrideNote   string
	updatedAt      string
}

type Service struct {
	mem     *memory.Service
	db      *sql.DB
	dbPath  string
	router  *router.Router
	bus     *eventbus.Bus
	cacheMu sync.RWMutex
	cache   map[string]*cachedRulePack
}

func New(mem *memory.Service) *Service {
	var (
		db     *sql.DB
		dbPath string
		rtr    *router.Router
		bus    *eventbus.Bus
	)
	if mem != nil {
		db = mem.DB()
		dbPath = mem.DBPath()
		rtr = mem.Router()
		bus = mem.Bus()
	}
	if bus == nil {
		bus = eventbus.New()
	}
	return &Service{
		mem:    mem,
		db:     db,
		dbPath: dbPath,
		router: rtr,
		bus:    bus,
		cache:  make(map[string]*cachedRulePack),
	}
}

func (s *Service) Memory() *memory.Service {
	return s.mem
}

func (s *Service) notifyChange() {
	if s != nil && s.bus != nil {
		s.bus.Publish("memories")
	}
}

func (s *Service) AuthorName(ctx context.Context, explicitAuthor, repoDir string) string {
	if a := domain.NormalizeGitHandle(explicitAuthor); a != "" {
		return a
	}
	if s != nil && s.mem != nil {
		if cfg := s.mem.GetConfigMap(ctx); cfg != nil {
			if a := domain.NormalizeGitHandle(cfg[config.CfgRulePackAuthor]); a != "" {
				return a
			}
		}
	}
	root := git.ResolveRoot(repoDir)
	if gAuthor := git.AuthorIdentity(ctx, root); gAuthor != "" {
		return domain.NormalizeGitHandle(gAuthor)
	}
	if s != nil && s.mem != nil {
		if acc := s.mem.CurrentAccount(ctx); acc != "" {
			return domain.NormalizeGitHandle(acc)
		}
	}
	return domain.NormalizeGitHandle(domain.DefaultAccount())
}

func (s *Service) SetAuthorName(ctx context.Context, author string) error {
	if s == nil || s.mem == nil {
		return nil
	}
	if err := domain.RequireAdmin(ctx); err != nil {
		return err
	}
	return s.mem.SaveConfig(ctx, map[string]string{config.CfgRulePackAuthor: domain.NormalizeGitHandle(author)})
}

func (s *Service) SyncMeta(ctx context.Context, rawScope string) (fileHash, lastError string) {
	sc := domain.NormalizeScope(rawScope)
	fh, _, _, le := s.getSyncRecord(ctx, sc)
	return fh, le
}

func (s *Service) resolveScopeRepoPath(ctx context.Context, rawScope, explicitRepo string) string {
	if s != nil && s.mem != nil {
		return s.mem.ResolveScopeRepoPath(ctx, rawScope, explicitRepo)
	}
	if domain.IsServerMode(ctx) {
		return ""
	}
	return git.ResolveRoot(explicitRepo)
}

func (s *Service) getSyncRecord(ctx context.Context, sc string) (fileHash, remoteBranch, diskBaselineHash, lastErr string) {
	if s == nil || s.db == nil {
		return "", "", "", ""
	}
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(file_hash, ''), COALESCE(remote_branch, ''), COALESCE(disk_baseline_hash, ''), COALESCE(last_error, '')
		FROM team_rule_sync WHERE scope = ?
	`, sc).Scan(&fileHash, &remoteBranch, &diskBaselineHash, &lastErr)
	return strings.TrimSpace(fileHash), strings.TrimSpace(remoteBranch), strings.TrimSpace(diskBaselineHash), strings.TrimSpace(lastErr)
}

func (s *Service) setSyncError(ctx context.Context, sc, msg string) {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO team_rule_sync (scope, last_error, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET last_error = excluded.last_error, updated_at = CURRENT_TIMESTAMP
	`, sc, strings.TrimSpace(msg))
}

func ruleSignature(cat, sub string, pinned bool, title, why, how, where string) string {
	return fmt.Sprintf("%s|%s|%t|%s|%s|%s|%s", cat, sub, pinned, title, why, how, where)
}

func virtualRuleID(ruleKey string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(domain.NormalizeRuleKey(ruleKey)))
	return TeamFactIDOffset + 1 + int64(h.Sum32()&0x7fffffff)
}

func cloneTeamRules(rules []domain.TeamRule, hash string) []domain.TeamRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]domain.TeamRule, len(rules))
	for i, r := range rules {
		r.FileHash = hash
		r.SortOrder = i
		out[i] = r
	}
	return out
}

// rulePackCacheKey isolates per-account remote TEAM_RULES.md snapshots in multi-user server mode
// so teammates on different branches or repos cannot overwrite each other's verified rule sets.
func rulePackCacheKey(ctx context.Context, sc string) string {
	norm := domain.NormalizeScope(sc)
	if !domain.IsServerMode(ctx) {
		return norm
	}
	acc := domain.EffectivePrincipalAccount(ctx)
	if acc == "" {
		return norm
	}
	return "u:" + acc + ":" + norm
}

func (s *Service) setCache(ctx context.Context, sc string, entry *cachedRulePack) {
	key := rulePackCacheKey(ctx, sc)
	now := time.Now()
	s.cacheMu.Lock()
	if s.cache == nil {
		s.cache = make(map[string]*cachedRulePack)
	}
	if entry == nil {
		delete(s.cache, key)
	} else {
		entry.lastAccess = now
		s.cache[key] = entry
		if len(s.cache) > maxRulePackCacheEntries {
			for k, e := range s.cache {
				if k != key && (e == nil || now.Sub(e.lastAccess) > rulePackCacheIdleTTL) {
					delete(s.cache, k)
				}
			}
			for len(s.cache) > maxRulePackCacheEntries {
				oldestKey := ""
				var oldestTime time.Time
				for k, e := range s.cache {
					if k == key {
						continue
					}
					var ts time.Time
					if e != nil {
						ts = e.lastAccess
					}
					if oldestKey == "" || ts.Before(oldestTime) {
						oldestKey = k
						oldestTime = ts
					}
				}
				if oldestKey == "" {
					break
				}
				delete(s.cache, oldestKey)
			}
		}
	}
	s.cacheMu.Unlock()
}

func (s *Service) getCache(ctx context.Context, sc string) *cachedRulePack {
	key := rulePackCacheKey(ctx, sc)
	now := time.Now()
	s.cacheMu.Lock()
	entry := s.cache[key]
	if entry != nil {
		entry.lastAccess = now
	}
	s.cacheMu.Unlock()
	return entry
}

func (s *Service) ensureLoaded(ctx context.Context, sc string) *cachedRulePack {
	if entry := s.getCache(ctx, sc); entry != nil {
		return entry
	}
	if s == nil || s.db == nil || !strings.HasPrefix(sc, "project:") {
		return nil
	}
	root := s.resolveScopeRepoPath(ctx, sc, "")
	if root == "" {
		return nil
	}

	_, remoteBranch, diskBaselineHash, _ := s.getSyncRecord(ctx, sc)
	raw, err := os.ReadFile(DocPath(root))
	diskHash := ""
	if err == nil {
		diskHash = CanonicalHash(raw)
	}

	var (
		chosenBytes []byte
		chosenHash  string
	)
	if remoteBranch != "" && (err != nil || diskHash == diskBaselineHash) {
		if remoteBytes, hasRemote, _ := git.ReadBlobAtRef(ctx, root, "refs/remotes/origin/"+remoteBranch, domain.RulePackRelPath); hasRemote {
			chosenBytes = remoteBytes
			chosenHash = CanonicalHash(remoteBytes)
		}
	}
	if len(chosenBytes) == 0 && err == nil {
		chosenBytes = raw
		chosenHash = diskHash
	}
	if len(chosenBytes) == 0 {
		entry := &cachedRulePack{}
		s.setCache(ctx, sc, entry)
		return entry
	}
	doc, parseErr := ParseDocument(chosenBytes, sc)
	if parseErr != nil {
		return nil
	}
	entry := &cachedRulePack{
		rules:            cloneTeamRules(doc.Rules, chosenHash),
		hash:             chosenHash,
		remoteBranch:     remoteBranch,
		diskBaselineHash: diskBaselineHash,
	}
	s.setCache(ctx, sc, entry)
	return entry
}

func (s *Service) IngestTeamRuleDoc(ctx context.Context, rawScope, rawMarkdown string) error {
	if s == nil {
		return nil
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return nil
	}
	raw := []byte(rawMarkdown)
	if len(strings.TrimSpace(rawMarkdown)) == 0 {
		s.setCache(ctx, sc, &cachedRulePack{})
		return nil
	}
	hash := CanonicalHash(raw)
	if prev := s.getCache(ctx, sc); prev != nil && prev.hash == hash {
		return nil
	}
	doc, err := ParseDocument(raw, sc)
	if err != nil {
		return err
	}
	s.setCache(ctx, sc, &cachedRulePack{
		rules: cloneTeamRules(doc.Rules, hash),
		hash:  hash,
	})
	return nil
}

func (s *Service) applyMirrorTx(ctx context.Context, sc string, doc *Document, hash, remoteBranch, diskBaselineHash string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	before := make(map[string]string)
	if prev := s.getCache(ctx, sc); prev != nil {
		for _, r := range prev.rules {
			before[r.RuleKey] = ruleSignature(r.Category, r.SubCategory, r.Pinned, r.Title, r.Why, r.How, r.Where)
		}
	}

	var changedKeys []string
	var invalidatedIDs []int64
	seenNew := make(map[string]bool)

	if err := sqlite.WithTx(ctx, s.db, func(tx *sql.Tx) error {
		if doc == nil || len(doc.Rules) == 0 {
			for key := range before {
				invalidatedIDs = append(invalidatedIDs, virtualRuleID(key))
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM team_rule_states WHERE scope = ?`, sc); err != nil {
				return err
			}
		} else {
			kept := make([]any, 0, len(doc.Rules)+1)
			kept = append(kept, sc)
			ph := make([]string, 0, len(doc.Rules))
			for _, r := range doc.Rules {
				seenNew[r.RuleKey] = true
				newSig := ruleSignature(r.Category, r.SubCategory, r.Pinned, r.Title, r.Why, r.How, r.Where)
				if prevSig, existed := before[r.RuleKey]; !existed {
					changedKeys = append(changedKeys, r.RuleKey)
				} else if prevSig != newSig {
					changedKeys = append(changedKeys, r.RuleKey)
					invalidatedIDs = append(invalidatedIDs, virtualRuleID(r.RuleKey))
				}
				kept = append(kept, r.RuleKey)
				ph = append(ph, "?")
			}
			for key := range before {
				if !seenNew[key] {
					invalidatedIDs = append(invalidatedIDs, virtualRuleID(key))
				}
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM team_rule_states WHERE scope = ? AND rule_key NOT IN (%s)`, strings.Join(ph, ",")), kept...); err != nil {
				return err
			}
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO team_rule_sync (scope, file_hash, remote_branch, disk_baseline_hash, last_error, updated_at)
			VALUES (?, ?, ?, ?, '', CURRENT_TIMESTAMP)
			ON CONFLICT(scope) DO UPDATE SET
				file_hash = excluded.file_hash,
				remote_branch = excluded.remote_branch,
				disk_baseline_hash = excluded.disk_baseline_hash,
				last_error = '',
				updated_at = CURRENT_TIMESTAMP
		`, sc, hash, strings.TrimSpace(remoteBranch), strings.TrimSpace(diskBaselineHash))
		return err
	}); err != nil {
		return nil, err
	}

	var nextRules []domain.TeamRule
	if doc != nil {
		nextRules = cloneTeamRules(doc.Rules, hash)
	}
	s.setCache(ctx, sc, &cachedRulePack{
		rules:            nextRules,
		hash:             hash,
		remoteBranch:     strings.TrimSpace(remoteBranch),
		diskBaselineHash: strings.TrimSpace(diskBaselineHash),
	})

	if s.mem != nil && len(invalidatedIDs) > 0 {
		s.mem.InvalidateSessionIDs(ctx, invalidatedIDs...)
	}
	config.RestrictDBPermissions(s.dbPath)
	return changedKeys, nil
}

func (s *Service) SyncTurn(ctx context.Context, rawScope, repoDir string) (SyncStatus, error) {
	return s.syncInternal(ctx, rawScope, repoDir, false)
}

func (s *Service) SyncExplicit(ctx context.Context, rawScope, repoDir string, fetchRemote bool) (SyncStatus, error) {
	return s.syncInternal(ctx, rawScope, repoDir, fetchRemote)
}

func currentDiskHash(root string) string {
	raw, err := os.ReadFile(DocPath(root))
	if err != nil {
		return ""
	}
	return CanonicalHash(raw)
}

func (s *Service) syncInternal(ctx context.Context, rawScope, repoDir string, fetchRemote bool) (SyncStatus, error) {
	sc := domain.NormalizeScope(rawScope)
	status := SyncStatus{Scope: sc, SyncedFrom: "empty"}
	if s == nil || s.db == nil || !strings.HasPrefix(sc, "project:") {
		return status, nil
	}
	root := s.resolveScopeRepoPath(ctx, sc, repoDir)
	status.RepoRoot = root
	if root == "" {
		return status, nil
	}

	storedHash, remoteBranch, diskBaselineHash, lastErr := s.getSyncRecord(ctx, sc)
	status.FileHash = storedHash
	status.LastError = lastErr

	if fetchRemote && git.HasOriginRemote(ctx, root) {
		targetBranch := git.DefaultTargetBranch(ctx, root)
		status.TargetBranch = targetBranch
		_ = git.FetchBranch(ctx, root, "origin", targetBranch)
		remoteRef := "refs/remotes/origin/" + targetBranch
		if remoteBytes, hasRemote, _ := git.ReadBlobAtRef(ctx, root, remoteRef, domain.RulePackRelPath); hasRemote {
			remoteHash := CanonicalHash(remoteBytes)
			curBranch := git.CurrentBranch(ctx, root)
			diskHash := currentDiskHash(root)
			if curBranch == targetBranch &&
				!git.IsPathDirty(ctx, root, domain.RulePackRelPath) &&
				git.AheadCount(ctx, root, curBranch, remoteRef) == 0 &&
				diskHash != remoteHash {
				return s.mirrorBytes(ctx, sc, root, targetBranch, remoteBytes, remoteHash, targetBranch, diskHash, "remote")
			}
			if curBranch != targetBranch {
				headBytes, hasHeadBlob, _ := git.ReadBlobAtRef(ctx, root, "HEAD", domain.RulePackRelPath)
				headHash := ""
				if hasHeadBlob {
					headHash = CanonicalHash(headBytes)
				}
				if diskHash == headHash && remoteHash != headHash {
					return s.mirrorBytes(ctx, sc, root, targetBranch, remoteBytes, remoteHash, targetBranch, diskHash, "remote")
				}
			}
		}
	}

	raw, err := os.ReadFile(DocPath(root))
	if err != nil {
		if !os.IsNotExist(err) {
			return status, err
		}
		if remoteBranch != "" && diskBaselineHash == "" && git.RemoteRefFastSHA(root, "origin", remoteBranch) != "" {
			if st, ok := s.cachedSyncStatus(ctx, sc, status, "remote", remoteBranch, ""); ok {
				return st, nil
			}
		}
		if storedHash != "" {
			if _, clearErr := s.applyMirrorTx(ctx, sc, nil, "", "", ""); clearErr != nil {
				return status, clearErr
			}
			status.Changed = true
			status.FileHash = ""
			s.notifyChange()
		}
		return status, nil
	}

	diskHash := CanonicalHash(raw)
	if storedHash == diskHash && lastErr == "" {
		st, _ := s.cachedSyncStatus(ctx, sc, status, "cached", "", diskHash)
		return st, nil
	}
	if remoteBranch != "" && diskHash == diskBaselineHash && git.RemoteRefFastSHA(root, "origin", remoteBranch) != "" {
		if st, ok := s.cachedSyncStatus(ctx, sc, status, "remote", remoteBranch, storedHash); ok {
			return st, nil
		}
	}

	return s.mirrorBytes(ctx, sc, root, "", raw, diskHash, "", "", "disk")
}

func (s *Service) cachedSyncStatus(ctx context.Context, sc string, status SyncStatus, source, targetBranch, fileHash string) (SyncStatus, bool) {
	rules, _ := s.ListTeamRules(ctx, sc)
	if source != "" {
		status.SyncedFrom = source
	}
	if targetBranch != "" {
		status.TargetBranch = targetBranch
	}
	if fileHash != "" {
		status.FileHash = fileHash
	}
	status.RuleCount = len(rules)
	status.PendingConflicts = s.CountPendingConflicts(ctx, sc)
	return status, len(rules) > 0
}

func (s *Service) mirrorBytes(ctx context.Context, sc, root, targetBranch string, raw []byte, hash, remoteBranch, diskBaselineHash, sourceLabel string) (SyncStatus, error) {
	status := SyncStatus{
		Scope:        sc,
		RepoRoot:     root,
		TargetBranch: targetBranch,
		FileHash:     hash,
		SyncedFrom:   sourceLabel,
	}
	storedHash, storedRemote, storedDiskBase, _ := s.getSyncRecord(ctx, sc)
	if storedHash == hash && storedRemote == remoteBranch && storedDiskBase == diskBaselineHash {
		st, _ := s.cachedSyncStatus(ctx, sc, status, "", "", "")
		return st, nil
	}

	doc, parseErr := ParseDocument(raw, sc)
	if parseErr != nil {
		s.setSyncError(ctx, sc, parseErr.Error())
		status.LastError = parseErr.Error()
		return status, parseErr
	}
	changedKeys, err := s.applyMirrorTx(ctx, sc, doc, hash, remoteBranch, diskBaselineHash)
	if err != nil {
		return status, err
	}
	status.Changed = true
	status.ChangedKeys = changedKeys
	status.RuleCount = len(doc.Rules)
	status.PendingConflicts = s.CountPendingConflicts(ctx, sc)
	s.notifyChange()
	return status, nil
}

func (s *Service) MirrorSnapshot(ctx context.Context, rawScope, repoRoot, remoteBranch string, raw []byte) (SyncStatus, error) {
	sc := domain.NormalizeScope(rawScope)
	diskBase := ""
	if strings.TrimSpace(remoteBranch) != "" {
		diskBase = currentDiskHash(repoRoot)
	}
	return s.mirrorBytes(ctx, sc, repoRoot, remoteBranch, raw, CanonicalHash(raw), remoteBranch, diskBase, "remote")
}

func (s *Service) loadRuleStates(ctx context.Context, sc string) (map[string]ruleStateRow, error) {
	states := make(map[string]ruleStateRow)
	if s == nil || s.db == nil {
		return states, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule_key, status, local_memory_id, COALESCE(local_content, ''),
		       COALESCE(conflict_reason, ''), COALESCE(override_note, ''), COALESCE(updated_at, '')
		FROM team_rule_states
		WHERE scope = ?
	`, sc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var st ruleStateRow
		if err := rows.Scan(
			&key, &st.status, &st.localMemoryID, &st.localContent,
			&st.conflictReason, &st.overrideNote, &st.updatedAt,
		); err != nil {
			return nil, err
		}
		states[key] = st
	}
	return states, rows.Err()
}

func (s *Service) ListTeamRules(ctx context.Context, rawScope string) ([]domain.TeamRule, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	sc := domain.NormalizeScope(rawScope)
	entry := s.ensureLoaded(ctx, sc)
	if entry == nil || len(entry.rules) == 0 {
		return nil, nil
	}
	states, err := s.loadRuleStates(ctx, sc)
	if err != nil {
		return nil, err
	}
	out := make([]domain.TeamRule, len(entry.rules))
	for i, r := range entry.rules {
		if st, ok := states[r.RuleKey]; ok {
			r.Resolution = st.status
			r.LocalOverrideNote = st.overrideNote
		}
		if strings.TrimSpace(r.Resolution) == "" {
			r.Resolution = domain.RuleStateAdopted
		}
		out[i] = r
	}
	return out, nil
}

// ListTeamRuleFacts converts active Team Rules (excluding local_override items) into read-only domain.Facts
// assigned disjoint positive virtual IDs above TeamFactIDOffset.
func (s *Service) ListTeamRuleFacts(ctx context.Context, rawScope string) ([]domain.Fact, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return nil, nil
	}
	rules, err := s.ListTeamRules(ctx, sc)
	if err != nil {
		return nil, err
	}
	var out []domain.Fact
	serverMode := domain.IsServerMode(ctx)
	for _, r := range rules {
		if !serverMode && r.Resolution == domain.RuleStateLocalOverride {
			continue
		}
		out = append(out, r.ToFact(virtualRuleID(r.RuleKey)))
	}
	return out, nil
}

func (s *Service) CountPendingConflicts(ctx context.Context, rawScope string) int {
	if s == nil || s.db == nil {
		return 0
	}
	var cnt int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM team_rule_states
		WHERE scope = ? AND status = ?
	`, domain.NormalizeScope(rawScope), domain.RuleStateConflictPending).Scan(&cnt)
	return cnt
}

func (s *Service) UpsertConflictState(ctx context.Context, rawScope, ruleKey, status string, localMemID int64, localContent, reason, overrideNote string) error {
	if s == nil || s.db == nil {
		return nil
	}
	sc := domain.NormalizeScope(rawScope)
	key := domain.NormalizeRuleKey(ruleKey)
	if key == "" {
		return errors.New("rule_key is required")
	}
	st := strings.TrimSpace(status)
	if st == "" {
		st = domain.RuleStateAdopted
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO team_rule_states (
			scope, rule_key, status, local_memory_id, local_content, conflict_reason, override_note, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope, rule_key) DO UPDATE SET
			status = excluded.status,
			local_memory_id = CASE WHEN excluded.local_memory_id > 0 THEN excluded.local_memory_id ELSE team_rule_states.local_memory_id END,
			local_content = CASE WHEN excluded.local_content != '' THEN excluded.local_content ELSE team_rule_states.local_content END,
			conflict_reason = excluded.conflict_reason,
			override_note = excluded.override_note,
			updated_at = CURRENT_TIMESTAMP
	`, sc, key, st, localMemID, strings.TrimSpace(localContent), strings.TrimSpace(reason), strings.TrimSpace(overrideNote))
	if err == nil {
		config.RestrictDBPermissions(s.dbPath)
		s.notifyChange()
	}
	return err
}

func (s *Service) ListConflicts(ctx context.Context, rawScope string, includeResolved bool) ([]domain.RulePackConflict, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	sc := domain.NormalizeScope(rawScope)
	entry := s.ensureLoaded(ctx, sc)
	if entry == nil || len(entry.rules) == 0 {
		return nil, nil
	}
	states, err := s.loadRuleStates(ctx, sc)
	if err != nil {
		return nil, err
	}

	var out []domain.RulePackConflict
	for _, r := range entry.rules {
		st, ok := states[r.RuleKey]
		if !ok {
			continue
		}
		if !includeResolved && st.status != domain.RuleStateConflictPending {
			continue
		}
		r.Resolution = st.status
		r.LocalOverrideNote = st.overrideNote
		out = append(out, domain.RulePackConflict{
			Scope:         sc,
			RuleKey:       r.RuleKey,
			Status:        st.status,
			LocalMemoryID: st.localMemoryID,
			LocalContent:  st.localContent,
			Reason:        st.conflictReason,
			UpdatedAt:     st.updatedAt,
			TeamRule:      r,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].TeamRule.SortOrder < out[j].TeamRule.SortOrder
	})
	return out, nil
}

func (s *Service) invalidateRuleSession(ctx context.Context, _ string, key string) {
	if s == nil || s.mem == nil {
		return
	}
	s.mem.InvalidateSessionIDs(ctx, virtualRuleID(key))
}

func (s *Service) ResolveConflict(ctx context.Context, rawScope, ruleKey, decision, note string) error {
	if s == nil || s.db == nil {
		return nil
	}
	sc := domain.NormalizeScope(rawScope)
	key := domain.NormalizeRuleKey(ruleKey)
	if key == "" {
		return errors.New("rule_key is required")
	}

	var localMemID int64
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(local_memory_id, 0) FROM team_rule_states WHERE scope = ? AND rule_key = ?
	`, sc, key).Scan(&localMemID)

	var err error
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "adopt_team", "adopt", "team":
		if localMemID > 0 && s.mem != nil {
			if _, ferr := s.mem.ForgetMemories(ctx, []int64{localMemID}); ferr != nil {
				return fmt.Errorf("retire conflicting local memory #%d: %w", localMemID, ferr)
			}
		}
		err = s.UpsertConflictState(ctx, sc, key, domain.RuleStateAdopted, localMemID, "", "", "")
	case "local_override", "override", "keep_local":
		if domain.IsServerMode(ctx) {
			return errors.New("team rules take precedence in server mode; adopt the team rule or update .memorize/TEAM_RULES.md")
		}
		err = s.UpsertConflictState(ctx, sc, key, domain.RuleStateLocalOverride, localMemID, "", "", note)
	case "mark_updated", "resolved", "draft_updated":
		err = s.UpsertConflictState(ctx, sc, key, domain.RuleStateAdopted, localMemID, "", "", "")
	default:
		return fmt.Errorf("unknown conflict resolution %q (use 'adopt_team', 'local_override', or 'mark_updated')", decision)
	}
	if err == nil {
		s.invalidateRuleSession(ctx, sc, key)
	}
	return err
}
