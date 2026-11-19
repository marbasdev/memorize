package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	thttp "memorize/internal/transport/http"
)

func newTestDeps(t *testing.T, dbPath string, eval domain.Evaluator, modelsDir string, hooks thttp.DashboardHooks) Deps {
	t.Helper()
	if dbPath == "" {
		dbPath = ":memory:"
	}
	if modelsDir == "" {
		modelsDir = t.TempDir()
	}
	mem, err := memory.Open(dbPath, eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	mgr := localmodel.NewManager(modelsDir)
	mem.Router().AttachLocalManager(mgr)
	return Deps{
		Memory:     mem,
		Progress:   prog,
		Guardrail:  gr,
		LocalModel: mgr,
		Hooks:      hooks,
	}
}

type noopEval struct{}

func (noopEval) Name() string { return "noop" }
func (noopEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}
func (noopEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Action:            "INSERT",
		Confidence:        0.95,
	}, nil
}
func (noopEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}
func (noopEval) SynthesizeConsolidation(_ context.Context, _ string, _ string, facts []domain.Fact) (string, error) {
	if len(facts) > 0 {
		return facts[0].Content, nil
	}
	return "", nil
}

type noopMutEval struct{ noopEval }

func (noopMutEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{Action: "INSERT", Category: "convention", IsPermanent: 0.9, Confidence: 0.9}, nil
}

func doHX(t *testing.T, method, targetURL string, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, targetURL, body)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, targetURL, err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, targetURL, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func loginSessionCookie(t *testing.T, h http.Handler, host, token string) *http.Cookie {
	t.Helper()
	form := url.Values{"token": {token}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(form))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login expected 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == domain.SessionCookieName {
			return c
		}
	}
	t.Fatalf("missing session cookie on /login response")
	return nil
}
