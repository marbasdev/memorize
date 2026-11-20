package rulepack

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"praxis/internal/domain"
)

const maxTrimmedLocalRunes = 180

type DuplicateCleanupAction struct {
	LocalMemoryID  int64  `json:"local_memory_id"`
	Action         string `json:"action"` // "delete" | "trim"
	TrimmedContent string `json:"trimmed_content,omitempty"`
}

type DuplicateCleanupResult struct {
	DeletedIDs []int64 `json:"deleted_ids,omitempty"`
	TrimmedIDs []int64 `json:"trimmed_ids,omitempty"`
}

func (s *Service) LoadExtraFacts(ctx context.Context, scopeA, scopeB, category string) ([]domain.Fact, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	scA := domain.NormalizeScope(scopeA)
	scB := domain.NormalizeScope(scopeB)
	var targets []string
	if strings.HasPrefix(scA, "project:") {
		targets = append(targets, scA)
	}
	if strings.HasPrefix(scB, "project:") && scB != scA {
		targets = append(targets, scB)
	}
	if len(targets) == 0 {
		return nil, nil
	}

	var out []domain.Fact
	for _, sc := range targets {
		facts, err := s.ListTeamRuleFacts(ctx, sc)
		if err != nil {
			return nil, err
		}
		for _, f := range facts {
			if matchesCategoryFilter(f, sc, category) {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

func matchesCategoryFilter(f domain.Fact, scope, rawCat string) bool {
	trimmed := strings.TrimSpace(rawCat)
	if trimmed == "" {
		return true
	}
	if strings.Contains(trimmed, "/") {
		cat, sub := domain.SplitCategoryAndSub(scope, trimmed, "")
		return f.Category == cat && f.SubCategory == sub
	}
	norm := domain.NormalizeCategoryForScope(scope, trimmed)
	return f.Category == norm || f.SubCategory == norm
}

func toOutlineEntries(rules []domain.TeamRule) []domain.RuleOutlineEntry {
	out := make([]domain.RuleOutlineEntry, 0, len(rules))
	for _, r := range rules {
		out = append(out, domain.RuleOutlineEntry{
			RuleKey:     r.RuleKey,
			Category:    r.Category,
			SubCategory: r.SubCategory,
			Pinned:      r.Pinned,
			Title:       r.Title,
		})
	}
	return out
}

func (s *Service) normalizeAttribution(ctx context.Context, root, addedBy, addedAt string) (string, string) {
	by := domain.NormalizeGitHandle(addedBy)
	if strings.TrimSpace(addedBy) == "" {
		by = s.AuthorName(ctx, "", root)
	}
	at := strings.TrimSpace(addedAt)
	if at == "" {
		at = time.Now().Format("2006-01-02")
	}
	return by, at
}

func (s *Service) DocOutline(ctx context.Context, rawScope, repoDir string) ([]domain.RuleOutlineEntry, string, error) {
	sc := domain.NormalizeScope(rawScope)
	root := s.resolveScopeRepoPath(ctx, sc, repoDir)
	if root != "" {
		if raw, rerr := os.ReadFile(DocPath(root)); rerr == nil {
			if doc, perr := ParseDocument(raw, sc); perr == nil && doc.Exists {
				return toOutlineEntries(doc.Rules), CanonicalHash(raw), nil
			}
		}
	}

	rules, err := s.ListTeamRules(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	storedHash, _, _, _ := s.getSyncRecord(ctx, sc)
	return toOutlineEntries(rules), storedHash, nil
}

func (s *Service) DraftProposalFromText(ctx context.Context, rawScope, repoDir, rawRuleText, sourceCat, sourceSub string) (domain.RulePlacementProposal, error) {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return domain.RulePlacementProposal{}, fmt.Errorf("team rule pack requires a project:* scope (got %q)", sc)
	}
	text := strings.TrimSpace(rawRuleText)
	if text == "" {
		return domain.RulePlacementProposal{}, fmt.Errorf("rule content cannot be empty")
	}
	if s == nil || s.router == nil {
		return domain.RulePlacementProposal{}, fmt.Errorf("inference router is unavailable for Team Rule drafting")
	}

	ev := s.router.EvalForTask(ctx, domain.TaskConsolidate)
	if !domain.EvaluatorSupportsTask(ev, domain.TaskConsolidate) {
		return domain.RulePlacementProposal{}, fmt.Errorf("no generative evaluation engine available for Team Rule drafting")
	}
	planner, ok := ev.(domain.RulePackPlanner)
	if !ok {
		return domain.RulePlacementProposal{}, fmt.Errorf("active engine %q does not support Team Rule drafting", ev.Name())
	}

	outline, baseHash, err := s.DocOutline(ctx, sc, repoDir)
	if err != nil {
		return domain.RulePlacementProposal{}, err
	}

	pctx := ctx
	if s.mem != nil {
		pctx = domain.WithPresetCategories(ctx, s.mem.EffectiveTaxonomyJSON(ctx))
	}

	evalStart := time.Now()
	prop, err := planner.DraftRulePackProposal(pctx, sc, text, sourceCat, sourceSub, outline)
	s.router.RecordTelemetry(pctx, domain.TaskConsolidate, ev, evalStart, len(outline)+1, err)
	if err != nil {
		return domain.RulePlacementProposal{}, err
	}

	prop.Category = domain.NormalizeCategoryForScope(sc, prop.Category)
	prop.SubCategory = domain.NormalizeSubCategory(prop.SubCategory)
	prop.RuleKey = domain.ComputeRuleKey(prop.Category, prop.Title)
	prop.Pinned = true
	root := repoDir
	if strings.TrimSpace(prop.AddedBy) == "" {
		if resolved := s.resolveScopeRepoPath(ctx, sc, repoDir); resolved != "" {
			root = resolved
		}
	}
	prop.AddedBy, prop.AddedAt = s.normalizeAttribution(ctx, root, prop.AddedBy, prop.AddedAt)
	prop.BaseHash = baseHash
	return prop, nil
}

func (s *Service) PromoteLocalMemory(ctx context.Context, localMemoryID int64, rawScope, repoDir string) (domain.RulePlacementProposal, domain.Fact, error) {
	if s == nil || s.mem == nil {
		return domain.RulePlacementProposal{}, domain.Fact{}, fmt.Errorf("memory service is unavailable")
	}
	if localMemoryID <= 0 {
		return domain.RulePlacementProposal{}, domain.Fact{}, fmt.Errorf("invalid local_memory_id %d", localMemoryID)
	}

	recs, err := s.mem.GetRecordsByIDs(ctx, []int64{localMemoryID})
	if err != nil {
		return domain.RulePlacementProposal{}, domain.Fact{}, err
	}
	if len(recs) == 0 {
		return domain.RulePlacementProposal{}, domain.Fact{}, fmt.Errorf("local memory #%d not found", localMemoryID)
	}
	rec := recs[0]
	if !rec.Active {
		return domain.RulePlacementProposal{}, domain.Fact{}, fmt.Errorf("local memory #%d is inactive/archived", localMemoryID)
	}
	if domain.IsUserPrivateScope(ctx, rec.Scope) && !domain.IsAdminContext(ctx) {
		cur := s.mem.CurrentAccount(ctx)
		if rec.Account != "" && rec.Account != cur {
			return domain.RulePlacementProposal{}, domain.Fact{}, fmt.Errorf("permission denied: private memory #%d belongs to %q", localMemoryID, rec.Account)
		}
	}
	src := domain.Fact{
		ID:          rec.ID,
		Scope:       rec.Scope,
		Account:     rec.Account,
		Category:    rec.Category,
		SubCategory: rec.SubCategory,
		Tags:        rec.Tags,
		Content:     rec.Content,
		Pinned:      rec.Pinned,
	}

	targetScope := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(targetScope, "project:") {
		if strings.HasPrefix(src.Scope, "project:") {
			targetScope = src.Scope
		}
	}
	prop, err := s.DraftProposalFromText(ctx, targetScope, repoDir, src.Content, src.Category, src.SubCategory)
	return prop, src, err
}

func (s *Service) DefaultConflictEvaluator(rawScope string) ConflictEvaluator {
	sc := domain.NormalizeScope(rawScope)
	return func(ctx context.Context, rule domain.TeamRule, latestDoc *Document) (bool, string, error) {
		if latestDoc == nil || len(latestDoc.Rules) == 0 || s == nil || s.router == nil {
			return false, "", nil
		}
		ev := s.router.EvalForTask(ctx, domain.TaskMutation)
		if ev == nil {
			return false, "", nil
		}
		facts := make([]domain.Fact, 0, len(latestDoc.Rules))
		byID := make(map[int64]domain.TeamRule, len(latestDoc.Rules))
		for i, r := range latestDoc.Rules {
			id := TeamFactIDOffset + int64(i+1)
			byID[id] = r
			facts = append(facts, r.ToFact(id))
		}
		evalStart := time.Now()
		dec, err := ev.EvaluateMutation(ctx, sc, rule.DisplayContent(), facts)
		s.router.RecordTelemetry(ctx, domain.TaskMutation, ev, evalStart, len(facts), err)
		if err != nil {
			return false, "", nil
		}
		switch strings.ToUpper(strings.TrimSpace(dec.Action)) {
		case "IGNORE":
			return true, "", nil
		case "OVERWRITE":
			var descs []string
			ids := dec.ConflictFactIDs
			if len(ids) == 0 && dec.ConflictFactID > 0 {
				ids = []int64{dec.ConflictFactID}
			}
			for _, id := range ids {
				if r, ok := byID[id]; ok {
					descs = append(descs, fmt.Sprintf("%q", r.Title))
				}
			}
			reason := "policy conflict with existing team rule"
			if len(descs) > 0 {
				reason = fmt.Sprintf("conflicts with team rule(s) %s", strings.Join(descs, ", "))
			}
			return false, reason, nil
		default:
			return false, "", nil
		}
	}
}

// PublishWithDefaults fills default CAS conflict checking and optionally archives a promoted local memory
// once the Team Rule is safely published (never when only a PR branch was opened).
func (s *Service) PublishWithDefaults(ctx context.Context, in PublishInput, retireLocalOnPublish bool) (PublishResult, error) {
	if in.ConflictCheck == nil {
		in.ConflictCheck = s.DefaultConflictEvaluator(in.Scope)
	}
	res, err := s.PublishProposal(ctx, in)
	if err != nil {
		return res, err
	}
	if retireLocalOnPublish && in.LocalMemoryID > 0 && s != nil && s.mem != nil {
		if res.Status == "published" || res.Status == "already_covered" {
			_, _ = s.mem.ForgetMemories(ctx, []int64{in.LocalMemoryID})
		}
	}
	return res, nil
}

func (s *Service) UpdateWithDefaults(ctx context.Context, in UpdateInput) (PublishResult, error) {
	if in.ConflictCheck == nil {
		in.ConflictCheck = s.DefaultConflictEvaluator(in.Scope)
	}
	return s.UpdateRule(ctx, in)
}

func (s *Service) ScanConflictsForSyncedRules(ctx context.Context, rawScope string, changedKeys []string) ([]domain.RulePackConflict, error) {
	if s == nil || s.db == nil || s.mem == nil || s.router == nil {
		return nil, nil
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return nil, nil
	}

	rules, err := s.ListTeamRules(ctx, sc)
	if err != nil || len(rules) == 0 {
		return nil, err
	}

	localFacts, err := s.mem.LoadActiveFacts(ctx, sc, sc, "")
	if err != nil || len(localFacts) == 0 {
		return nil, err
	}

	var keyFilter map[string]bool
	if changedKeys != nil {
		if len(changedKeys) == 0 {
			return nil, nil
		}
		keyFilter = make(map[string]bool, len(changedKeys))
		for _, k := range changedKeys {
			keyFilter[k] = true
		}
	}

	ev := s.router.EvalForTask(ctx, domain.TaskMutation)
	if ev == nil {
		return nil, nil
	}
	localByID := make(map[int64]domain.Fact, len(localFacts))
	for _, f := range localFacts {
		localByID[f.ID] = f
	}

	var detected []domain.RulePackConflict
	for _, r := range rules {
		if keyFilter != nil && !keyFilter[r.RuleKey] {
			continue
		}
		if r.Resolution == domain.RuleStateLocalOverride {
			continue
		}

		evalStart := time.Now()
		dec, evalErr := ev.EvaluateMutation(ctx, sc, r.DisplayContent(), localFacts)
		s.router.RecordTelemetry(ctx, domain.TaskMutation, ev, evalStart, len(localFacts), evalErr)
		if evalErr != nil {
			continue
		}

		if strings.ToUpper(strings.TrimSpace(dec.Action)) == "OVERWRITE" {
			ids := dec.ConflictFactIDs
			if len(ids) == 0 && dec.ConflictFactID > 0 {
				ids = []int64{dec.ConflictFactID}
			}
			var conflictFact domain.Fact
			for _, id := range ids {
				if lf, ok := localByID[id]; ok {
					conflictFact = lf
					break
				}
			}
			if conflictFact.ID > 0 {
				reason := fmt.Sprintf("Team rule %q conflicts with local memory #%d", r.Title, conflictFact.ID)
				if uerr := s.UpsertConflictState(ctx, sc, r.RuleKey, domain.RuleStateConflictPending, conflictFact.ID, conflictFact.Content, reason, ""); uerr == nil {
					r.Resolution = domain.RuleStateConflictPending
					detected = append(detected, domain.RulePackConflict{
						Scope:         sc,
						RuleKey:       r.RuleKey,
						TeamRule:      r,
						LocalMemoryID: conflictFact.ID,
						LocalContent:  conflictFact.Content,
						Reason:        reason,
						Status:        domain.RuleStateConflictPending,
					})
				}
				continue
			}
		}

		if r.Resolution == domain.RuleStateConflictPending {
			_ = s.UpsertConflictState(ctx, sc, r.RuleKey, domain.RuleStateAdopted, 0, "", "", "")
		}
	}
	return detected, nil
}

func (s *Service) SyncTurnAndScanConflicts(ctx context.Context, rawScope, repoDir string) (SyncStatus, string, error) {
	status, err := s.SyncTurn(ctx, rawScope, repoDir)
	if err != nil {
		return status, s.FormatPendingConflictNotice(ctx, status.Scope), err
	}
	if status.Changed && len(status.ChangedKeys) > 0 {
		if _, serr := s.ScanConflictsForSyncedRules(ctx, status.Scope, status.ChangedKeys); serr == nil {
			status.PendingConflicts = s.CountPendingConflicts(ctx, status.Scope)
		}
	}
	return status, s.FormatPendingConflictNotice(ctx, status.Scope), nil
}

func (s *Service) SyncTurnNotice(ctx context.Context, rawScope, repoDir string) (string, error) {
	_, notice, err := s.SyncTurnAndScanConflicts(ctx, rawScope, repoDir)
	return notice, err
}

func (s *Service) FormatPendingConflictNotice(ctx context.Context, rawScope string) string {
	if s == nil || s.db == nil {
		return ""
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	conflicts, err := s.ListConflicts(ctx, sc, false)
	if err != nil || len(conflicts) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[TEAM RULE CONFLICT DETECTED in %s — %d pending decision(s)]\n", sc, len(conflicts))
	sb.WriteString("Stop and ask the user whether to adopt the Team Rule or keep their local override before applying conflicting changes:\n")
	for i, c := range conflicts {
		fmt.Fprintf(&sb, "- Conflict #%d: Team Rule %q vs Local `#%d` (%s)", i+1, c.TeamRule.Title, c.LocalMemoryID, c.LocalContent)
		if c.Reason != "" {
			fmt.Fprintf(&sb, " — Reason: %s", c.Reason)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Resolve via Web Console (`Conflicts` panel) or MCP `team_rules` (`action=\"resolve_conflict\"`, `id`, `decision=\"adopt_team\"` or `\"local_override\"`).\n")
	return sb.String()
}

func (s *Service) DetectRedundantLocalMemories(ctx context.Context, rawScope string) ([]domain.RuleDuplicateSuggestion, string, error) {
	if s == nil || s.db == nil || s.mem == nil || s.router == nil {
		return nil, "", nil
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return nil, "", nil
	}

	allTeam, err := s.ListTeamRules(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	activeTeam := make([]domain.TeamRule, 0, len(allTeam))
	for _, r := range allTeam {
		if r.Resolution != domain.RuleStateLocalOverride {
			activeTeam = append(activeTeam, r)
		}
	}
	if len(activeTeam) == 0 {
		return nil, "", nil
	}

	localFacts, err := s.mem.LoadActiveFacts(ctx, sc, sc, "")
	if err != nil || len(localFacts) == 0 {
		return nil, "", err
	}

	ev := s.router.EvalForTask(ctx, domain.TaskConsolidate)
	if !domain.EvaluatorSupportsTask(ev, domain.TaskConsolidate) {
		return nil, "", fmt.Errorf("no generative evaluation engine available for Team Rule redundancy detection")
	}
	planner, ok := ev.(domain.RulePackPlanner)
	if !ok {
		return nil, "", fmt.Errorf("active engine %q does not support redundancy analysis", ev.Name())
	}

	evalStart := time.Now()
	rawSuggestions, err := planner.FindRedundantLocalMemories(ctx, sc, activeTeam, localFacts)
	s.router.RecordTelemetry(ctx, domain.TaskConsolidate, ev, evalStart, len(localFacts), err)
	if err != nil {
		return nil, ev.Name(), err
	}

	var clean []domain.RuleDuplicateSuggestion
	for _, sug := range rawSuggestions {
		if sug.Kind == "partial_overlap" {
			trimmed := strings.TrimSpace(sug.TrimmedContent)
			if trimmed == "" || domain.CheckSecurityGuard(trimmed) != "" {
				continue
			}
			if len([]rune(trimmed)) > maxTrimmedLocalRunes {
				trimmed = strings.TrimSpace(string([]rune(trimmed)[:maxTrimmedLocalRunes]))
			}
			sug.TrimmedContent = trimmed
		}
		clean = append(clean, sug)
	}
	return clean, ev.Name(), nil
}

func (s *Service) ApplyDuplicateCleanup(ctx context.Context, rawScope string, actions []DuplicateCleanupAction) (DuplicateCleanupResult, error) {
	var res DuplicateCleanupResult
	if s == nil || s.mem == nil {
		return res, fmt.Errorf("memory service is unavailable")
	}
	sc := domain.NormalizeScope(rawScope)
	ids := make([]int64, 0, len(actions))
	for _, act := range actions {
		if act.LocalMemoryID > 0 {
			ids = append(ids, act.LocalMemoryID)
		}
	}
	recs, err := s.mem.GetRecordsByIDs(ctx, ids)
	if err != nil {
		return res, err
	}
	byID := make(map[int64]domain.MemoryRecord, len(recs))
	for _, rec := range recs {
		byID[rec.ID] = rec
	}
	for _, act := range actions {
		rec, ok := byID[act.LocalMemoryID]
		if !ok || !rec.Active || (strings.HasPrefix(sc, "project:") && rec.Scope != sc) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(act.Action)) {
		case "delete", "forget", "remove":
			n, err := s.mem.ForgetMemories(ctx, []int64{act.LocalMemoryID})
			if err != nil {
				return res, err
			}
			if n > 0 {
				res.DeletedIDs = append(res.DeletedIDs, act.LocalMemoryID)
			}
		case "trim", "reduce":
			trimmed := strings.TrimSpace(act.TrimmedContent)
			if trimmed == "" {
				return res, fmt.Errorf("trimmed_content cannot be empty for local memory #%d", act.LocalMemoryID)
			}
			if reason := domain.CheckSecurityGuard(trimmed); reason != "" {
				return res, fmt.Errorf("security guard: %s", reason)
			}
			if len([]rune(trimmed)) > maxTrimmedLocalRunes {
				return res, fmt.Errorf("trimmed_content exceeds %d runes for memory #%d", maxTrimmedLocalRunes, act.LocalMemoryID)
			}
			if err := s.mem.UpdateMemoryFull(ctx, act.LocalMemoryID, "", "", nil, trimmed, nil); err != nil {
				return res, err
			}
			res.TrimmedIDs = append(res.TrimmedIDs, act.LocalMemoryID)
		}
	}
	return res, nil
}
