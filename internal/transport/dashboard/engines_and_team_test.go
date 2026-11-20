package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"praxis/internal/domain"
	"praxis/internal/inference/discovery"
	"praxis/internal/infra/config"
	"praxis/internal/modules/rulepack"
	thttp "praxis/internal/transport/http"
)

func TestGeminiDecisionsCatalogAndFeatureRoutingUI(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dash_routing.db")
	deps := newTestDeps(t, dbPath, noopEval{}, filepath.Join(t.TempDir(), "models"), thttp.DashboardHooks{})
	svc := deps.Memory

	ts := httptest.NewServer(NewHandler(deps))
	defer ts.Close()

	postHX := func(path string, form url.Values) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s failed: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b)
	}

	reqDec, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/engines?view=gemini&gtier=decision", nil)
	reqDec.Header.Set("HX-Request", "true")
	respDec, err := http.DefaultClient.Do(reqDec)
	if err != nil || respDec.StatusCode != http.StatusOK {
		t.Fatalf("GET decision tier failed: %v (%v)", respDec, err)
	}
	decHTMLBytes, _ := io.ReadAll(respDec.Body)
	_ = respDec.Body.Close()
	decHTML := string(decHTMLBytes)
	if !strings.Contains(decHTML, "gemini-decision-1") || !strings.Contains(decHTML, "Decision-Only (No text gen)") {
		t.Fatalf("expected gemini-decision-1 with Decision-Only badge in gtier=decision:\n%s", decHTML)
	}

	saveResp, saveHTML := postHX("/ui/engines/save", url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-decision-1"},
	})
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save(gemini-decision-1) failed status=%d body=%s", saveResp.StatusCode, saveHTML)
	}
	for _, want := range []string{
		`id="engineActiveBadge"`,
		`id="taskRoutingSection"`,
		`Decision-Model Compatible`,
		`Decision OK (0-tok)`,
		`Text-Generation Only (No Decision Models)`,
		`Text-Gen Only (No Decision)`,
		`name="gen_provider"`,
		`gemini:gemini-decision-1`,
		`Built-in Heuristic`,
	} {
		if !strings.Contains(saveHTML, want) {
			t.Fatalf("expected %q in engines HTML after saving gemini-decision-1:\n%s", want, saveHTML)
		}
	}

	badForm := url.Values{
		"task_consolidate_provider": {"gemini"},
		"task_consolidate_model":    {"gemini-decision-1"},
	}
	badResp, badBody := postHX("/ui/engines/routing", badForm)
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting gemini-decision-1 on consolidate task, got %d (%s)", badResp.StatusCode, badBody)
	}
	badGenResp, badGenBody := postHX("/ui/engines/routing", url.Values{"gen_provider": {"jev"}})
	if badGenResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting jev as generative companion, got %d (%s)", badGenResp.StatusCode, badGenBody)
	}

	okResp, okHTML := postHX("/ui/engines/routing", url.Values{
		"gen_provider":           {"gemini"},
		"gen_model":              {"gemini-3.8-flash"},
		"task_taxonomy_provider": {"local"},
		"task_taxonomy_model":    {"qwen3:4b"},
	})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid routing save, got %d (%s)", okResp.StatusCode, okHTML)
	}
	for _, want := range []string{
		`id="engineActiveBadge"`,
		`+1 custom`,
		"Custom Routing",
		"Reset All to Bulk",
		"gemini:gemini-3.8-flash",
		"local:qwen3:4b",
		"gemini:gemini-decision-1",
	} {
		if !strings.Contains(okHTML, want) {
			t.Fatalf("expected %q in custom routing response:\n%s", want, okHTML)
		}
	}

	rstResp, rstHTML := postHX("/ui/engines/routing", url.Values{"mode": {"reset_all"}})
	if rstResp.StatusCode != http.StatusOK || !strings.Contains(rstHTML, "Bulk Mode (All Inherit)") {
		t.Fatalf("expected Reset All to Bulk to restore Bulk Mode, got status=%d\n%s", rstResp.StatusCode, rstHTML)
	}

	badProgResp, badProgBody := postHX("/ui/engines/routing", url.Values{"task_progress_provider": {"jev"}})
	if badProgResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 from /ui/engines/routing when assigning jev to progress task, got %d (%s)", badProgResp.StatusCode, badProgBody)
	}

	okMixResp, okMixBody := postHX("/ui/engines/routing", url.Values{
		"gen_provider":           {"gemini"},
		"gen_model":              {"gemini-3.5-flash-lite"},
		"task_verify_provider":   {"gemini"},
		"task_verify_model":      {"gemini-decision-1"},
		"task_progress_provider": {"local"},
		"task_progress_model":    {"qwen3:4b"},
	})
	if okMixResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /ui/engines/routing mixed update, got %d (%s)", okMixResp.StatusCode, okMixBody)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskVerify); p != "gemini:gemini-decision-1" {
		t.Fatalf("expected TaskVerify gemini:gemini-decision-1, got %q", p)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskProgress); p != "local:qwen3:4b" {
		t.Fatalf("expected TaskProgress local:qwen3:4b, got %q", p)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskConsolidate); p != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("expected TaskConsolidate gemini:gemini-3.5-flash-lite, got %q", p)
	}
}

func TestTopEngineSetupWarningBannerAndOOBClearing(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("PRAXIS_PROVIDER", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LOCAL_MODEL_URL", "")

	deps := newTestDeps(t, ":memory:", nil, "", thttp.DashboardHooks{})

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	getRoot := func(lang string) string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if lang != "" {
			req.AddCookie(&http.Cookie{Name: "praxis_lang", Value: lang})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	enRoot := getRoot("en")
	if !strings.Contains(enRoot, `id="engineSetupAlert" class="setup-alert"`) || !strings.Contains(enRoot, `id="btnGoEngines"`) {
		t.Fatalf("expected visible #engineSetupAlert banner and #btnGoEngines on unconfigured dashboard:\n%s", enRoot)
	}
	koRoot := getRoot("ko")
	if !strings.Contains(koRoot, "추론 엔진 설정 필요") || !strings.Contains(koRoot, "추론 엔진 설정하기") {
		t.Fatalf("expected Korean setup warning text on dashboard:\n%s", koRoot)
	}

	code, noKeyHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-3.5-flash-lite"},
	})
	if code != http.StatusOK || !strings.Contains(noKeyHTML, `id="engineSetupAlert" class="setup-alert" role="alert" hx-swap-oob="outerHTML"`) || !strings.Contains(noKeyHTML, "GEMINI_API_KEY") {
		t.Fatalf("expected OOB GEMINI_API_KEY warning after saving gemini without key (code=%d):\n%s", code, noKeyHTML)
	}

	code, withKeyHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":       {"gemini"},
		"gemini_model":   {"gemini-3.5-flash-lite"},
		"gemini_api_key": {"AIzaTestDummyKeyForUnitTest1234"},
	})
	if code != http.StatusOK || !strings.Contains(withKeyHTML, `<div id="engineSetupAlert" hidden hx-swap-oob="outerHTML"></div>`) {
		t.Fatalf("expected OOB hidden #engineSetupAlert after saving valid gemini key (code=%d):\n%s", code, withKeyHTML)
	}
	clearedRoot := getRoot("en")
	if strings.Contains(clearedRoot, `class="setup-alert"`) || !strings.Contains(clearedRoot, `<div id="engineSetupAlert" hidden ></div>`) {
		t.Fatalf("expected hidden #engineSetupAlert after reload:\n%s", clearedRoot)
	}
}

func TestPortModalSaveResetCSRFAndDaemonHooks(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	modelsDir := filepath.Join(tmp, "models")

	var appliedDP, appliedGP int
	deps := newTestDeps(t, dbPath, noopEval{}, modelsDir, thttp.DashboardHooks{
		DaemonStatusFn: func() thttp.DaemonPortState {
			cfg := config.LoadPortConfig(dbPath)
			return thttp.DaemonPortState{
				PID:            4242,
				ListenAddr:     fmt.Sprintf("127.0.0.1:%d", cfg.DaemonPort),
				UptimeSec:      12,
				ConfiguredPort: cfg.DaemonPort,
				ActivePort:     cfg.DaemonPort,
			}
		},
		ApplyDaemonPortFn: func(_ context.Context, dp, gp int) (thttp.DaemonPortApplyResult, error) {
			appliedDP, appliedGP = dp, gp
			return thttp.DaemonPortApplyResult{
				ListenAddr:       fmt.Sprintf("127.0.0.1:%d", dp),
				ActiveDaemonPort: dp,
				Restarting:       true,
				NewURL:           fmt.Sprintf("http://127.0.0.1:%d", dp),
				ActiveGGUFPort:   gp,
				GGUFEndpoint:     fmt.Sprintf("http://127.0.0.1:%d", gp),
			}, nil
		},
	})
	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	rootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	if !strings.Contains(string(rootBytes), `id="daemonBadge"`) || !strings.Contains(string(rootBytes), `/ui/modal/ports`) {
		t.Fatalf("expected clickable #daemonBadge linked to /ui/modal/ports in root HTML")
	}

	jsResp, err := http.Get(ts.URL + "/assets/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, _ := io.ReadAll(jsResp.Body)
	_ = jsResp.Body.Close()
	if !strings.Contains(string(jsBytes), "praxisPortRedirect") {
		t.Fatalf("expected ui.js to handle praxisPortRedirect event")
	}

	code, modalHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/ports", nil)
	if code != http.StatusOK || !strings.Contains(modalHTML, `id="portsDialog"`) ||
		!strings.Contains(modalHTML, `name="daemon_port"`) || !strings.Contains(modalHTML, `name="gguf_port"`) {
		t.Fatalf("expected modal_ports HTML (code=%d):\n%s", code, modalHTML)
	}

	unauthResp, err := http.PostForm(ts.URL+"/ui/ports/save", url.Values{"daemon_port": {"19095"}, "gguf_port": {"19088"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-HX POST /ui/ports/save, got %d", unauthResp.StatusCode)
	}

	badCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/ports/save", url.Values{"daemon_port": {"19000"}, "gguf_port": {"19000"}})
	if badCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for equal ports, got %d", badCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/ports/save", strings.NewReader(url.Values{"daemon_port": {"19195"}, "gguf_port": {"19188"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	saveResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	saveBody, _ := io.ReadAll(saveResp.Body)
	_ = saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK || appliedDP != 19195 || appliedGP != 19188 {
		t.Fatalf("expected 200 OK & applied (19195,19188), got code=%d applied=(%d,%d) body=%s", saveResp.StatusCode, appliedDP, appliedGP, string(saveBody))
	}
	if cfg := config.LoadPortConfig(dbPath); cfg.DaemonPort != 19195 || cfg.GGUFPort != 19188 {
		t.Fatalf("expected saved ports.json (19195, 19188), got %+v", cfg)
	}
	var trig map[string]any
	if err := json.Unmarshal([]byte(saveResp.Header.Get("HX-Trigger")), &trig); err != nil {
		t.Fatalf("parse HX-Trigger %q: %v", saveResp.Header.Get("HX-Trigger"), err)
	}
	redir, _ := trig["praxisPortRedirect"].(map[string]any)
	if redir["url"] != "http://127.0.0.1:19195" {
		t.Fatalf("expected praxisPortRedirect url http://127.0.0.1:19195, got %+v", trig)
	}

	resetCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/ports/save", url.Values{"reset": {"1"}})
	if resetCode != http.StatusOK || appliedDP != domain.DefaultDaemonPort || appliedGP != domain.DefaultGGUFPort {
		t.Fatalf("expected reset to restore (18095, 18088), got code=%d applied=(%d,%d)", resetCode, appliedDP, appliedGP)
	}
}

type noneEval struct{ noopEval }

func (noneEval) Name() string { return "none" }

func TestLocalInferenceProbeAndSetupLanding(t *testing.T) {
	ctx := context.Background()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:4b"},{"id":"gemma3:12b"}]}`))
	}))
	defer ollamaSrv.Close()

	flideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","model":"OmniJev/OneJev1.6-0.8B"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer flideSrv.Close()

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>not an engine</body></html>`))
	}))
	defer htmlSrv.Close()

	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	closedURL := closedSrv.URL
	closedSrv.Close()

	got := discovery.ProbeLocalInferenceServers(ctx,
		discovery.LocalProbeTarget{Label: "Ollama", Provider: "local", BaseURL: ollamaSrv.URL + "/v1"},
		discovery.LocalProbeTarget{Label: "Flide / OmniJev", Provider: "jev", BaseURL: flideSrv.URL},
		discovery.LocalProbeTarget{Label: "WebUI", Provider: "local", BaseURL: htmlSrv.URL},
		discovery.LocalProbeTarget{Label: "Down", Provider: "local", BaseURL: closedURL},
	)
	if len(got) != 2 {
		t.Fatalf("expected 2 verified local inference servers, got %d (%+v)", len(got), got)
	}
	if got[0].Label != "Ollama" || got[0].Provider != "local" || len(got[0].Models) != 2 || got[0].Models[0] != "qwen3:4b" {
		t.Fatalf("unexpected Ollama probe result: %+v", got[0])
	}
	if got[1].Label != "Flide / OmniJev" || got[1].Provider != "jev" || len(got[1].Models) < 2 || got[1].Models[0] != "jev-latest" {
		t.Fatalf("unexpected Flide probe result: %+v", got[1])
	}

	for _, k := range []string{
		"PRAXIS_PROVIDER", "GEMINI_API_KEY", "TYPESAFE_API_KEY",
		"FLIDE_API_KEY", "JEV_API_URL", "LOCAL_MODEL_URL", "LOCAL_GGUF_URL",
	} {
		t.Setenv(k, "")
	}
	deps := newTestDeps(t, ":memory:", noneEval{}, "", thttp.DashboardHooks{})

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp1, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if !strings.Contains(string(body1), `data-setup-required="1"`) {
		t.Fatalf("expected data-setup-required=\"1\" on unconfigured landing page")
	}

	form := url.Values{
		"provider":       {"gemini"},
		"gemini_api_key": {"AIzaTestKey123456"},
	}
	saveReq, err := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/save", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq.Header.Set("HX-Request", "true")
	saveResp, err := http.DefaultClient.Do(saveReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save status = %d", saveResp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if strings.Contains(string(body2), `data-setup-required="1"`) || !strings.Contains(string(body2), `data-setup-required=""`) {
		t.Fatalf("expected data-setup-required=\"\" after configuring Gemini key")
	}
}

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
	deps.Memory.RememberRepoPath(ctx, "project:facetonly", devFacetRepo)
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

	const projScope = "project:teamui"
	deps.Memory.RememberRepoPath(ctx, projScope, devADir)

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

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

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

	const raceTitle = "Always run go test -race before merge"
	raceKey := domain.ComputeRuleKey("testing", raceTitle)
	code, promHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/team-promote?id="+url.QueryEscape(strconv.FormatInt(promoteID, 10))+"&team_scope="+url.QueryEscape(projScope), nil)
	if code != http.StatusOK || !strings.Contains(promHTML, `name="retire_local"`) || !strings.Contains(promHTML, raceTitle) {
		t.Fatalf("GET /ui/modal/team-promote failed (%d):\n%s", code, promHTML)
	}
	if strings.Contains(promHTML, `name="rule_key"`) || strings.Contains(promHTML, `name="added_by"`) || strings.Contains(promHTML, `name="pr_fallback"`) || strings.Contains(promHTML, "[invariant]") {
		t.Fatalf("team promote modal must not expose rule_key, manual added_by input, pr_fallback, or [invariant] tier:\n%s", promHTML)
	}

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
	if pubResp.StatusCode != http.StatusOK || !strings.Contains(pubHTML, raceTitle) || !strings.Contains(hxTrig, "praxisCloseModal") {
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

	runGitForUITest(t, devBDir, "pull", "origin", "main")
	rawRemote, err := os.ReadFile(filepath.Join(devBDir, domain.RulePackRelPath))
	if err != nil || !strings.Contains(string(rawRemote), raceTitle) || !strings.Contains(string(rawRemote), "@Alice-Lead") {
		t.Fatalf("expected origin/devB to have clean rule %q by @Alice-Lead, err=%v content=%s", raceTitle, err, string(rawRemote))
	}
	if strings.Contains(string(rawRemote), "[invariant]") || strings.Contains(string(rawRemote), "[rule]") || strings.Contains(string(rawRemote), raceKey) {
		t.Fatalf("remote TEAM_RULES.md must not expose rule_key or tier tags:\n%s", string(rawRemote))
	}

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

	code, confHTML := doHX(t, http.MethodPost, ts.URL+"/ui/team/conflict", url.Values{
		"team_scope": {projScope},
		"rule_key":   {dbKey},
		"decision":   {domain.RuleStateLocalOverride},
		"note":       {"Keep local staging override for integration test"},
	})
	if code != http.StatusOK || strings.Contains(confHTML, "1 conflict(s)") || !strings.Contains(confHTML, "local_override") {
		t.Fatalf("POST /ui/team/conflict failed (%d):\n%s", code, confHTML)
	}

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
	if updResp.StatusCode != http.StatusOK || !strings.Contains(updHTML, updatedRaceTitle) || !strings.Contains(updTrig, "praxisCloseModal") {
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
}
