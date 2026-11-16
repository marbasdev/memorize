package dashboard_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/rulepack"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

type teamUITestEval struct {
	noopEval
	mutateFn     func(scope, cand string, existing []domain.Fact) domain.MutationDecision
	draftFn      func(scope, raw, cat, sub string, outline []domain.RuleOutlineEntry) domain.RulePlacementProposal
	redundancyFn func(scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) []domain.RuleDuplicateSuggestion
}

func (e *teamUITestEval) EvaluateMutation(ctx context.Context, scope, cand string, existing []domain.Fact) (domain.MutationDecision, error) {
	if e.mutateFn != nil {
		return e.mutateFn(scope, cand, existing), nil
	}
	return e.noopEval.EvaluateMutation(ctx, scope, cand, existing)
}

func (e *teamUITestEval) DraftRulePackProposal(_ context.Context, scope, rawRuleText, sourceCat, sourceSub string, outline []domain.RuleOutlineEntry) (domain.RulePlacementProposal, error) {
	if e.draftFn != nil {
		return e.draftFn(scope, rawRuleText, sourceCat, sourceSub, outline), nil
	}
	afterKey := ""
	if len(outline) > 0 {
		afterKey = outline[len(outline)-1].RuleKey
	}
	normCat := domain.NormalizeCategoryForScope(scope, sourceCat)
	return domain.RulePlacementProposal{
		RuleKey:     domain.ComputeRuleKey(normCat, rawRuleText),
		Category:    normCat,
		SubCategory: sourceSub,
		Title:       rawRuleText,
		AfterKey:    afterKey,
	}, nil
}

func (e *teamUITestEval) FindRedundantLocalMemories(_ context.Context, scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) ([]domain.RuleDuplicateSuggestion, error) {
	if e.redundancyFn != nil {
		return e.redundancyFn(scope, teamRules, localFacts), nil
	}
	return nil, nil
}

func runGitForUITest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func initOriginAndTwoClones(t *testing.T) (bareDir, devADir, devBDir string) {
	t.Helper()
	tmp := t.TempDir()
	bareDir = filepath.Join(tmp, "origin.git")
	devADir = filepath.Join(tmp, "dev-a")
	devBDir = filepath.Join(tmp, "dev-b")

	runGitForUITest(t, tmp, "init", "--bare", "--initial-branch=main", bareDir)
	runGitForUITest(t, tmp, "clone", bareDir, devADir)
	runGitForUITest(t, devADir, "config", "user.name", "Alice Reviewer")
	runGitForUITest(t, devADir, "config", "user.email", "alice@example.com")
	if err := os.WriteFile(filepath.Join(devADir, "README.md"), []byte("# Team UI Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForUITest(t, devADir, "add", "README.md")
	runGitForUITest(t, devADir, "commit", "-m", "init")
	runGitForUITest(t, devADir, "branch", "-M", "main")
	runGitForUITest(t, devADir, "push", "-u", "origin", "main")

	runGitForUITest(t, tmp, "clone", bareDir, devBDir)
	runGitForUITest(t, devBDir, "config", "user.name", "Bob Teammate")
	runGitForUITest(t, devBDir, "config", "user.email", "bob@example.com")
	return bareDir, devADir, devBDir
}

func TestDashboardTeamRulesHTMXWorkflowAndLineBudget(t *testing.T) {
	ctx := context.Background()
	_, devADir, devBDir := initOriginAndTwoClones(t)

	ev := &teamUITestEval{}
	deps := newTestDeps(t, filepath.Join(t.TempDir(), "mem.db"), ev, "", thttp.DashboardHooks{})
	rpSvc := rulepack.New(deps.Memory)
	deps.Memory.AttachExtraFactsProvider(rpSvc)
	deps.Guardrail.AttachRulePack(rpSvc)
	deps.RulePack = rpSvc

	// Verify project-only facet discovery when team_rules has a scope without any local memories row.
	_, devFacetRepo, devFacetRemote := initOriginAndTwoClones(t)
	rpRemote := newTestDeps(t, filepath.Join(t.TempDir(), "remote.db"), &teamUITestEval{}, "", thttp.DashboardHooks{})
	rpRemoteSvc := rulepack.New(rpRemote.Memory)
	if _, err := rpRemoteSvc.PublishProposal(ctx, rulepack.PublishInput{
		Scope:   "project:facetonly",
		RepoDir: devFacetRemote,
		Proposal: domain.RulePlacementProposal{
			Category: "architecture",
			Title:    "Seed team_rules scope facet discovery",
		},
	}); err != nil {
		t.Fatalf("seed facetonly remote: %v", err)
	}
	sqlite.RememberRepoPath(ctx, deps.Memory.DB(), "project:facetonly", devFacetRepo)
	if _, err := rpSvc.SyncExplicit(ctx, "project:facetonly", devFacetRepo, true); err != nil {
		t.Fatalf("sync facetonly: %v", err)
	}
	facets, err := deps.Memory.GetDashboardFacets(ctx)
	if err != nil {
		t.Fatalf("GetDashboardFacets: %v", err)
	}
	foundFacetOnly := false
	for _, sc := range facets.Scopes {
		if sc == "project:facetonly" {
			foundFacetOnly = true
			break
		}
	}
	if !foundFacetOnly {
		t.Fatalf("expected project:facetonly in GetDashboardFacets.Scopes, got %v", facets.Scopes)
	}

	// Main Team Rules UI repo & memories for project:teamui
	const projScope = "project:teamui"
	sqlite.RememberRepoPath(ctx, deps.Memory.DB(), projScope, devADir)

	// 1. Seed a global memory + 4 project local memories.
	if _, err := deps.Memory.CommitMemoryFull(ctx, "Global naming rule should not get project promote button", "convention", "naming", "", "global"); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Memory.CommitMemoryFull(ctx, "Always run go test -race before merge", "testing", "go", "", projScope); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Memory.CommitMemoryFull(ctx, "Run race detector tests before merging PRs", "testing", "go", "", projScope); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Memory.CommitMemoryFull(ctx, "Always use CGO_ENABLED=0 and keep local debug port 19090", "database", "sqlite", "", projScope); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Memory.CommitMemoryFull(ctx, "Use PostgreSQL in local staging", "database", "postgres", "", projScope); err != nil {
		t.Fatal(err)
	}

	facts, err := deps.Memory.LoadActiveFacts(ctx, projScope, projScope, "")
	if err != nil || len(facts) != 4 {
		t.Fatalf("expected 4 active local facts in %s, got %d (err=%v)", projScope, len(facts), err)
	}
	var promoteID, dupID, partialID, conflictID int64
	for _, f := range facts {
		switch {
		case strings.Contains(f.Content, "go test -race"):
			promoteID = f.ID
		case strings.Contains(f.Content, "merging PRs"):
			dupID = f.ID
		case strings.Contains(f.Content, "debug port 19090"):
			partialID = f.ID
		case strings.Contains(f.Content, "PostgreSQL"):
			conflictID = f.ID
		}
	}
	if promoteID == 0 || dupID == 0 || partialID == 0 || conflictID == 0 {
		t.Fatalf("failed to resolve seeded local memory IDs from %+v", facts)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 2. GET / renders Team Rules tab + project-only promote button (global row has none).
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	var rootBuf strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		rootBuf.WriteString(sc.Text())
		rootBuf.WriteByte('\n')
	}
	_ = resp.Body.Close()
	rootHTML := rootBuf.String()
	for _, want := range []string{`id="panel-team"`, `id="teamSection"`, `id="tabBtn-team"`, "/ui/modal/team-promote?id="} {
		if !strings.Contains(rootHTML, want) {
			t.Fatalf("GET / missing %q", want)
		}
	}
	if strings.Contains(rootHTML, "/ui/modal/team-promote?id=1&") {
		t.Fatalf("global memory #1 must not expose project team-promote button")
	}

	// 3. POST /ui/team/config persists normalized @git handle.
	code, cfgHTML := doHX(t, http.MethodPost, ts.URL+"/ui/team/config", url.Values{
		"team_scope": {projScope},
		"author":     {"Alice Lead"},
	})
	if code != http.StatusOK || !strings.Contains(cfgHTML, "@Alice-Lead") {
		t.Fatalf("POST /ui/team/config failed (%d):\n%s", code, cfgHTML)
	}
	if got := rpSvc.AuthorName(ctx, "", devADir); got != "@Alice-Lead" {
		t.Fatalf("expected author @Alice-Lead, got %q", got)
	}

	// 4. POST /ui/modal/team-draft returns pre-filled modal_team_publish without rule_key, added_by input, or [invariant].
	code, draftHTML := doHX(t, http.MethodPost, ts.URL+"/ui/modal/team-draft", url.Values{
		"team_scope":   {projScope},
		"category":     {"testing"},
		"sub_category": {"go"},
		"raw_rule":     {"Enforce clean test isolation"},
	})
	if code != http.StatusOK || !strings.Contains(draftHTML, `id="teamPublishDialog"`) || !strings.Contains(draftHTML, "Enforce clean test isolation") {
		t.Fatalf("POST /ui/modal/team-draft failed (%d):\n%s", code, draftHTML)
	}
	if strings.Contains(draftHTML, `name="rule_key"`) || strings.Contains(draftHTML, `name="added_by"`) || strings.Contains(draftHTML, `name="pr_fallback"`) || strings.Contains(draftHTML, "[invariant]") {
		t.Fatalf("team draft modal must not expose rule_key, manual added_by input, pr_fallback, or [invariant] tier:\n%s", draftHTML)
	}

	// 5. GET /ui/modal/team-promote loads local memory #promoteID and checks retire_local.
	const raceTitle = "Always run go test -race before merge"
	raceKey := domain.ComputeRuleKey("testing", raceTitle)
	code, promHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/team-promote?id="+url.QueryEscape(strconv.FormatInt(promoteID, 10))+"&team_scope="+url.QueryEscape(projScope), nil)
	if code != http.StatusOK || !strings.Contains(promHTML, `name="retire_local"`) || !strings.Contains(promHTML, raceTitle) {
		t.Fatalf("GET /ui/modal/team-promote failed (%d):\n%s", code, promHTML)
	}
	if strings.Contains(promHTML, `name="rule_key"`) || strings.Contains(promHTML, `name="added_by"`) || strings.Contains(promHTML, `name="pr_fallback"`) || strings.Contains(promHTML, "[invariant]") {
		t.Fatalf("team promote modal must not expose rule_key, manual added_by input, pr_fallback, or [invariant] tier:\n%s", promHTML)
	}

	// 6. POST /ui/team/publish auto-populates @Alice-Lead without added_by param, writes 1-line .memorize/TEAM_RULES.md, pushes to origin, and closes modal.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/ui/team/publish", strings.NewReader(url.Values{
		"team_scope":      {projScope},
		"local_memory_id": {strconv.FormatInt(promoteID, 10)},
		"category":        {"testing"},
		"sub_category":    {"go"},
		"title":           {raceTitle},
		"retire_local":    {"1"},
	}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	pubResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var pubBuf strings.Builder
	pubSc := bufio.NewScanner(pubResp.Body)
	for pubSc.Scan() {
		pubBuf.WriteString(pubSc.Text())
		pubBuf.WriteByte('\n')
	}
	_ = pubResp.Body.Close()
	pubHTML := pubBuf.String()
	hxTrig := pubResp.Header.Get("HX-Trigger")
	if pubResp.StatusCode != http.StatusOK || !strings.Contains(pubHTML, raceTitle) || !strings.Contains(hxTrig, "memorizeCloseModal") {
		t.Fatalf("POST /ui/team/publish failed status=%d HX-Trigger=%q:\n%s", pubResp.StatusCode, hxTrig, pubHTML)
	}
	if strings.Contains(pubHTML, `class="team-rule-key"`) || strings.Contains(pubHTML, "[invariant]") || strings.Contains(pubHTML, "[rule]") || !strings.Contains(pubHTML, `hx-target="#modalHost"`) {
		t.Fatalf("team UI must not render rule_key/tier tags and must target #modalHost:\n%s", pubHTML)
	}

	activeAfter, err := deps.Memory.LoadActiveFacts(ctx, projScope, projScope, "")
	if err != nil {
		t.Fatalf("LoadActiveFacts: %v", err)
	}
	for _, f := range activeAfter {
		if f.ID == promoteID {
			t.Fatalf("expected promoted local memory #%d to be retired", promoteID)
		}
	}

	// Verify devB pulls the pushed clean 1-line rule from bare origin (no key, no [invariant]/[rule]).
	runGitForUITest(t, devBDir, "pull", "origin", "main")
	rawRemote, err := os.ReadFile(filepath.Join(devBDir, domain.RulePackRelPath))
	if err != nil || !strings.Contains(string(rawRemote), raceTitle) || !strings.Contains(string(rawRemote), "@Alice-Lead") {
		t.Fatalf("expected origin/devB to have clean rule %q by @Alice-Lead, err=%v content=%s", raceTitle, err, string(rawRemote))
	}
	if strings.Contains(string(rawRemote), "[invariant]") || strings.Contains(string(rawRemote), "[rule]") || strings.Contains(string(rawRemote), raceKey) {
		t.Fatalf("remote TEAM_RULES.md must not expose rule_key or tier tags:\n%s", string(rawRemote))
	}

	// 7. Teammate (devB) pushes a conflicting rule to origin; POST /ui/team/sync pulls and flags conflict_pending.
	const dbTitle = "Use pure-Go modernc SQLite without CGO"
	dbKey := domain.ComputeRuleKey("database", dbTitle)
	teammateDeps := newTestDeps(t, filepath.Join(t.TempDir(), "teammate.db"), &teamUITestEval{}, "", thttp.DashboardHooks{})
	teammateRP := rulepack.New(teammateDeps.Memory)
	if _, err := teammateRP.PublishProposal(ctx, rulepack.PublishInput{
		Scope:   projScope,
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			Category: "database",
			Title:    dbTitle,
			AddedBy:  "Bob Teammate",
		},
	}); err != nil {
		t.Fatalf("teammate publish: %v", err)
	}

	ev.mutateFn = func(_ string, cand string, existing []domain.Fact) domain.MutationDecision {
		if strings.Contains(cand, "modernc SQLite") {
			for _, f := range existing {
				if f.ID == conflictID {
					return domain.MutationDecision{Action: "OVERWRITE", ConflictFactID: conflictID, Confidence: 0.94}
				}
			}
		}
		return domain.MutationDecision{Action: "INSERT", Category: "architecture", Confidence: 0.9}
	}
	code, syncHTML := doHX(t, http.MethodPost, ts.URL+"/ui/team/sync", url.Values{"team_scope": {projScope}})
	if code != http.StatusOK || !strings.Contains(syncHTML, dbTitle) || !strings.Contains(syncHTML, "1 conflict(s)") || strings.Contains(syncHTML, "["+dbKey+"]") {
		t.Fatalf("POST /ui/team/sync expected clean pending conflict for %q, got (%d):\n%s", dbTitle, code, syncHTML)
	}

	// 8. POST /ui/team/conflict resolves via local_override and clears pending conflict banner.
	code, confHTML := doHX(t, http.MethodPost, ts.URL+"/ui/team/conflict", url.Values{
		"team_scope": {projScope},
		"rule_key":   {dbKey},
		"decision":   {domain.RuleStateLocalOverride},
		"note":       {"Keep local staging override for integration test"},
	})
	if code != http.StatusOK || strings.Contains(confHTML, "1 conflict(s)") || !strings.Contains(confHTML, "local_override") {
		t.Fatalf("POST /ui/team/conflict failed (%d):\n%s", code, confHTML)
	}

	// 9. GET /ui/modal/team-duplicates + POST /ui/team/duplicates/apply delete duplicate & trim partial overlap without touching team_rules.
	ev.redundancyFn = func(_ string, _ []domain.TeamRule, _ []domain.Fact) []domain.RuleDuplicateSuggestion {
		return []domain.RuleDuplicateSuggestion{
			{
				LocalMemoryID: dupID,
				LocalCategory: "testing",
				LocalContent:  "Run race detector tests before merging PRs",
				TeamRuleKey:   raceKey,
				TeamRuleTitle: raceTitle,
				Kind:          "duplicate",
				Reason:        "Fully covered by team rule",
			},
			{
				LocalMemoryID:  partialID,
				LocalCategory:  "database",
				LocalContent:   "Always use CGO_ENABLED=0 and keep local debug port 19090",
				TeamRuleKey:    dbKey,
				TeamRuleTitle:  dbTitle,
				Kind:           "partial_overlap",
				TrimmedContent: "Keep local debug port 19090",
				Reason:         "Remove CGO part covered by team rule; retain personal port preference",
			},
		}
	}
	code, dupModalHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/team-duplicates?team_scope="+url.QueryEscape(projScope), nil)
	if code != http.StatusOK || !strings.Contains(dupModalHTML, `id="teamDupesDialog"`) || !strings.Contains(dupModalHTML, "Keep local debug port 19090") || strings.Contains(dupModalHTML, "["+raceKey+"]") {
		t.Fatalf("GET /ui/modal/team-duplicates failed (%d):\n%s", code, dupModalHTML)
	}

	rulesBefore, _ := rpSvc.ListTeamRules(ctx, projScope)
	applyForm := url.Values{
		"team_scope": {projScope},
		"ids":        {strconv.FormatInt(dupID, 10), strconv.FormatInt(partialID, 10)},
	}
	applyForm.Set("action_"+strconv.FormatInt(dupID, 10), "delete")
	applyForm.Set("action_"+strconv.FormatInt(partialID, 10), "trim")
	applyForm.Set("trimmed_"+strconv.FormatInt(partialID, 10), "Keep local debug port 19090")

	code, _ = doHX(t, http.MethodPost, ts.URL+"/ui/team/duplicates/apply", applyForm)
	if code != http.StatusOK {
		t.Fatalf("POST /ui/team/duplicates/apply failed: %d", code)
	}
	activeAfterCleanup, err := deps.Memory.LoadActiveFacts(ctx, projScope, projScope, "")
	if err != nil {
		t.Fatalf("LoadActiveFacts: %v", err)
	}
	activeByID := make(map[int64]domain.Fact, len(activeAfterCleanup))
	for _, f := range activeAfterCleanup {
		activeByID[f.ID] = f
	}
	if _, exists := activeByID[dupID]; exists {
		t.Fatalf("expected duplicate local memory #%d to be deleted/deactivated", dupID)
	}
	if trimmed, ok := activeByID[partialID]; !ok || trimmed.Content != "Keep local debug port 19090" {
		t.Fatalf("expected partial local memory #%d trimmed to 'Keep local debug port 19090', got %+v", partialID, trimmed)
	}
	rulesAfter, _ := rpSvc.ListTeamRules(ctx, projScope)
	if len(rulesAfter) != len(rulesBefore) {
		t.Fatalf("team_rules must stay untouched during duplicate cleanup: before=%d after=%d", len(rulesBefore), len(rulesAfter))
	}

	// 10. GET /ui/modal/team-edit + POST /ui/team/update + POST /ui/team/delete update and remove remote rules cleanly without manual added_by or pr_fallback input.
	code, editHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/team-edit?team_scope="+url.QueryEscape(projScope)+"&rule_key="+url.QueryEscape(raceKey), nil)
	if code != http.StatusOK || !strings.Contains(editHTML, raceTitle) || !strings.Contains(editHTML, "/ui/team/update") || strings.Contains(editHTML, `name="added_by"`) || strings.Contains(editHTML, `name="pr_fallback"`) {
		t.Fatalf("GET /ui/modal/team-edit failed (%d):\n%s", code, editHTML)
	}

	const updatedRaceTitle = "Always run go test -race -count=1 before merge"
	updReq, err := http.NewRequest(http.MethodPost, ts.URL+"/ui/team/update", strings.NewReader(url.Values{
		"team_scope":   {projScope},
		"target_key":   {raceKey},
		"category":     {"testing"},
		"sub_category": {"go"},
		"title":        {updatedRaceTitle},
	}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	updReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updReq.Header.Set("HX-Request", "true")
	updResp, err := http.DefaultClient.Do(updReq)
	if err != nil {
		t.Fatal(err)
	}
	var updBuf strings.Builder
	updSc := bufio.NewScanner(updResp.Body)
	for updSc.Scan() {
		updBuf.WriteString(updSc.Text())
		updBuf.WriteByte('\n')
	}
	_ = updResp.Body.Close()
	updHTML := updBuf.String()
	updTrig := updResp.Header.Get("HX-Trigger")
	if updResp.StatusCode != http.StatusOK || !strings.Contains(updHTML, updatedRaceTitle) || !strings.Contains(updTrig, "memorizeCloseModal") {
		t.Fatalf("POST /ui/team/update failed status=%d HX-Trigger=%q:\n%s", updResp.StatusCode, updTrig, updHTML)
	}

	runGitForUITest(t, devBDir, "pull", "origin", "main")
	rawAfterUpd, err := os.ReadFile(filepath.Join(devBDir, domain.RulePackRelPath))
	if err != nil || !strings.Contains(string(rawAfterUpd), updatedRaceTitle) || strings.Contains(string(rawAfterUpd), raceTitle) {
		t.Fatalf("expected updated rule %q and removed old title in origin, err=%v content=%s", updatedRaceTitle, err, string(rawAfterUpd))
	}

	updatedRaceKey := domain.ComputeRuleKey("testing", updatedRaceTitle)
	code, delHTML := doHX(t, http.MethodPost, ts.URL+"/ui/team/delete", url.Values{
		"team_scope": {projScope},
		"rule_key":   {updatedRaceKey},
	})
	if code != http.StatusOK || strings.Contains(delHTML, updatedRaceTitle) || !strings.Contains(delHTML, dbTitle) {
		t.Fatalf("POST /ui/team/delete failed (%d):\n%s", code, delHTML)
	}

	runGitForUITest(t, devBDir, "pull", "origin", "main")
	rawAfterDel, err := os.ReadFile(filepath.Join(devBDir, domain.RulePackRelPath))
	if err != nil || strings.Contains(string(rawAfterDel), updatedRaceTitle) || !strings.Contains(string(rawAfterDel), dbTitle) {
		t.Fatalf("expected deleted rule %q absent and %q kept in origin, err=%v content=%s", updatedRaceTitle, dbTitle, err, string(rawAfterDel))
	}

	// 11. Core Rule #26: all custom UI templates & assets stay < 800 lines.
	var customUIFiles []string
	tmplMatches, _ := filepath.Glob("templates/*.html")
	customUIFiles = append(customUIFiles, tmplMatches...)
	for _, asset := range []string{"assets/ui.js", "assets/theme.js", "assets/styles.css"} {
		customUIFiles = append(customUIFiles, asset)
	}
	for _, p := range customUIFiles {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("read UI file %s: %v", p, rerr)
		}
		lines := strings.Count(string(data), "\n")
		if lines >= 800 {
			t.Fatalf("custom UI file %s has %d lines (>= 800 limit)", p, lines)
		}
	}
}
