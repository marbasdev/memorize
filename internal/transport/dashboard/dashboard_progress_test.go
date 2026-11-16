package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func TestFourTabProgressDistributionAndScopeStatsUI(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", plannerEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory
	prog := deps.Progress
	gr := deps.Guardrail

	// Seed 6 architecture rules in project:one (including 1 unclassified sub_category) + 2 rules in project:two
	for i := 1; i <= 6; i++ {
		sub := "ui"
		if i == 6 {
			sub = ""
		}
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("프로젝트 원 아키텍처 핵심 규칙 #%d", i), "architecture", sub, "", "project:one"); err != nil {
			t.Fatalf("Commit project:one %d: %v", i, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("프로젝트 투 별도 규칙 #%d", i), "database", "wal", "", "project:two"); err != nil {
			t.Fatalf("Commit project:two %d: %v", i, err)
		}
	}

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	// 1. 4-tab SSR layout includes Progress tab + fixed-height 2D squarified treemap + persistent collapse state
	rootResp, err := http.Get(ts.URL + "/?lang=ko")
	if err != nil || rootResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /?lang=ko failed: %v", err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	rootHTML := string(rootBytes)
	for _, needle := range []string{
		`id="tabBtn-progress"`,
		`id="panel-progress"`,
		`id="tabBtn-guardrails"`,
		`id="panel-guardrails"`,
		`가드레일`,
		`id="tabBtn-calibrate"`,
		`class="cat-breakdown" :open="distOpen"`,
		`@click.prevent="setDistOpen(!distOpen)"`,
		`class="treemap-mosaic treemap-2d"`,
		`style="left:0.00%;top:0.00%;width:100.00%;height:100.00%;"`,
		`class="treemap-tile uncat`,
		`(미분류)`,
	} {
		if !strings.Contains(rootHTML, needle) {
			t.Fatalf("expected 5-tab root HTML to contain %q", needle)
		}
	}

	// 2. Scope-linked KPIs & distribution via /ui/memories?scope=project:one (HX-Request OOB swap) and /api/state?scope=project:one
	memReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/memories?scope=project:one&state=active", nil)
	memReq.Header.Set("HX-Request", "true")
	memResp, err := http.DefaultClient.Do(memReq)
	if err != nil || memResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/memories?scope=project:one failed: %v", err)
	}
	memBytes, _ := io.ReadAll(memResp.Body)
	_ = memResp.Body.Close()
	memHTML := string(memBytes)
	if !strings.Contains(memHTML, `id="kpiStrip"`) || !strings.Contains(memHTML, `id="statActive">6<`) {
		t.Fatalf("expected OOB #statActive to reflect scope=project:one count (6), got:\n%s", memHTML)
	}
	if strings.Contains(memHTML, `id="confHist"`) {
		t.Fatalf("expected /ui/memories OOB response NOT to overwrite Calibration tab #confHist")
	}

	// 2b. Calibration tab confidence histogram has its own independent scope selector (#calHistScope)
	histOneResp, err := http.Get(ts.URL + "/ui/calibrate/histogram?cal_scope=project:one&lang=ko")
	if err != nil || histOneResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate/histogram?cal_scope=project:one failed: %v", err)
	}
	histOneBytes, _ := io.ReadAll(histOneResp.Body)
	_ = histOneResp.Body.Close()
	if !strings.Contains(string(histOneBytes), `<option value="project:one" selected>`) || !strings.Contains(string(histOneBytes), `>6</span>`) {
		t.Fatalf("expected independent cal_scope=project:one histogram with active count 6, got:\n%s", string(histOneBytes))
	}
	histTwoResp, err := http.Get(ts.URL + "/ui/calibrate/histogram?cal_scope=project:two&lang=ko")
	if err != nil || histTwoResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate/histogram?cal_scope=project:two failed: %v", err)
	}
	histTwoBytes, _ := io.ReadAll(histTwoResp.Body)
	_ = histTwoResp.Body.Close()
	if !strings.Contains(string(histTwoBytes), `<option value="project:two" selected>`) || !strings.Contains(string(histTwoBytes), `>2</span>`) {
		t.Fatalf("expected independent cal_scope=project:two histogram with active count 2, got:\n%s", string(histTwoBytes))
	}

	if code, body := doHX(t, http.MethodGet, ts.URL+"/ui/memories?scope=project:two&state=active", nil); code != http.StatusOK || !strings.Contains(body, `project:two`) {
		t.Fatalf("GET /ui/memories?scope=project:two failed: status=%d", code)
	}
	statsTwo, err := svc.GetDashboardStatsForScope(ctx, "project:two")
	if err != nil {
		t.Fatalf("GetDashboardStatsForScope(project:two): %v", err)
	}
	distTwo, err := svc.GetCategoryDistribution(ctx, "project:two")
	if err != nil {
		t.Fatalf("GetCategoryDistribution(project:two): %v", err)
	}
	if statsTwo.ActiveCount != 2 || distTwo.TotalActive != 2 {
		t.Fatalf("expected scope=project:two stats/distribution active=2, got stats=%+v dist=%+v", statsTwo, distTwo)
	}

	// 3. Category-scoped Consolidate modal from distribution [Merge...] button
	catMergeResp, err := http.Get(ts.URL + "/ui/modal/consolidate?merge_scope=project:one&merge_category=architecture")
	if err != nil || catMergeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET category-scoped consolidate modal failed: %v", err)
	}
	catMergeBytes, _ := io.ReadAll(catMergeResp.Body)
	_ = catMergeResp.Body.Close()
	if !strings.Contains(string(catMergeBytes), `name="merge_category" value="architecture"`) {
		t.Fatalf("expected hidden merge_category=architecture in consolidate modal")
	}

	// 3b. Unified AI Recommend (suggest -> apply-plan) shows both merge & re-categorization in Korean
	uniSugReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/suggest?lang=ko", strings.NewReader("merge_scope=project:one"))
	uniSugReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uniSugReq.Header.Set("HX-Request", "true")
	uniSugResp, err := http.DefaultClient.Do(uniSugReq)
	if err != nil || uniSugResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/suggest?lang=ko failed: %v (%v)", uniSugResp, err)
	}
	uniSugBytes, _ := io.ReadAll(uniSugResp.Body)
	_ = uniSugResp.Body.Close()
	for _, needle := range []string{`AI 병합 추천 분포`, `AI 재분류 추천`, `value="tilemap"`, `id="btnConfirmAIMerge"`} {
		if !strings.Contains(string(uniSugBytes), needle) {
			t.Fatalf("expected unified Korean AI plan to contain %q, got:\n%s", needle, string(uniSugBytes))
		}
	}

	rebAppForm := url.Values{
		"merge_scope": {"project:one"},
		"item_idx":    {"0"},
		"item_0_id":   {"6"},
		"item_0_cat":  {"architecture"},
		"item_0_sub":  {"tilemap"},
	}
	rebAppReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/apply-plan?lang=ko", strings.NewReader(rebAppForm.Encode()))
	rebAppReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rebAppReq.Header.Set("HX-Request", "true")
	rebAppResp, err := http.DefaultClient.Do(rebAppReq)
	if err != nil || rebAppResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/apply-plan (rebalance-only) failed: %v (%v)", rebAppResp, err)
	}
	_ = rebAppResp.Body.Close()
	rebCheck, _ := svc.GetRecordsByIDs(ctx, []int64{6})
	if len(rebCheck) != 1 || rebCheck[0].SubCategory != "tilemap" || !strings.Contains(rebCheck[0].Content, "핵심 규칙 #6") {
		t.Fatalf("expected #6 sub_category=tilemap with untouched content, got %+v", rebCheck)
	}

	// 4. Progress HTMX routes: Read-Only 3-lane board + pagination + Sync button (no manual add/edit/status/delete routes)
	csrfReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/sync", strings.NewReader("scope=project:one"))
	csrfReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfResp, err := http.DefaultClient.Do(csrfReq)
	if err != nil || csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected POST /ui/progress/sync without HX-Request to fail with 403, got %v (%v)", csrfResp, err)
	}
	_ = csrfResp.Body.Close()

	// Seed agent-driven activity entries (12 earlier milestones + 2 latest feature tracks) to verify read-only activity cards & pagination
	for i := 1; i <= 12; i++ {
		details := ""
		if i == 12 {
			details = "최근 완료 세부 내역 검증"
		}
		if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
			Scope:   "project:one",
			Track:   fmt.Sprintf("완료 작업 #%d", i),
			Summary: fmt.Sprintf("완료된 마일스톤 항목 %d", i),
			Details: details,
			Source:  "agent",
		}); err != nil {
			t.Fatalf("RecordProgress done %d: %v", i, err)
		}
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   "project:one",
		Track:   "세션 자동 연동",
		Summary: "SessionStart Git 자동 동기화 점검",
		Source:  "agent",
	}); err != nil {
		t.Fatalf("RecordProgress work 2: %v", err)
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:     "project:one",
		Track:     "4탭 진행현황 타임라인 개편",
		Summary:   "4탭 진행상황 UI 구현",
		Details:   "상단 활동 요약 카드\n날짜별 세로 히스토리 페이지네이션",
		NextStep:  "E2E 검증",
		MemoryIDs: []int64{1, 2},
		Source:    "agent",
	}); err != nil {
		t.Fatalf("RecordProgress work 1: %v", err)
	}

	progReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&lang=ko&page=1&page_size=10", nil)
	progReq.Header.Set("HX-Request", "true")
	progResp, err := http.DefaultClient.Do(progReq)
	if err != nil || progResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress page 1 failed: %v", err)
	}
	progBytes, _ := io.ReadAll(progResp.Body)
	_ = progResp.Body.Close()
	progHTML := string(progBytes)
	for _, needle := range []string{
		`id="projectProgressBoard"`,
		`prog-project-card`,
		`프로젝트별 활동 요약`,
		`최근 활동 흐름`,
		`구현 · 작업`,
		`id="btnSyncProgress"`,
		`id="progressTimeline"`,
		`id="progPaginationBar"`,
		`4탭 진행현황 타임라인 개편`,
		`4탭 진행상황 UI 구현`,
		`E2E 검증`,
		`class="prog-detail-fold"`,
	} {
		if !strings.Contains(progHTML, needle) {
			t.Fatalf("expected read-only progress page 1 HTML to contain %q, got:\n%s", needle, progHTML)
		}
	}
	for _, forbidden := range []string{
		`지금 하는 중`,
		`이전 진행 흐름`,
		`/ui/progress/add`,
		`/ui/progress/status`,
		`/ui/progress/update`,
		`/ui/progress/delete`,
		`/ui/modal/progress-edit`,
	} {
		if strings.Contains(progHTML, forbidden) {
			t.Fatalf("expected read-only progress HTML to omit legacy/manual item %q", forbidden)
		}
	}

	// Verify page 2 of history pagination
	page2Req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&lang=ko&page=2&page_size=10", nil)
	page2Req.Header.Set("HX-Request", "true")
	page2Resp, err := http.DefaultClient.Do(page2Req)
	if err != nil || page2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress page 2 failed: %v", err)
	}
	page2Bytes, _ := io.ReadAll(page2Resp.Body)
	_ = page2Resp.Body.Close()
	if !strings.Contains(string(page2Bytes), "완료된 마일스톤 항목 1") || !strings.Contains(string(page2Bytes), `id="projectProgressBoard"`) {
		t.Fatalf("expected page 2 to render older history items while preserving top project board")
	}

	// Verify track filter on /ui/progress isolates the requested track
	trkReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&track="+url.QueryEscape("4탭 진행현황 타임라인 개편"), nil)
	trkReq.Header.Set("HX-Request", "true")
	trkResp, err := http.DefaultClient.Do(trkReq)
	if err != nil || trkResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress?track=... failed: %v", err)
	}
	trkBytes, _ := io.ReadAll(trkResp.Body)
	_ = trkResp.Body.Close()
	if !strings.Contains(string(trkBytes), "4탭 진행상황 UI 구현") {
		t.Fatalf("expected track-filtered progress HTML to include matching track item")
	}

	progOv, err := prog.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: "project:one", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("GetProgressOverviewWithQuery failed: %v", err)
	}
	if progOv.TotalCount < 14 || len(progOv.Records) != 10 || progOv.TotalPages < 2 || len(progOv.RecentHistory) != 5 {
		t.Fatalf("expected paginated progress overview with RecentHistory=5, got %+v", progOv)
	}

	// Verify Korean toast in HX-Trigger header uses pure 7-bit ASCII \uXXXX escapes (preventing browser Latin-1 mojibake)
	syncReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/sync?lang=ko", strings.NewReader("scope=project:one"))
	syncReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	syncReq.Header.Set("HX-Request", "true")
	syncResp, err := http.DefaultClient.Do(syncReq)
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/sync?lang=ko failed: %v (%v)", syncResp, err)
	}
	_ = syncResp.Body.Close()
	rawTrig := syncResp.Header.Get("HX-Trigger")
	for i := 0; i < len(rawTrig); i++ {
		if rawTrig[i] > 0x7F {
			t.Fatalf("expected HX-Trigger HTTP header to be pure 7-bit ASCII (\\uXXXX escaped), found byte 0x%X in %q", rawTrig[i], rawTrig)
		}
	}
	var trigMap map[string]any
	if err := json.Unmarshal([]byte(rawTrig), &trigMap); err != nil {
		t.Fatalf("unmarshal HX-Trigger %q: %v", rawTrig, err)
	}
	toastStr, _ := trigMap["memorizeToast"].(string)
	if !strings.HasPrefix(toastStr, "동기화 완료") {
		t.Fatalf("expected decoded Korean toast starting with '동기화 완료', got %q", toastStr)
	}

	// 5. Verify Guardrail & Plan Verification Logs UI + OOB simulator update + server-side pagination
	for i := 1; i <= 11; i++ {
		_, _ = gr.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       "project:one",
			Source:      "antigravity-pretool",
			ToolName:    "run_command",
			Verdict:     domain.GuardrailVerdictRuleViolation,
			Blocked:     true,
			RuleCount:   3,
			ViolatedIDs: []int64{1},
			PlanSnippet: fmt.Sprintf("위반 명령 테스트 #%d", i),
			Details:     "[#1 project:one/architecture · 96%] 핵심 규칙 위반 차단",
		})
	}
	verReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/verify?lang=ko", strings.NewReader("scope=project:one&plan="+url.QueryEscape("정상 아키텍처 준수 계획 점검")))
	verReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verReq.Header.Set("HX-Request", "true")
	verResp, err := http.DefaultClient.Do(verReq)
	if err != nil || verResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/verify failed: %v (%v)", verResp, err)
	}
	verBytes, _ := io.ReadAll(verResp.Body)
	_ = verResp.Body.Close()
	if !strings.Contains(string(verBytes), `id="guardrailLogsSection"`) || !strings.Contains(string(verBytes), `hx-swap-oob="true"`) {
		t.Fatalf("expected POST /ui/verify to include OOB #guardrailLogsSection refresh")
	}

	// Guardrails tab (/ui/guardrails) and Calibration tab (/ui/calibrate) must render their separated responsibilities cleanly
	grTabResp, err := http.Get(ts.URL + "/ui/guardrails?lang=ko")
	if err != nil || grTabResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrails failed: %v", err)
	}
	grTabBytes, _ := io.ReadAll(grTabResp.Body)
	_ = grTabResp.Body.Close()
	grTabHTML := string(grTabBytes)
	verIdx := strings.Index(grTabHTML, `id="verifySimulatorCard"`)
	logIdx := strings.Index(grTabHTML, `id="guardrailLogsSection"`)
	if verIdx < 0 || logIdx < 0 || verIdx > logIdx || strings.Contains(grTabHTML, `id="thresholdsCard"`) {
		t.Fatalf("expected /ui/guardrails to place verifySimulatorCard ABOVE guardrailLogsSection and exclude thresholdsCard:\n%s", grTabHTML)
	}
	calTabResp, err := http.Get(ts.URL + "/ui/calibrate?lang=ko")
	if err != nil || calTabResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate failed: %v", err)
	}
	calTabBytes, _ := io.ReadAll(calTabResp.Body)
	_ = calTabResp.Body.Close()
	calTabHTML := string(calTabBytes)
	if !strings.Contains(calTabHTML, `id="thresholdsCard"`) || !strings.Contains(calTabHTML, `id="probeResults"`) || strings.Contains(calTabHTML, `id="guardrailLogsSection"`) || strings.Contains(calTabHTML, `id="verifySimulatorCard"`) {
		t.Fatalf("expected /ui/calibrate to contain thresholds + relevance probe ONLY (without guardrailLogsSection or verifySimulatorCard):\n%s", calTabHTML)
	}

	grPage1Resp, err := http.Get(ts.URL + "/ui/guardrail-logs?gr_scope=project:one&gr_page=1&gr_page_size=10&lang=ko")
	if err != nil || grPage1Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrail-logs page 1 failed: %v", err)
	}
	grPage1Bytes, _ := io.ReadAll(grPage1Resp.Body)
	_ = grPage1Resp.Body.Close()
	for _, needle := range []string{`id="guardrailLogsSection"`, `규칙 위반 차단`, `위반 명령 테스트 #11`, `id="guardrailPaginationBar"`} {
		if !strings.Contains(string(grPage1Bytes), needle) {
			t.Fatalf("expected guardrail logs page 1 HTML to contain %q, got:\n%s", needle, string(grPage1Bytes))
		}
	}

	grPage2Resp, err := http.Get(ts.URL + "/ui/guardrail-logs?gr_scope=project:one&gr_page=2&gr_page_size=10&lang=ko")
	if err != nil || grPage2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrail-logs page 2 failed: %v", err)
	}
	grPage2Bytes, _ := io.ReadAll(grPage2Resp.Body)
	_ = grPage2Resp.Body.Close()
	if !strings.Contains(string(grPage2Bytes), `위반 명령 테스트 #1`) {
		t.Fatalf("expected guardrail logs page 2 to contain older entry '위반 명령 테스트 #1'")
	}

	grPage, err := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("QueryGuardrailLogsPage failed: %v", err)
	}
	if grPage.TotalCount != 12 || grPage.BlockedCount != 11 || grPage.PassCount != 1 || grPage.TotalPages != 2 || len(grPage.Records) != 10 {
		t.Fatalf("unexpected guardrail logs pagination payload: %+v", grPage)
	}

	// 6. Verify default Forever (0d) retention, header-exposed Auto-TTL select, and explicit 2-step Confirm Cleanup controls
	defRetResp, err := http.Get(ts.URL + "/api/retention")
	if err != nil || defRetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET default /api/retention failed: %v", err)
	}
	var defRetPol domain.RetentionPolicy
	if err := json.NewDecoder(defRetResp.Body).Decode(&defRetPol); err != nil {
		t.Fatalf("decode default /api/retention: %v", err)
	}
	_ = defRetResp.Body.Close()
	if defRetPol.ProgressDays != 0 || defRetPol.GuardrailDays != 0 {
		t.Fatalf("expected default ProgressDays=0 GuardrailDays=0 (Forever), got %+v", defRetPol)
	}

	// Single-line exposed header controls (Auto-TTL select + Immediate Cleanup period select + explicit Confirm button, no collapsible drawer):
	for _, needle := range []string{
		`id="progRetentionDays"`,
		`<option value="0" selected>영구 보관</option>`,
		`id="progCleanDaysSelect"`,
		`id="progCleanupControls"`,
		`id="btnCleanupProgressAsk"`,
		`id="btnCleanupProgress"`,
	} {
		if !strings.Contains(progHTML, needle) {
			t.Fatalf("expected /ui/progress HTML to contain %q", needle)
		}
	}
	for _, needle := range []string{
		`id="guardrailRetentionDays"`,
		`<option value="0" selected>영구 보관</option>`,
		`value="stop"`,
		`Stop 훅 (일괄 검증)`,
		`id="guardrailCleanDaysSelect"`,
		`id="guardrailCleanupIncBlocked"`,
		`id="guardrailCleanupControls"`,
		`id="btnCleanupGuardrailAsk"`,
		`id="btnCleanupGuardrail"`,
	} {
		if !strings.Contains(string(grPage1Bytes), needle) {
			t.Fatalf("expected /ui/guardrail-logs HTML to contain %q", needle)
		}
	}

	// Set retention via SSR endpoint
	retReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/retention?lang=ko", strings.NewReader("scope=project:one&retention_days=30"))
	retReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	retReq.Header.Set("HX-Request", "true")
	retResp, err := http.DefaultClient.Do(retReq)
	if err != nil || retResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/retention failed: %v (%v)", retResp, err)
	}
	_ = retResp.Body.Close()

	grRetReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/retention?lang=ko", strings.NewReader("gr_scope=project:one&retention_days=7"))
	grRetReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grRetReq.Header.Set("HX-Request", "true")
	grRetResp, err := http.DefaultClient.Do(grRetReq)
	if err != nil || grRetResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/retention failed: %v (%v)", grRetResp, err)
	}
	_ = grRetResp.Body.Close()

	apiRetResp, err := http.Get(ts.URL + "/api/retention")
	if err != nil || apiRetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/retention failed: %v", err)
	}
	var retPol domain.RetentionPolicy
	if err := json.NewDecoder(apiRetResp.Body).Decode(&retPol); err != nil {
		t.Fatalf("decode /api/retention: %v", err)
	}
	_ = apiRetResp.Body.Close()
	if retPol.ProgressDays != 30 || retPol.GuardrailDays != 7 {
		t.Fatalf("expected ProgressDays=30 GuardrailDays=7, got %+v", retPol)
	}

	// Clean guardrail PASS logs only (older_than_days=0 -> removes the 1 PASS log while keeping all 11 blocked violations)
	grCleanPassReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/cleanup?lang=ko", strings.NewReader("gr_scope=project:one&older_than_days=0"))
	grCleanPassReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grCleanPassReq.Header.Set("HX-Request", "true")
	grCleanPassResp, err := http.DefaultClient.Do(grCleanPassReq)
	if err != nil || grCleanPassResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/cleanup (PASS only) failed: %v (%v)", grCleanPassResp, err)
	}
	_ = grCleanPassResp.Body.Close()

	grAfterPassPage, _ := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", State: "all", Page: 1, PageSize: 50})
	if grAfterPassPage.TotalCount != 11 || grAfterPassPage.BlockedCount != 11 || grAfterPassPage.PassCount != 0 {
		t.Fatalf("expected PASS-only cleanup to preserve all 11 blocked logs, got %+v", grAfterPassPage)
	}

	// Clean guardrail logs INCLUDING blocked violations (include_blocked=1, older_than_days=0)
	grCleanAllReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/cleanup?lang=ko", strings.NewReader("gr_scope=project:one&older_than_days=0&include_blocked=1"))
	grCleanAllReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grCleanAllReq.Header.Set("HX-Request", "true")
	grCleanAllResp, err := http.DefaultClient.Do(grCleanAllReq)
	if err != nil || grCleanAllResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/cleanup (include_blocked=1) failed: %v (%v)", grCleanAllResp, err)
	}
	_ = grCleanAllResp.Body.Close()

	grAfterAllPage, _ := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", State: "all", Page: 1, PageSize: 50})
	if grAfterAllPage.TotalCount != 0 {
		t.Fatalf("expected 0 guardrail logs after include_blocked=1 cleanup, got %+v", grAfterAllPage)
	}

	// Clean all progress history (older_than_days=0): removes all recorded entries in scope while leaving CSRF protection intact!
	progCleanReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/cleanup?lang=ko", strings.NewReader("scope=project:one&older_than_days=0"))
	progCleanReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	progCleanReq.Header.Set("HX-Request", "true")
	progCleanResp, err := http.DefaultClient.Do(progCleanReq)
	if err != nil || progCleanResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/cleanup failed: %v (%v)", progCleanResp, err)
	}
	progCleanHTML, _ := io.ReadAll(progCleanResp.Body)
	_ = progCleanResp.Body.Close()
	if strings.Contains(string(progCleanHTML), "4탭 진행상황 UI 구현") || strings.Contains(string(progCleanHTML), "완료된 마일스톤 항목 1") {
		t.Fatalf("expected older_than_days=0 cleanup to remove all project activity entries")
	}
	ovAfterClean, err := prog.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: "project:one", Page: 1, PageSize: 10})
	if err != nil || ovAfterClean.TotalCount != 0 {
		t.Fatalf("expected 0 remaining progress rows after cleanup, got ov=%+v err=%v", ovAfterClean, err)
	}
}
