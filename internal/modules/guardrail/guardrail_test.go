package guardrail_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"memorize/internal/domain"
)

func TestGuardrailEnforcesNonPinnedRulesAndBlocksMutatingGitCommands(t *testing.T) {
	ctx := context.Background()
	eval := &stubEval{}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))

	eval.dec = domain.MutationDecision{
		Category:          "convention",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Action:            "INSERT",
		Confidence:        0.95,
	}
	_, _ = st.mem.CommitMemory(ctx, "Always format structured audit logs in JSON", "convention", scope)

	eval.dec = domain.MutationDecision{
		Category:          "security",
		Tier:              "rule",
		IsPermanent:       0.92,
		IsGlobalInvariant: 0.20,
		Action:            "INSERT",
		Confidence:        0.93,
	}
	_, _ = st.mem.CommitMemory(ctx, "Never force-delete main branch or remove origin remote in git", "security", scope)

	eval.violations = []domain.PlanViolation{
		{
			Fact:        domain.Fact{ID: 2, Scope: scope, Category: "security", Content: "Never force-delete main branch or remove origin remote in git"},
			Probability: 0.95,
			Confidence:  0.94,
		},
	}

	for _, safeCmd := range []string{"git status", "git branch -a", "git remote -v"} {
		payload := fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"run_command","args":{"CommandLine":%q}}}`, repo, safeCmd)
		out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(payload), "")
		if code != 0 || out != `{"decision":"allow"}` {
			t.Fatalf("expected safe command %q to allow, got code=%d out=%s", safeCmd, code, out)
		}
	}

	for _, mutCmd := range []string{"git branch -D main", "git remote remove origin"} {
		payload := fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"run_command","args":{"CommandLine":%q}}}`, repo, mutCmd)
		out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(payload), "")
		if code != 0 || !strings.Contains(out, `"decision":"deny"`) || !strings.Contains(out, "Never force-delete main branch") {
			t.Fatalf("expected mutating command %q to be blocked by non-pinned rule #2, got code=%d out=%s", mutCmd, code, out)
		}
	}
}

func TestTurnPreMutateCodeGateAndSameTurnRuleEnforcement(t *testing.T) {
	ctx := context.Background()
	eval := &stubEval{
		dec: domain.MutationDecision{
			Category:          "architecture",
			Tier:              "invariant",
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.92,
			Action:            "INSERT",
			Confidence:        0.94,
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))

	turnPayload := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"lastUserInput":"앞으로 외부 API 호출 클라이언트는 반드시 3초 타임아웃을 설정한다"}`, repo)
	_, code, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(turnPayload), "")
	if err != nil || code != 0 {
		t.Fatalf("antigravity-turn failed: code=%d err=%v", code, err)
	}

	violatingEdit := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"http.DefaultClient.Get(externalAPI) // 타임아웃 없음"}}}`,
		repo, filepath.Join(repo, "client.go"))
	gateOut, gateCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(violatingEdit), "")
	if gateCode != 0 || !strings.Contains(gateOut, `"decision":"deny"`) || !strings.Contains(gateOut, "[MEMORIZE PRE-MUTATE GATE]") {
		t.Fatalf("expected Pre-Mutate Code Gate to deny edit before commit_memory, got code=%d out=%s", gateCode, gateOut)
	}

	if _, err := st.mem.CommitMemory(ctx, "외부 API 호출 클라이언트는 반드시 3초 타임아웃을 설정한다", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	eval.violations = []domain.PlanViolation{
		{
			Fact:        domain.Fact{ID: 1, Scope: scope, Category: "architecture", Content: "외부 API 호출 클라이언트는 반드시 3초 타임아웃을 설정한다"},
			Probability: 0.96,
			Confidence:  0.95,
		},
	}
	guardOut, guardCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(violatingEdit), "")
	if guardCode != 0 || !strings.Contains(guardOut, `"decision":"deny"`) || !strings.Contains(guardOut, "[MEMORIZE GUARDRAIL BLOCKED]") || !strings.Contains(guardOut, "3초 타임아웃") {
		t.Fatalf("expected same-turn Guardrail to block violating edit against newly committed rule #1, got code=%d out=%s", guardCode, guardOut)
	}

	eval.violations = nil
	okEdit := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"client := &http.Client{Timeout: 3 * time.Second}"}}}`,
		repo, filepath.Join(repo, "client.go"))
	okOut, okCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(okEdit), "")
	if okCode != 0 || okOut != `{"decision":"allow"}` {
		t.Fatalf("expected compliant edit to allow, got code=%d out=%s", okCode, okOut)
	}
}

func TestFirstChatOnboardingOfferSkipAndGuardrailFlow(t *testing.T) {
	ctx := context.Background()
	eval := &stubEval{
		dec: domain.MutationDecision{
			Category:          "convention",
			Tier:              "rule",
			Cardinality:       "exclusive",
			IsPermanent:       0.92,
			IsGlobalInvariant: 0.30,
			Action:            "INSERT",
			Confidence:        0.93,
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if got := st.gr.GetOnboardingStatus(ctx, scope); got != "pending" {
		t.Fatalf("expected initial status 'pending', got %q", got)
	}

	turn1 := fmt.Sprintf(`{"conversationId":"conv-onboard-1","workspacePaths":[%q],"lastUserInput":"이 프로젝트 구조 알려줘"}`, repo)
	out1, code1, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(turn1), "")
	if err != nil || code1 != 0 || !strings.Contains(out1, "[ONBOARDING AVAILABLE for "+scope+"]") {
		t.Fatalf("expected first-chat hook to offer onboarding for %s, got code=%d out=%s err=%v", scope, code1, out1, err)
	}

	out2, code2, _ := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(turn1), "")
	if code2 != 0 || out2 != "{}" {
		t.Fatalf("expected repeat hook in same session not to re-prompt onboarding, got code=%d out=%s", code2, out2)
	}

	mcpCallPayload := fmt.Sprintf(`{"conversationId":"conv-onboard-1","workspacePaths":[%q],"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"memorize_mcp","ToolName":"manage_memory"}}}`, repo)
	mcpOut, mcpCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(mcpCallPayload), "")
	if mcpCode != 0 || mcpOut != `{"decision":"allow"}` {
		t.Fatalf("expected call_mcp_tool(manage_memory) to allow cleanly, got code=%d out=%s", mcpCode, mcpOut)
	}

	skipRepo := t.TempDir()
	skipScope := "project:" + strings.ToLower(filepath.Base(skipRepo))
	if err := st.gr.SkipOnboarding(ctx, skipScope); err != nil {
		t.Fatalf("SkipOnboarding: %v", err)
	}
	skipPayload := fmt.Sprintf(`{"conversationId":"conv-skip-fresh","workspacePaths":[%q],"lastUserInput":"안녕"}`, skipRepo)
	skipOut, _, _ := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(skipPayload), "")
	if strings.Contains(skipOut, "ONBOARDING AVAILABLE") {
		t.Fatalf("expected skipped project never to offer onboarding, got: %s", skipOut)
	}
	if err := st.gr.ResetOnboarding(ctx, skipScope); err != nil || st.gr.GetOnboardingStatus(ctx, skipScope) != "pending" {
		t.Fatalf("expected ResetOnboarding to restore pending state, err=%v", err)
	}

	if _, err := st.mem.CommitMemory(ctx, "모든 서버 로그는 레거시 fmt.Println 대신 표준 log/slog JSON 로거만 사용한다", "convention", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	if st.gr.GetOnboardingStatus(ctx, scope) != "completed" {
		t.Fatalf("expected onboarding status 'completed' once a rule is committed, got %q", st.gr.GetOnboardingStatus(ctx, scope))
	}

	postPayload := fmt.Sprintf(`{"conversationId":"conv-post-onboard","workspacePaths":[%q],"lastUserInput":"워커 코드 정리하자"}`, repo)
	postOut, _, _ := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(postPayload), "")
	if strings.Contains(postOut, "ONBOARDING AVAILABLE") || !strings.Contains(postOut, "log/slog") {
		t.Fatalf("expected post-onboarding start hook to inject active rules without onboarding prompt, got: %s", postOut)
	}

	eval.violations = []domain.PlanViolation{
		{
			Fact:        domain.Fact{ID: 1, Scope: scope, Category: "convention", Content: "모든 서버 로그는 레거시 fmt.Println 대신 표준 log/slog JSON 로거만 사용한다"},
			Probability: 0.96,
			Confidence:  0.95,
		},
	}
	legacyEdit := fmt.Sprintf(`{"conversationId":"conv-post-onboard","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"fmt.Println(err)"}}}`,
		repo, filepath.Join(repo, "worker.go"))
	denyOut, denyCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(legacyEdit), "")
	if denyCode != 0 || !strings.Contains(denyOut, `"decision":"deny"`) || !strings.Contains(denyOut, "log/slog") {
		t.Fatalf("expected guardrail to block legacy fmt.Println edit, got code=%d out=%s", denyCode, denyOut)
	}
}

type planCapturingEval struct {
	stubEval
	capturedPlans []string
}

func (p *planCapturingEval) VerifyPlan(ctx context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	p.capturedPlans = append(p.capturedPlans, plan)
	return p.stubEval.VerifyPlan(ctx, plan, rules)
}

func TestPreToolDiffEditSnippetAndUTF8Safety(t *testing.T) {
	ctx := context.Background()
	eval := &planCapturingEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	_, _ = st.mem.CommitMemory(ctx, "AGENTS.md와 CLAUDE.md는 제외하고 CONSTRAINTS.md 및 핵심규칙만 유지한다", "architecture", scope)

	replacePayload := fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":"var manifests = []string{\n\t\"AGENTS.md\", \"CLAUDE.md\", \"CONSTRAINTS.md\", \"README.md\",\n\t\"go.mod\",\n}","Replacement":"var manifests = []string{\n\t\"CONSTRAINTS.md\", \"README.md\",\n\t\"go.mod\",\n}"}}}`,
		repo, filepath.Join(repo, "gitsync.go"))
	out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(replacePayload), "")
	if code != 0 || out != `{"decision":"allow"}` {
		t.Fatalf("expected clean replacement to allow, got code=%d out=%s", code, out)
	}
	if len(eval.capturedPlans) != 1 {
		t.Fatalf("expected VerifyPlan to be called once, got %d", len(eval.capturedPlans))
	}
	p1 := eval.capturedPlans[0]
	if !strings.Contains(p1, `- 	"AGENTS.md", "CLAUDE.md", "CONSTRAINTS.md", "README.md",`) ||
		!strings.Contains(p1, `+ 	"CONSTRAINTS.md", "README.md",`) ||
		strings.Contains(p1, "var manifests") || strings.Contains(p1, `"go.mod"`) {
		t.Fatalf("expected unified -/+ diff snippet without unchanged context lines, got:\n%s", p1)
	}

	delPayload := fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":"keep1\nAGENTS.md\nkeep2","Replacement":"keep1\nkeep2"}}}`,
		repo, filepath.Join(repo, "gitsync.go"))
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(delPayload), "")
	if len(eval.capturedPlans) != 2 || !strings.Contains(eval.capturedPlans[1], "Removed:\nAGENTS.md") {
		t.Fatalf("expected pure deletion to report Removed: block, got: %+v", eval.capturedPlans)
	}

	longOld := "AGENTS.md " + strings.Repeat("한글규칙문맥", 120)
	longNew := "CONSTRAINTS.md 핵심규칙 " + strings.Repeat("가나다라마바사", 40)
	utf8Payload := fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":%q,"Replacement":%q}}}`,
		repo, filepath.Join(repo, "gitsync.go"), "ctx\n"+longOld, "ctx\n"+longNew)
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(utf8Payload), "")
	if len(eval.capturedPlans) != 3 {
		t.Fatalf("expected 3rd VerifyPlan call, got %d", len(eval.capturedPlans))
	}
	p3 := eval.capturedPlans[2]
	if !utf8.ValidString(p3) || !strings.Contains(p3, "- AGENTS.md") || !strings.Contains(p3, "+ CONSTRAINTS.md") {
		t.Fatalf("expected valid UTF-8 and both -/+ sides preserved after truncation, valid=%v got:\n%s", utf8.ValidString(p3), p3)
	}
}

type spyReadyEval struct {
	scoreCalls int
	dec        domain.MutationDecision
}

func (s *spyReadyEval) Name() string { return "spy-ready" }
func (s *spyReadyEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	s.scoreCalls++
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}
func (s *spyReadyEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return s.dec, nil
}
func (s *spyReadyEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestFirstRunEngineSetupGateAndExplicitEnvProvider(t *testing.T) {
	for _, k := range []string{
		"MEMORY_PROVIDER", "GEMINI_API_KEY", "TYPESAFE_API_KEY",
		"FLIDE_API_KEY", "JEV_API_URL", "LOCAL_MODEL_URL", "LOCAL_GGUF_URL",
	} {
		t.Setenv(k, "")
	}
	if got := domain.ResolveExplicitEnvProvider(); got != "" {
		t.Fatalf("expected empty provider with no env vars set, got %q", got)
	}
	t.Setenv("GEMINI_API_KEY", "AIzaTestOnly")
	if got := domain.ResolveExplicitEnvProvider(); got != "gemini" {
		t.Fatalf("expected unambiguous GEMINI_API_KEY to select gemini, got %q", got)
	}
	t.Setenv("LOCAL_MODEL_URL", "http://127.0.0.1:11434/v1")
	if got := domain.ResolveExplicitEnvProvider(); got != "" {
		t.Fatalf("expected ambiguous multiple provider env vars without MEMORY_PROVIDER to return empty, got %q", got)
	}
	t.Setenv("MEMORY_PROVIDER", "local")
	if got := domain.ResolveExplicitEnvProvider(); got != "local" {
		t.Fatalf("expected explicit MEMORY_PROVIDER to win over multiple env vars, got %q", got)
	}

	ctx := context.Background()
	eval := &spyReadyEval{}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	eval.dec = domain.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.95,
		Action:            "INSERT",
		Confidence:        0.95,
	}
	if _, err := st.mem.CommitMemory(ctx, "Pinned: never bypass AuthMiddleware", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory pinned: %v", err)
	}
	eval.dec = domain.MutationDecision{
		Category:          "architecture",
		Tier:              "rule",
		IsPermanent:       0.90,
		IsGlobalInvariant: 0.20,
		Action:            "INSERT",
		Confidence:        0.95,
	}
	if _, err := st.mem.CommitMemory(ctx, "Auth tokens expire in 15m", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory rule: %v", err)
	}

	ready := false
	st.mem.Router().SetReadinessChecker(func(_ context.Context, _ map[string]string) (bool, string) {
		if !ready {
			return false, "no API key or local model"
		}
		return true, ""
	})

	out1, code1, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(`{"session_id":"sess-setup-gate"}`), repo)
	if err != nil || code1 != 0 {
		t.Fatalf("ExecuteHook start unready: code=%d err=%v", code1, err)
	}
	if !strings.Contains(out1, "[MEMORIZE ENGINE SETUP REQUIRED]") || !strings.Contains(out1, "/#engines") {
		t.Fatalf("expected setup notice with /#engines link, got:\n%s", out1)
	}
	if !strings.Contains(out1, "Pinned: never bypass AuthMiddleware") {
		t.Fatalf("expected pinned rule to still be surfaced offline, got:\n%s", out1)
	}
	if strings.Contains(out1, "[ONBOARDING AVAILABLE") {
		t.Fatalf("expected onboarding notice suppressed while engine is not ready, got:\n%s", out1)
	}
	if eval.scoreCalls != 0 {
		t.Fatalf("expected 0 ScoreRelevance calls while engine not ready, got %d", eval.scoreCalls)
	}

	out2, code2, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(`{"session_id":"sess-setup-gate","prompt":"fix auth handler"}`), repo)
	if err != nil || code2 != 0 {
		t.Fatalf("ExecuteHook turn unready: code=%d err=%v", code2, err)
	}
	if strings.Contains(out2, "[MEMORIZE ENGINE SETUP REQUIRED]") {
		t.Fatalf("expected per-session cooldown to suppress duplicate setup banner, got:\n%s", out2)
	}
	if eval.scoreCalls != 0 {
		t.Fatalf("expected 0 ScoreRelevance calls on turn while engine not ready, got %d", eval.scoreCalls)
	}

	coldRepo := t.TempDir()
	outColdUnready, code, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(`{"session_id":"sess-cold-gate"}`), coldRepo)
	if err != nil || code != 0 {
		t.Fatalf("ExecuteHook cold unready: code=%d err=%v", code, err)
	}
	if strings.Contains(outColdUnready, "[ONBOARDING AVAILABLE") {
		t.Fatalf("expected onboarding suppressed on cold repo when engine unready, got:\n%s", outColdUnready)
	}

	ready = true
	out3, code3, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(`{"session_id":"sess-setup-gate","prompt":"refactor auth middleware"}`), repo)
	if err != nil || code3 != 0 {
		t.Fatalf("ExecuteHook turn ready: code=%d err=%v", code3, err)
	}
	if eval.scoreCalls == 0 {
		t.Fatalf("expected ScoreRelevance to run once engine becomes ready, got out:\n%s", out3)
	}
	outColdReady, code, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(`{"session_id":"sess-cold-gate","prompt":"inspect codebase"}`), coldRepo)
	if err != nil || code != 0 {
		t.Fatalf("ExecuteHook cold ready: code=%d err=%v", code, err)
	}
	if !strings.Contains(outColdReady, "[ONBOARDING AVAILABLE for ") {
		t.Fatalf("expected onboarding notice to activate once engine becomes ready, got:\n%s", outColdReady)
	}
}
