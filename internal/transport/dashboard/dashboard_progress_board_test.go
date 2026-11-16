package dashboard_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func TestUnifiedProgressBoardAndLiveSSE(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	prog := deps.Progress

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	// 1. Cross-origin SSE must be rejected with 403
	evilReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	evilReq.Header.Set("Origin", "https://evil.example.com")
	evilResp, err := http.DefaultClient.Do(evilReq)
	if err != nil || evilResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-origin /api/events to return 403, got %v (%v)", evilResp, err)
	}
	_ = evilResp.Body.Close()

	// 2. Connect SSE stream and verify real-time push on Progress & Guardrail mutations
	sseCtx, sseCancel := context.WithTimeout(ctx, 4*time.Second)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/api/events", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events failed: %v (%v)", sseResp, err)
	}
	defer sseResp.Body.Close()

	reader := bufio.NewReader(sseResp.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(firstLine, ": connected") {
		t.Fatalf("expected initial ': connected' SSE comment, got %q (%v)", firstLine, err)
	}
	gotHello := false
	for i := 0; i < 5; i++ {
		hline, herr := reader.ReadString('\n')
		if herr != nil {
			t.Fatalf("read SSE hello line: %v", herr)
		}
		if strings.HasPrefix(hline, "event: hello") {
			hdata, _ := reader.ReadString('\n')
			if strings.Contains(hdata, `"boot":`) {
				gotHello = true
				break
			}
		}
	}
	if !gotHello {
		t.Fatalf("expected initial SSE 'event: hello' with boot ID")
	}

	// Trigger mutations across multiple projects, sessions, and work kinds
	ctxAlpha := domain.WithSessionID(ctx, "sess-alpha-11112222")
	if _, err := prog.RecordProgress(ctxAlpha, domain.ProgressInput{
		Scope:    "project:alpha",
		Track:    "인증 게이트웨이",
		Summary:  "ALPHA_WORK_SUMMARY JWT 토큰 회전 구현 완료",
		NextStep: "ALPHA_NEXT_STEP 리프레시 토큰 검증",
		Source:   "agent",
	}); err != nil {
		t.Fatalf("RecordProgress alpha: %v", err)
	}

	// Verify SSE pushed event: change + JSON seq payload
	gotChangeEvent := false
	for i := 0; i < 6; i++ {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			t.Fatalf("read SSE event line: %v", rerr)
		}
		if strings.HasPrefix(line, "event: change") {
			dataLine, _ := reader.ReadString('\n')
			if strings.HasPrefix(dataLine, "data: {\"seq\":") && strings.Contains(dataLine, `"domain":"progress"`) {
				gotChangeEvent = true
				break
			}
		}
	}
	if !gotChangeEvent {
		t.Fatalf("expected real-time SSE 'event: change' notification after RecordProgress")
	}
	sseCancel()

	ctxBeta := domain.WithSessionID(ctx, "sess-beta-33334444")
	if _, err := prog.RecordProgress(ctxBeta, domain.ProgressInput{
		Scope:    "project:beta",
		Track:    "결제 정산 배치",
		Summary:  "BETA_WORK_SUMMARY 정산 파이프라인 구축 완료",
		NextStep: "BETA_NEXT_STEP 실패 재시도 큐",
		Source:   "agent",
	}); err != nil {
		t.Fatalf("RecordProgress beta: %v", err)
	}
	if _, err := prog.RecordProgress(ctxBeta, domain.ProgressInput{
		Scope:     "project:beta",
		Track:     "architecture / settlement",
		Summary:   "BETA_MEM_SUMMARY 정산 멱등성 키 불변 규칙 반영",
		MemoryIDs: []int64{7},
		Source:    "memory",
	}); err != nil {
		t.Fatalf("RecordProgress beta memory: %v", err)
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:     "project:alpha",
		Track:     "architecture / auth",
		Summary:   "ALPHA_GIT_SUMMARY feat(auth): add JWKS cache",
		CommitSHA: "a1b2c3d4",
		Source:    "git_sync",
	}); err != nil {
		t.Fatalf("RecordProgress alpha git: %v", err)
	}

	fetchProg := func(query string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?"+query, nil)
		req.Header.Set("HX-Request", "true")
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ui/progress?%s failed: %v (%v)", query, resp, derr)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b)
	}

	// 3. Default All-Projects unified view shows BOTH projects and ALL 3 work kinds
	allHTML := fetchProg("lang=ko")
	for _, needle := range []string{
		"전체 프로젝트",
		"alpha",
		"beta",
		"구현 · 작업",
		"기억 · 규칙",
		"Git 커밋",
		"ALPHA_WORK_SUMMARY",
		"BETA_WORK_SUMMARY",
		"BETA_MEM_SUMMARY",
		"ALPHA_GIT_SUMMARY",
	} {
		if !strings.Contains(allHTML, needle) {
			t.Fatalf("expected All-Projects unified progress HTML to contain %q", needle)
		}
	}

	// 4. Filtering by scope updates BOTH top project board AND bottom history (zero alpha leak!)
	betaScopeHTML := fetchProg("lang=ko&scope=project:beta")
	if !strings.Contains(betaScopeHTML, "BETA_WORK_SUMMARY") || !strings.Contains(betaScopeHTML, "BETA_MEM_SUMMARY") {
		t.Fatalf("expected beta scope filter to include beta work & memory entries")
	}
	if strings.Contains(betaScopeHTML, "ALPHA_WORK_SUMMARY") || strings.Contains(betaScopeHTML, "ALPHA_NEXT_STEP") || strings.Contains(betaScopeHTML, "ALPHA_GIT_SUMMARY") {
		t.Fatalf("expected beta scope filter to exclude alpha from BOTH top board and bottom timeline")
	}

	// 5. Filtering by kind=git updates BOTH top 3-lane board AND bottom history
	gitOnlyHTML := fetchProg("lang=ko&kind=git")
	if !strings.Contains(gitOnlyHTML, "ALPHA_GIT_SUMMARY") {
		t.Fatalf("expected kind=git filter to include git commit milestone")
	}
	if strings.Contains(gitOnlyHTML, "ALPHA_WORK_SUMMARY") || strings.Contains(gitOnlyHTML, "BETA_WORK_SUMMARY") || strings.Contains(gitOnlyHTML, "BETA_MEM_SUMMARY") {
		t.Fatalf("expected kind=git filter to exclude work/memory items from top board and timeline")
	}

	// 6. Filtering by track updates BOTH top activity board AND bottom history
	trackOnlyHTML := fetchProg("lang=ko&track=" + url.QueryEscape("결제 정산 배치"))
	if !strings.Contains(trackOnlyHTML, "BETA_WORK_SUMMARY") || strings.Contains(trackOnlyHTML, "ALPHA_WORK_SUMMARY") {
		t.Fatalf("expected track filter to synchronize top activity board and timeline with selected track only")
	}
}

func TestDashboardProgressProjectCentricAndDoneWindow(t *testing.T) {
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	prog := deps.Progress

	ctx := context.Background()
	scope := "project:liveui"
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "UI Activity Refactor",
		Category: "backend",
		Summary:  "Refactored progress to project activity feed",
		NextStep: "Verify SSR rendering",
		Source:   "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "Window Selector",
		Category: "backend",
		Summary:  "Finished live window selector backend",
		Source:   "agent",
	}); err != nil {
		t.Fatal(err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Progress HTML renders project-centric activity cards (최근 활동 흐름) and done_window selector without liveSessionsStrip or status filter controls.
	resp, err := http.Get(ts.URL + "/ui/progress?scope=" + url.QueryEscape(scope) + "&done_window=24h&lang=ko")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	html := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /ui/progress, got %d", resp.StatusCode)
	}
	for _, want := range []string{
		`id="progDoneWindow"`,
		`id="projectProgressBoard"`,
		`프로젝트별 활동 요약`,
		`최근 활동 흐름`,
		`구현 · 작업`,
		`value="24h" selected`,
		`최근 24시간`,
		`최신 시각순`,
		`방금 전`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("expected progress HTML to contain %q, got:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		`지금 하는 중`, `이전 진행 흐름`,
		`liveSessionsStrip`, `progSessionFilter`,
		"/ui/progress/add", "/ui/progress/status", "/ui/progress/update", "/ui/progress/delete", "/ui/modal/progress-edit",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("Progress contract violated: found forbidden string %q", forbidden)
		}
	}

	// Verify English rendering and alternate done_window=30d selection.
	enResp, err := http.Get(ts.URL + "/ui/progress?scope=" + url.QueryEscape(scope) + "&done_window=30d&lang=en")
	if err != nil {
		t.Fatal(err)
	}
	enBytes, _ := io.ReadAll(enResp.Body)
	_ = enResp.Body.Close()
	enHTML := string(enBytes)
	for _, want := range []string{`value="30d" selected`, `Last 30d`, `by latest time`, `Project Activity`, `Recent Activity Flow`, `Task · Work`} {
		if !strings.Contains(enHTML, want) {
			t.Fatalf("expected English /ui/progress HTML to contain %q", want)
		}
	}

	// 2. Minimalist ui.js keeps domain SSE filtering without liveSessions polling or custom DOM morpher bloat.
	jsResp, err := http.Get(ts.URL + "/assets/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, _ := io.ReadAll(jsResp.Body)
	_ = jsResp.Body.Close()
	js := string(jsBytes)
	for _, want := range []string{
		"es.onerror",
		"handleConnectivityResume", "visibilitychange", "progDoneWindow", "domainToTabs", "defaultSettleDelay",
		"!document.hidden", "switchTab(next, true)",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("expected ui.js to contain %q", want)
		}
	}
	for _, removed := range []string{"morphTargetHTML", "startFallbackPoll", "tag === 'SELECT'", "refreshLiveSessions", "liveSessionsBadge"} {
		if strings.Contains(js, removed) {
			t.Fatalf("expected removed bloat %q not to appear in ui.js", removed)
		}
	}

	cssResp, err := http.Get(ts.URL + "/assets/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	cssBytes, _ := io.ReadAll(cssResp.Body)
	_ = cssResp.Body.Close()
	css := string(cssBytes)
	for _, want := range []string{"scrollbar-gutter: stable", ".prog-projects-grid", ".prog-project-card", ".prog-flow-item"} {
		if !strings.Contains(css, want) {
			t.Fatalf("expected styles.css to include %q", want)
		}
	}
	if strings.Contains(css, ".live-sessions-strip") {
		t.Fatalf("expected styles.css not to include removed .live-sessions-strip")
	}
}
