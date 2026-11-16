package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/inference/router"
	"memorize/internal/infra/config"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func TestGeminiDecisionsCatalogAndFeatureRoutingUI(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dash_routing.db")
	deps := newTestDeps(t, dbPath, noopEval{}, filepath.Join(t.TempDir(), "models"), thttp.DashboardHooks{})
	svc := deps.Memory

	taskRes := router.NewTaskResolver()
	svc.Router().SetTaskResolver(taskRes)
	svc.Router().SetDynamicResolver(func(cfg map[string]string) domain.Evaluator {
		ev, _ := taskRes(cfg, "")
		return ev
	})

	ts := httptest.NewServer(dashboard.NewHandler(deps))
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

	// 1. Gemini Decision tier shows gemini-decision-1 with Decision-Only badge
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

	// 2. Saving gemini-decision-1 as bulk default exposes Generative Companion selector + heuristic indicators
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

	// 3. Server rejects assigning non-generative models to text-generation tasks or gen_provider
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

	// 4. Valid mixed routing: gen_provider=gemini (3.8-flash) + task_taxonomy=local (qwen3:4b)
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

	// 5. Reset All to Bulk clears custom task routes back to Bulk Mode
	rstResp, rstHTML := postHX("/ui/engines/routing", url.Values{"mode": {"reset_all"}})
	if rstResp.StatusCode != http.StatusOK || !strings.Contains(rstHTML, "Bulk Mode (All Inherit)") {
		t.Fatalf("expected Reset All to Bulk to restore Bulk Mode, got status=%d\n%s", rstResp.StatusCode, rstHTML)
	}

	// 6. SSR /ui/engines/routing rejects non-generative provider on progress task and resolves mixed TaskProviders
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
	t.Setenv("MEMORY_PROVIDER", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LOCAL_MODEL_URL", "")

	deps := newTestDeps(t, ":memory:", nil, "", thttp.DashboardHooks{})
	deps.Memory.Router().SetTaskResolver(router.NewTaskResolver())

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	getRoot := func(lang string) string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if lang != "" {
			req.AddCookie(&http.Cookie{Name: "memorize_lang", Value: lang})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// 1. Unconfigured engine shows the top setup warning banner + Configure Engine button in both EN and KO
	enRoot := getRoot("en")
	if !strings.Contains(enRoot, `id="engineSetupAlert" class="setup-alert"`) || !strings.Contains(enRoot, `id="btnGoEngines"`) {
		t.Fatalf("expected visible #engineSetupAlert banner and #btnGoEngines on unconfigured dashboard:\n%s", enRoot)
	}
	koRoot := getRoot("ko")
	if !strings.Contains(koRoot, "추론 엔진 설정 필요") || !strings.Contains(koRoot, "추론 엔진 설정하기") {
		t.Fatalf("expected Korean setup warning text on dashboard:\n%s", koRoot)
	}

	// 2. Selecting Gemini without GEMINI_API_KEY updates banner via OOB to keep warning active
	code, noKeyHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-3.5-flash-lite"},
	})
	if code != http.StatusOK || !strings.Contains(noKeyHTML, `id="engineSetupAlert" class="setup-alert" role="alert" hx-swap-oob="outerHTML"`) || !strings.Contains(noKeyHTML, "GEMINI_API_KEY") {
		t.Fatalf("expected OOB GEMINI_API_KEY warning after saving gemini without key (code=%d):\n%s", code, noKeyHTML)
	}

	// 3. Providing GEMINI_API_KEY clears the banner live via OOB and keeps it hidden on reload
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
	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Root page includes clickable #daemonBadge -> /ui/modal/ports, and ui.js handles memorizePortRedirect.
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
	if !strings.Contains(string(jsBytes), "memorizePortRedirect") {
		t.Fatalf("expected ui.js to handle memorizePortRedirect event")
	}

	// 2. Render modal_ports with current port inputs.
	code, modalHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/ports", nil)
	if code != http.StatusOK || !strings.Contains(modalHTML, `id="portsDialog"`) ||
		!strings.Contains(modalHTML, `name="daemon_port"`) || !strings.Contains(modalHTML, `name="gguf_port"`) {
		t.Fatalf("expected modal_ports HTML (code=%d):\n%s", code, modalHTML)
	}

	// 3. Reject non-HTMX POST (CSRF) and invalid/equal ports.
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

	// 4. Valid HX save persists ports.json, invokes daemon hook, and emits memorizePortRedirect trigger.
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
	redir, _ := trig["memorizePortRedirect"].(map[string]any)
	if redir["url"] != "http://127.0.0.1:19195" {
		t.Fatalf("expected memorizePortRedirect url http://127.0.0.1:19195, got %+v", trig)
	}

	// 5. Reset action restores defaults (18095 / 18088).
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

	// Verify landing page emits data-setup-required="1" when unconfigured and clears it after saving Gemini key.
	for _, k := range []string{
		"MEMORY_PROVIDER", "GEMINI_API_KEY", "TYPESAFE_API_KEY",
		"FLIDE_API_KEY", "JEV_API_URL", "LOCAL_MODEL_URL", "LOCAL_GGUF_URL",
	} {
		t.Setenv(k, "")
	}
	deps := newTestDeps(t, ":memory:", noneEval{}, "", thttp.DashboardHooks{})

	h := dashboard.NewHandler(deps)
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
