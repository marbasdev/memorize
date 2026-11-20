package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"praxis/internal/domain"
	"praxis/internal/modules/rulepack"
)

type ssrTeamCategoryGroup struct {
	Category     string
	HeadingTitle string
	Rules        []domain.TeamRule
}

type ssrTeamData struct {
	Lang             string
	ServerMode       bool
	CanAdmin         bool
	ProjectScopes    []string
	SelectedScope    string
	RepoRoot         string
	Author           string
	FileHash         string
	ShortHash        string
	LastError        string
	TotalRules       int
	PendingConflicts int
	Groups           []ssrTeamCategoryGroup
	Conflicts        []domain.RulePackConflict
	Categories       []string
	AllSubCategories map[string][]string
}

type ssrTeamPublishModalData struct {
	Lang             string
	Scope            string
	ProjectScopes    []string
	LocalMemoryID    int64
	LocalContent     string
	Proposal         domain.RulePlacementProposal
	Outline          []domain.RuleOutlineEntry
	Author           string
	RetireLocal      bool
	Categories       []string
	AllSubCategories map[string][]string
}

type ssrTeamDuplicatesModalData struct {
	Lang        string
	Scope       string
	SourceLabel string
	Suggestions []domain.RuleDuplicateSuggestion
}

type ssrTeamEditModalData struct {
	Lang             string
	Scope            string
	Rule             domain.TeamRule
	Categories       []string
	AllSubCategories map[string][]string
}

func collectProjectScopes(ctx context.Context, d Deps) []string {
	seen := map[string]bool{}
	if d.Memory != nil {
		if facets, err := d.Memory.GetDashboardFacets(ctx); err == nil {
			for _, sc := range facets.Scopes {
				if strings.HasPrefix(sc, "project:") {
					seen[sc] = true
				}
			}
		}
		for _, sc := range d.Memory.ListSyncedProjectScopes(ctx) {
			seen[sc] = true
		}
	}
	out := make([]string, 0, len(seen))
	for sc := range seen {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out
}

func resolveTeamScope(ctx context.Context, d Deps, q url.Values, projScopes []string) string {
	cand := strings.TrimSpace(q.Get("team_scope"))
	if cand == "" {
		cand = strings.TrimSpace(q.Get("scope"))
	}
	if cand != "" {
		norm := domain.NormalizeScope(cand)
		if strings.HasPrefix(norm, "project:") {
			return norm
		}
	}
	if d.Memory != nil {
		for _, sc := range projScopes {
			if st, err := d.RulePack.SyncTurn(ctx, sc, ""); err == nil && st.RuleCount > 0 {
				return sc
			}
		}
	}
	if len(projScopes) > 0 {
		return projScopes[0]
	}
	return ""
}

func buildTeamView(ctx context.Context, d Deps, lang string, q url.Values) ssrTeamData {
	projScopes := collectProjectScopes(ctx, d)
	selected := resolveTeamScope(ctx, d, q, projScopes)
	view := ssrTeamData{
		Lang:          lang,
		ServerMode:    d.ServerMode || domain.IsServerMode(ctx),
		CanAdmin:      domain.CanAccessAdminFeatures(ctx),
		ProjectScopes: projScopes,
		SelectedScope: selected,
	}
	if d.Memory != nil {
		if facets, err := d.Memory.GetDashboardFacets(ctx); err == nil {
			view.Categories = facets.Categories
			view.AllSubCategories = facets.SubCategories
		}
	}
	if d.RulePack == nil || selected == "" {
		return view
	}

	if d.Memory != nil {
		repoRoot := ""
		if !view.ServerMode {
			repoRoot = d.Memory.ResolveScopeRepoPath(ctx, selected, "")
			view.RepoRoot = repoRoot
		}
		view.Author = d.RulePack.AuthorName(ctx, "", repoRoot)
		view.FileHash, view.LastError = d.RulePack.SyncMeta(ctx, selected)
		if len(view.FileHash) > 12 {
			view.ShortHash = view.FileHash[:12]
		} else {
			view.ShortHash = view.FileHash
		}
	}

	rules, err := d.RulePack.ListTeamRules(ctx, selected)
	if err == nil {
		view.TotalRules = len(rules)
		byCat := make(map[string]*ssrTeamCategoryGroup)
		var catOrder []string
		for _, r := range rules {
			if _, exists := byCat[r.Category]; !exists {
				catOrder = append(catOrder, r.Category)
				byCat[r.Category] = &ssrTeamCategoryGroup{
					Category:     r.Category,
					HeadingTitle: domain.FormatCategoryHeadingTitle(r.Category),
				}
			}
			byCat[r.Category].Rules = append(byCat[r.Category].Rules, r)
		}
		for _, cat := range catOrder {
			view.Groups = append(view.Groups, *byCat[cat])
		}
	}

	conflicts, err := d.RulePack.ListConflicts(ctx, selected, false)
	if err == nil {
		view.Conflicts = conflicts
		view.PendingConflicts = len(conflicts)
	}
	return view
}

func teamHX(d Deps, gate func(http.HandlerFunc) http.HandlerFunc, fn func(w http.ResponseWriter, r *http.Request, lang, sc string)) http.HandlerFunc {
	return gate(func(w http.ResponseWriter, r *http.Request) {
		if d.RulePack == nil {
			http.Error(w, "team rule pack service unavailable", http.StatusInternalServerError)
			return
		}
		rawScope := r.FormValue("team_scope")
		if r.Method == http.MethodGet {
			rawScope = r.URL.Query().Get("team_scope")
		}
		fn(w, r, requestLang(r), domain.NormalizeScope(rawScope))
	})
}

func renderTeamSection(w http.ResponseWriter, r *http.Request, d Deps, lang string, vals url.Values, toast string, closeModal bool) {
	view := buildTeamView(r.Context(), d, lang, vals)
	setHXTriggers(w, toast, closeModal, false)
	writeHTMLTemplate(w, "team_section", view)
}

func rejectServerTeamGit(d Deps, w http.ResponseWriter, r *http.Request) bool {
	if d.ServerMode || domain.IsServerMode(r.Context()) {
		http.Error(w, "team rule git operations must run from connected client repositories rather than the team server", http.StatusBadRequest)
		return true
	}
	return false
}

func registerSSRTeamRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/team", getHX(func(w http.ResponseWriter, r *http.Request) {
		view := buildTeamView(r.Context(), d, requestLang(r), r.URL.Query())
		writeHTMLTemplate(w, "team_section", view)
	}))

	mux.HandleFunc("/ui/team/config", teamHX(d, func(next http.HandlerFunc) http.HandlerFunc {
		return postHX(requireAdminRoute(next))
	}, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if !strings.HasPrefix(sc, "project:") {
			http.Error(w, "select a project:* scope first", http.StatusBadRequest)
			return
		}
		if _, hasAuthor := r.Form["author"]; hasAuthor {
			author := strings.TrimSpace(r.FormValue("author"))
			if err := d.RulePack.SetAuthorName(r.Context(), author); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		toast := langText(lang, fmt.Sprintf("Saved team settings for %s", sc), fmt.Sprintf("%s 팀 룰 설정 저장 완료", sc))
		renderTeamSection(w, r, d, lang, r.Form, toast, false)
	}))

	mux.HandleFunc("/ui/team/sync", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		ctx := r.Context()
		st, err := d.RulePack.SyncExplicit(ctx, sc, "", true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if st.Changed && len(st.ChangedKeys) > 0 {
			if _, serr := d.RulePack.ScanConflictsForSyncedRules(ctx, sc, st.ChangedKeys); serr == nil {
				st.PendingConflicts = d.RulePack.CountPendingConflicts(ctx, sc)
			}
		}
		toast := langText(lang,
			fmt.Sprintf("Synced %d team rules (%s), %d conflicts", st.RuleCount, st.SyncedFrom, st.PendingConflicts),
			fmt.Sprintf("팀 룰 %d건 싱크 완료 (%s), 충돌 %d건", st.RuleCount, st.SyncedFrom, st.PendingConflicts),
		)
		renderTeamSection(w, r, d, lang, r.Form, toast, false)
	}))

	mux.HandleFunc("/ui/team/conflict", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		key := strings.TrimSpace(r.FormValue("rule_key"))
		decision := strings.TrimSpace(r.FormValue("decision"))
		note := strings.TrimSpace(r.FormValue("note"))
		if err := d.RulePack.ResolveConflict(r.Context(), sc, key, decision, note); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang, fmt.Sprintf("Conflict resolved (%s)", decision), fmt.Sprintf("충돌 해결 완료 (%s)", decision))
		renderTeamSection(w, r, d, lang, r.Form, toast, false)
	}))

	mux.HandleFunc("/ui/modal/team-draft", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		ctx := r.Context()
		rawRule := strings.TrimSpace(r.FormValue("raw_rule"))
		cat := strings.TrimSpace(r.FormValue("category"))
		sub := strings.TrimSpace(r.FormValue("sub_category"))

		prop, err := d.RulePack.DraftProposalFromText(ctx, sc, "", rawRule, cat, sub)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		outline, _, _ := d.RulePack.DocOutline(ctx, sc, "")
		facets, _ := d.Memory.GetDashboardFacets(ctx)
		writeHTMLTemplate(w, "modal_team_publish", ssrTeamPublishModalData{
			Lang:             lang,
			Scope:            sc,
			ProjectScopes:    collectProjectScopes(ctx, d),
			Proposal:         prop,
			Outline:          outline,
			Author:           prop.AddedBy,
			Categories:       facets.Categories,
			AllSubCategories: facets.SubCategories,
		})
	}))

	mux.HandleFunc("/ui/modal/team-promote", teamHX(d, getHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		ctx := r.Context()
		id := parsePositiveInt64(r.URL.Query().Get("id"))
		projScopes := collectProjectScopes(ctx, d)
		reqScope := sc
		if reqScope == "" {
			reqScope = resolveTeamScope(ctx, d, r.URL.Query(), projScopes)
		}
		prop, srcFact, err := d.RulePack.PromoteLocalMemory(ctx, id, reqScope, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		targetScope := domain.NormalizeScope(reqScope)
		if !strings.HasPrefix(targetScope, "project:") && strings.HasPrefix(srcFact.Scope, "project:") {
			targetScope = srcFact.Scope
		}
		outline, _, _ := d.RulePack.DocOutline(ctx, targetScope, "")
		facets, _ := d.Memory.GetDashboardFacets(ctx)
		writeHTMLTemplate(w, "modal_team_publish", ssrTeamPublishModalData{
			Lang:             lang,
			Scope:            targetScope,
			ProjectScopes:    projScopes,
			LocalMemoryID:    srcFact.ID,
			LocalContent:     srcFact.Content,
			Proposal:         prop,
			Outline:          outline,
			Author:           prop.AddedBy,
			RetireLocal:      true,
			Categories:       facets.Categories,
			AllSubCategories: facets.SubCategories,
		})
	}))

	mux.HandleFunc("/ui/team/publish", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		localID := parsePositiveInt64(r.FormValue("local_memory_id"))
		retireLocal := r.FormValue("retire_local") == "1" || r.FormValue("retire_local") == "true"

		addedAt := strings.TrimSpace(r.FormValue("added_at"))
		if addedAt == "" {
			addedAt = time.Now().Format("2006-01-02")
		}
		prop := domain.RulePlacementProposal{
			Category:    strings.TrimSpace(r.FormValue("category")),
			SubCategory: strings.TrimSpace(r.FormValue("sub_category")),
			Pinned:      true,
			Title:       strings.TrimSpace(r.FormValue("title")),
			AddedBy:     domain.NormalizeGitHandle(r.FormValue("added_by")),
			AddedAt:     addedAt,
			AfterKey:    strings.TrimSpace(r.FormValue("after_key")),
		}

		res, err := d.RulePack.PublishWithDefaults(r.Context(), rulepack.PublishInput{
			Scope:         sc,
			Proposal:      prop,
			LocalMemoryID: localID,
		}, retireLocal)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang, fmt.Sprintf("Team Rule (%s): %s", res.Status, res.Rule.Title), fmt.Sprintf("팀 룰 반영 (%s): %s", res.Status, res.Rule.Title))
		if res.Status == "conflict_pending" && res.ConflictReason != "" {
			toast = fmt.Sprintf("Publish paused: %s", res.ConflictReason)
		}
		renderTeamSection(w, r, d, lang, r.Form, toast, true)
	}))

	mux.HandleFunc("/ui/modal/team-edit", teamHX(d, getHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		ctx := r.Context()
		key := domain.NormalizeRuleKey(r.URL.Query().Get("rule_key"))
		rules, err := d.RulePack.ListTeamRules(ctx, sc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var target domain.TeamRule
		found := false
		for _, rl := range rules {
			if rl.RuleKey == key {
				target = rl
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "team rule not found", http.StatusNotFound)
			return
		}
		facets, _ := d.Memory.GetDashboardFacets(ctx)
		writeHTMLTemplate(w, "modal_team_edit", ssrTeamEditModalData{
			Lang:             lang,
			Scope:            sc,
			Rule:             target,
			Categories:       facets.Categories,
			AllSubCategories: facets.SubCategories,
		})
	}))

	mux.HandleFunc("/ui/team/update", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		targetKey := strings.TrimSpace(r.FormValue("target_key"))
		res, err := d.RulePack.UpdateWithDefaults(r.Context(), rulepack.UpdateInput{
			Scope:     sc,
			TargetKey: targetKey,
			Updated: domain.TeamRule{
				Category:    strings.TrimSpace(r.FormValue("category")),
				SubCategory: strings.TrimSpace(r.FormValue("sub_category")),
				Title:       strings.TrimSpace(r.FormValue("title")),
				AddedBy:     domain.NormalizeGitHandle(r.FormValue("added_by")),
			},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang, fmt.Sprintf("Team Rule (%s): %s", res.Status, res.Rule.Title), fmt.Sprintf("팀 룰 수정 (%s): %s", res.Status, res.Rule.Title))
		if res.Status == "conflict_pending" && res.ConflictReason != "" {
			toast = fmt.Sprintf("Update paused: %s", res.ConflictReason)
		}
		renderTeamSection(w, r, d, lang, r.Form, toast, true)
	}))

	mux.HandleFunc("/ui/team/delete", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		if rejectServerTeamGit(d, w, r) {
			return
		}
		key := strings.TrimSpace(r.FormValue("rule_key"))
		res, err := d.RulePack.RemoveRuleRemote(r.Context(), rulepack.RemoveInput{
			Scope:     sc,
			TargetKey: key,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang, fmt.Sprintf("Removed Team Rule (%s)", res.Status), fmt.Sprintf("팀 룰 삭제 완료 (%s)", res.Status))
		renderTeamSection(w, r, d, lang, r.Form, toast, false)
	}))

	mux.HandleFunc("/ui/modal/team-duplicates", teamHX(d, getHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		sugs, src, err := d.RulePack.DetectRedundantLocalMemories(r.Context(), sc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "modal_team_duplicates", ssrTeamDuplicatesModalData{
			Lang:        lang,
			Scope:       sc,
			SourceLabel: src,
			Suggestions: sugs,
		})
	}))

	mux.HandleFunc("/ui/team/duplicates/apply", teamHX(d, postHX, func(w http.ResponseWriter, r *http.Request, lang, sc string) {
		var actions []rulepack.DuplicateCleanupAction
		for _, id := range parseFormIDs(r.Form) {
			actKind := strings.TrimSpace(r.FormValue(fmt.Sprintf("action_%d", id)))
			trimmed := strings.TrimSpace(r.FormValue(fmt.Sprintf("trimmed_%d", id)))
			if actKind == "" {
				continue
			}
			actions = append(actions, rulepack.DuplicateCleanupAction{
				LocalMemoryID:  id,
				Action:         actKind,
				TrimmedContent: trimmed,
			})
		}
		res, err := d.RulePack.ApplyDuplicateCleanup(r.Context(), sc, actions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang,
			fmt.Sprintf("Cleaned %d deleted, %d trimmed", len(res.DeletedIDs), len(res.TrimmedIDs)),
			fmt.Sprintf("정리 완료: 삭제 %d건, 축소 %d건", len(res.DeletedIDs), len(res.TrimmedIDs)),
		)
		renderTeamSection(w, r, d, lang, r.Form, toast, true)
	}))
}
