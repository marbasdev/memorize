package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"praxis/internal/domain"
	"praxis/internal/inference/localmodel"
	"praxis/internal/modules/rulepack"
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

	clientFile := filepath.Join(repo, "client.go")
	if err := os.WriteFile(clientFile, []byte("package client\n// base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	violatingEdit := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"http.DefaultClient.Get(externalAPI) // 타임아웃 없음"}}}`,
		repo, clientFile)
	gateOut, gateCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(violatingEdit), "")
	if gateCode != 0 || !strings.Contains(gateOut, `"decision":"deny"`) || !strings.Contains(gateOut, "[PRAXIS PRE-MUTATE GATE]") {
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
	badCmd := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"toolCall":{"name":"run_command","args":{"CommandLine":"go run ./cmd/fetch --no-timeout"}}}`, repo)
	cmdDenyOut, cmdDenyCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(badCmd), "")
	if cmdDenyCode != 0 || !strings.Contains(cmdDenyOut, `"decision":"deny"`) || !strings.Contains(cmdDenyOut, "[PRAXIS GUARDRAIL BLOCKED]") || !strings.Contains(cmdDenyOut, "3초 타임아웃") || !strings.Contains(cmdDenyOut, "never self-revoke without consent") {
		t.Fatalf("expected same-turn Guardrail to block violating run_command immediately with consent guidance, got code=%d out=%s", cmdDenyCode, cmdDenyOut)
	}

	editAllowOut, editAllowCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(violatingEdit), "")
	if editAllowCode != 0 || editAllowOut != `{"decision":"allow"}` {
		t.Fatalf("expected edit_file to allow after Pre-Mutate Gate and defer check to Stop, got code=%d out=%s", editAllowCode, editAllowOut)
	}
	if err := os.WriteFile(clientFile, []byte("package client\nhttp.DefaultClient.Get(externalAPI) // 타임아웃 없음\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopPayload := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL","continuationActive":false}`, repo)
	stopFixOut, stopFixCode, _ := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(stopPayload), "")
	if stopFixCode != 0 || !strings.Contains(stopFixOut, `"decision":"continue"`) || !strings.Contains(stopFixOut, "[PRAXIS GUARDRAIL — FIX BEFORE FINISHING]") || !strings.Contains(stopFixOut, "3초 타임아웃") || !strings.Contains(stopFixOut, "ask the user to revise that #ID") {
		t.Fatalf("expected Stop hook to force continuation fix on violating edit against newly committed rule #1, got code=%d out=%s", stopFixCode, stopFixOut)
	}

	eval.violations = nil
	okEdit := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"client := &http.Client{Timeout: 3 * time.Second}"}}}`,
		repo, clientFile)
	okOut, okCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(okEdit), "")
	if okCode != 0 || okOut != `{"decision":"allow"}` {
		t.Fatalf("expected compliant edit to allow, got code=%d out=%s", okCode, okOut)
	}
	if err := os.WriteFile(clientFile, []byte("package client\nvar c = &http.Client{Timeout: 3 * time.Second}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopDonePayload := fmt.Sprintf(`{"conversationId":"conv-gate-1","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL","continuationActive":true}`, repo)
	stopDoneOut, stopDoneCode, _ := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(stopDonePayload), "")
	if stopDoneCode != 0 || stopDoneOut != `{}` {
		t.Fatalf("expected Stop with continuationActive=true to finish cleanly without looping, got code=%d out=%s", stopDoneCode, stopDoneOut)
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

	mcpCallPayload := fmt.Sprintf(`{"conversationId":"conv-onboard-1","workspacePaths":[%q],"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"praxis_mcp","ToolName":"manage_memory"}}}`, repo)
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
	workerFile := filepath.Join(repo, "worker.go")
	_ = os.WriteFile(workerFile, []byte("package worker\n"), 0o600)
	legacyEdit := fmt.Sprintf(`{"conversationId":"conv-post-onboard","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"fmt.Println(err)"}}}`,
		repo, workerFile)
	editAllowOut, editAllowCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(legacyEdit), "")
	if editAllowCode != 0 || editAllowOut != `{"decision":"allow"}` {
		t.Fatalf("expected edit_file to allow at PreToolUse and batch at Stop, got code=%d out=%s", editAllowCode, editAllowOut)
	}
	_ = os.WriteFile(workerFile, []byte("package worker\nfunc f(err error) { fmt.Println(err) }\n"), 0o600)
	stopDenyOut, stopDenyCode, _ := st.gr.ExecuteHook(ctx, "antigravity-stop",
		[]byte(fmt.Sprintf(`{"conversationId":"conv-post-onboard","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL"}`, repo)), "")
	if stopDenyCode != 0 || !strings.Contains(stopDenyOut, `"decision":"continue"`) || !strings.Contains(stopDenyOut, "log/slog") {
		t.Fatalf("expected Stop hook to block and request fix for legacy fmt.Println edit, got code=%d out=%s", stopDenyCode, stopDenyOut)
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

	targetFile := filepath.Join(repo, "gitsync.go")
	orig1 := "var manifests = []string{\n\t\"AGENTS.md\", \"CLAUDE.md\", \"CONSTRAINTS.md\", \"README.md\",\n\t\"go.mod\",\n}"
	new1 := "var manifests = []string{\n\t\"CONSTRAINTS.md\", \"README.md\",\n\t\"go.mod\",\n}"
	_ = os.WriteFile(targetFile, []byte(orig1), 0o600)
	replacePayload := fmt.Sprintf(`{"conversationId":"conv-diff-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":%q,"Replacement":%q}}}`,
		repo, targetFile, orig1, new1)
	out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(replacePayload), "")
	if code != 0 || out != `{"decision":"allow"}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected edit_file PreToolUse to allow with 0 VerifyPlan calls, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}
	_ = os.WriteFile(targetFile, []byte(new1), 0o600)
	stopPayload := fmt.Sprintf(`{"conversationId":"conv-diff-1","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL"}`, repo)
	if sOut, sCode, _ := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(stopPayload), ""); sCode != 0 || sOut != `{}` {
		t.Fatalf("expected clean Stop batch verify to pass, got code=%d out=%s", sCode, sOut)
	}
	if len(eval.capturedPlans) != 1 {
		t.Fatalf("expected VerifyPlan to be called once at Stop, got %d", len(eval.capturedPlans))
	}
	p1 := eval.capturedPlans[0]
	if !strings.Contains(p1, `- 	"AGENTS.md", "CLAUDE.md", "CONSTRAINTS.md", "README.md",`) ||
		!strings.Contains(p1, `+ 	"CONSTRAINTS.md", "README.md",`) ||
		strings.Contains(p1, "var manifests") || strings.Contains(p1, `"go.mod"`) {
		t.Fatalf("expected net -/+ hunk diff without unchanged context lines, got:\n%s", p1)
	}

	orig2 := "keep1\nAGENTS.md\nkeep2"
	new2 := "keep1\nkeep2"
	_ = os.WriteFile(targetFile, []byte(orig2), 0o600)
	delPayload := fmt.Sprintf(`{"conversationId":"conv-diff-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":%q,"Replacement":%q}}}`,
		repo, targetFile, orig2, new2)
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(delPayload), "")
	_ = os.WriteFile(targetFile, []byte(new2), 0o600)
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(stopPayload), "")
	if len(eval.capturedPlans) != 2 || !strings.Contains(eval.capturedPlans[1], "Removed:\nAGENTS.md") {
		t.Fatalf("expected pure deletion to report Removed: block, got: %+v", eval.capturedPlans)
	}

	longOld := "AGENTS.md " + strings.Repeat("한글규칙문맥", 120)
	longNew := "CONSTRAINTS.md 핵심규칙 " + strings.Repeat("가나다라마바사", 40)
	_ = os.WriteFile(targetFile, []byte("ctx\n"+longOld), 0o600)
	utf8Payload := fmt.Sprintf(`{"conversationId":"conv-diff-1","workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Target":%q,"Replacement":%q}}}`,
		repo, targetFile, "ctx\n"+longOld, "ctx\n"+longNew)
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(utf8Payload), "")
	_ = os.WriteFile(targetFile, []byte("ctx\n"+longNew), 0o600)
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(stopPayload), "")
	if len(eval.capturedPlans) != 3 {
		t.Fatalf("expected 3rd VerifyPlan call at Stop, got %d", len(eval.capturedPlans))
	}
	p3 := eval.capturedPlans[2]
	if !utf8.ValidString(p3) || !strings.Contains(p3, "- AGENTS.md") || !strings.Contains(p3, "+ CONSTRAINTS.md") {
		t.Fatalf("expected valid UTF-8 and changed hunks preserved after truncation, valid=%v got:\n%s", utf8.ValidString(p3), p3)
	}
}

type spyReadyEval struct {
	stubEval
	scoreCalls int
}

func (s *spyReadyEval) Name() string { return "spy-ready" }
func (s *spyReadyEval) ScoreRelevance(ctx context.Context, task string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	s.scoreCalls++
	return s.stubEval.ScoreRelevance(ctx, task, facts)
}

func TestFirstRunEngineSetupGateAndExplicitEnvProvider(t *testing.T) {
	for _, k := range []string{
		"PRAXIS_PROVIDER", "GEMINI_API_KEY", "TYPESAFE_API_KEY",
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
		t.Fatalf("expected ambiguous multiple provider env vars without PRAXIS_PROVIDER to return empty, got %q", got)
	}
	t.Setenv("PRAXIS_PROVIDER", "local")
	if got := domain.ResolveExplicitEnvProvider(); got != "local" {
		t.Fatalf("expected explicit PRAXIS_PROVIDER to win over multiple env vars, got %q", got)
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

	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("PRAXIS_PROVIDER", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LOCAL_MODEL_URL", "")
	st.mem.Router().AttachLocalManager(localmodel.NewManager(t.TempDir()))

	out1, code1, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(`{"session_id":"sess-setup-gate"}`), repo)
	if err != nil || code1 != 0 {
		t.Fatalf("ExecuteHook start unready: code=%d err=%v", code1, err)
	}
	if !strings.Contains(out1, "[PRAXIS ENGINE SETUP REQUIRED]") || !strings.Contains(out1, "/#engines") {
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
	if strings.Contains(out2, "[PRAXIS ENGINE SETUP REQUIRED]") {
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

	st.mem.Router().AttachLocalManager(nil)
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

func TestReadOnlyShellPureGoEnvPrefixes(t *testing.T) {
	ctx := context.Background()
	eval := &batchVerifyEval{
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
	if _, err := st.mem.CommitMemory(ctx, "CGO_ENABLED=0 순수 Go 바이너리 빌드만 허용한다", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	eval.violate = true
	preTool := func(cmd string) string {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"session_id": "sess-go-env-prefix",
			"tool_name":  "run_command",
			"tool_input": map[string]any{"command": cmd},
			"cwd":        repo,
		})
		out, code, err := st.gr.ExecuteHook(ctx, "antigravity-pretool", payload, repo)
		if err != nil || code != 0 {
			t.Fatalf("ExecuteHook(%q) error=%v code=%d out=%s", cmd, err, code, out)
		}
		return out
	}

	bypassCmds := []string{
		"GOOS=darwin CGO_ENABLED=0 go vet ./...",
		"GOOS=linux CGO_ENABLED=0 go vet ./...",
		"GOOS=windows CGO_ENABLED=0 go vet ./...",
		"CGO_ENABLED=0 GOARCH=arm64 go test -race -count=1 ./...",
		"GOOS=darwin CGO_ENABLED=0 go vet ./... && GOOS=linux CGO_ENABLED=0 go vet ./...",
		"go list ./...",
		"go version",
		"go env GOPATH",
		"gofmt -l .",
		"gofmt -d internal/modules/guardrail",
	}
	for _, cmd := range bypassCmds {
		before := len(eval.capturedPlans)
		out := preTool(cmd)
		if len(eval.capturedPlans) != before {
			t.Fatalf("expected pure-Go read-only command %q to bypass VerifyPlan, calls grew %d -> %d", cmd, before, len(eval.capturedPlans))
		}
		if !strings.Contains(out, `"decision":"allow"`) {
			t.Fatalf("expected bypass command %q to return allow, got %s", cmd, out)
		}
	}

	checkedCmds := []string{
		"CGO_ENABLED=1 go test ./...",
		"CGO_ENABLED=0 go build ./cmd/praxis",
		"FOO=bar go vet ./...",
		"CGO_ENABLED=0 ls",
		"go test -c ./...",
		"go test -o=/tmp/t.bin ./...",
		"go test -exec=true ./...",
		"go env -w GOOS=linux",
		"gofmt -w .",
		"gofmt -lw internal/",
	}
	for _, cmd := range checkedCmds {
		before := len(eval.capturedPlans)
		out := preTool(cmd)
		if len(eval.capturedPlans) != before+1 {
			t.Fatalf("expected non-exempt command %q to invoke VerifyPlan once, calls=%d -> %d", cmd, before, len(eval.capturedPlans))
		}
		if !strings.Contains(out, `"decision":"deny"`) {
			t.Fatalf("expected non-exempt command %q to deny when violate=true, got %s", cmd, out)
		}
	}
}

type lazyOverrideVerifyEval struct {
	stubEval
	capturedPlans  []string
	capturedCounts []int
}

func (e *lazyOverrideVerifyEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	e.capturedPlans = append(e.capturedPlans, plan)
	e.capturedCounts = append(e.capturedCounts, len(rules))
	if strings.Contains(plan, "Turn Context") && strings.Contains(plan, "allow explicit exception") {
		return nil, nil
	}
	if len(rules) > 0 {
		return []domain.PlanViolation{{Fact: rules[0], Probability: 0.94, Confidence: 0.90}}, nil
	}
	return nil, nil
}

func TestGuardrailOmitsStaleTurnPromptAfterMutation(t *testing.T) {
	ctx := context.Background()
	eval := &batchVerifyEval{
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
	if _, err := st.mem.CommitMemory(ctx, "project_progress stores events without status", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	const sessID = "sess-mutated-prompt"
	const prompt = "drop project_progress.status and update rule"
	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": sessID,
		"cwd":        repo,
		"prompt":     prompt,
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-turn", turnPayload, repo)

	buildPayload, _ := json.Marshal(map[string]any{
		"session_id": sessID,
		"cwd":        repo,
		"tool_name":  "run_command",
		"tool_input": map[string]any{"command": "CGO_ENABLED=0 go build ./cmd/praxis"},
	})

	if _, _, err := st.gr.ExecuteHook(ctx, "claude-pretool", buildPayload, repo); err != nil {
		t.Fatalf("ExecuteHook clean pretool: %v", err)
	}
	if len(eval.capturedPlans) != 1 || strings.Contains(eval.capturedPlans[0], prompt) {
		t.Fatalf("expected clean Phase 1 call without prompt prose, got %+v", eval.capturedPlans)
	}

	eval.violate = true
	if _, _, err := st.gr.ExecuteHook(ctx, "claude-pretool", buildPayload, repo); err != nil {
		t.Fatalf("ExecuteHook violated pretool before mutate: %v", err)
	}
	if len(eval.capturedPlans) != 3 || strings.Contains(eval.capturedPlans[1], prompt) || !strings.Contains(eval.capturedPlans[2], prompt) {
		t.Fatalf("expected Phase 1 (clean) + Phase 2 (with prompt) before mutate, got %+v", eval.capturedPlans)
	}

	st.mem.MarkTurnScopeMutatedCtx(ctx, scope)
	if _, _, err := st.gr.ExecuteHook(ctx, "claude-pretool", buildPayload, repo); err != nil {
		t.Fatalf("ExecuteHook pretool after mutate: %v", err)
	}
	if len(eval.capturedPlans) != 4 {
		t.Fatalf("expected single Phase 1 call without Phase 2 after mutate, got %d plans", len(eval.capturedPlans))
	}
	if strings.Contains(eval.capturedPlans[3], prompt) {
		t.Fatalf("expected mutated turn NOT to re-attach stale rule-change prompt, got:\n%s", eval.capturedPlans[3])
	}
}

func TestGuardrailLazyOverrideAllowsExplicitExceptionAcrossFlaggedRulesOnly(t *testing.T) {
	ctx := context.Background()
	eval := &lazyOverrideVerifyEval{
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
	for _, r := range []string{"Rule one forbid X", "Rule two forbid Y", "Rule three forbid Z"} {
		if _, err := st.mem.CommitMemory(ctx, r, "architecture", scope); err != nil {
			t.Fatalf("CommitMemory: %v", err)
		}
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	const sessID = "sess-lazy-override"
	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": sessID,
		"cwd":        repo,
		"prompt":     "Please allow explicit exception to run tool once",
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-turn", turnPayload, repo)

	cmdPayload, _ := json.Marshal(map[string]any{
		"session_id": sessID,
		"cwd":        repo,
		"tool_name":  "run_command",
		"tool_input": map[string]any{"command": "echo-via-custom-runner"},
	})
	out, code, err := st.gr.ExecuteHook(ctx, "antigravity-pretool", cmdPayload, repo)
	if err != nil || code != 0 || !strings.Contains(out, `"decision":"allow"`) {
		t.Fatalf("expected lazy Phase 2 override to allow cleanly, got code=%d err=%v out=%s", code, err, out)
	}
	if len(eval.capturedCounts) != 2 || eval.capturedCounts[0] != 3 || eval.capturedCounts[1] != 1 {
		t.Fatalf("expected Phase 1 (3 rules) -> Phase 2 (only 1 flagged rule), got counts=%v", eval.capturedCounts)
	}
}

type teamVerifyEval struct {
	stubEval
	verifyFn func(plan string, rules []domain.Fact) []domain.PlanViolation
}

func (e *teamVerifyEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if e.verifyFn != nil {
		return e.verifyFn(plan, rules), nil
	}
	return nil, nil
}

func TestGuardrailSyncsAndEnforcesTeamRulePack(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "teamrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".praxis"), 0755); err != nil {
		t.Fatal(err)
	}

	ev := &teamVerifyEval{}
	stack := newTestStack(t, filepath.Join(tmp, "mem.db"), ev)
	rp := rulepack.New(stack.mem)
	stack.mem.AttachExtraFactsProvider(rp)
	stack.gr.AttachRulePack(rp)

	ctx := context.Background()
	scope := "project:teamrepo"

	ev.dec = domain.MutationDecision{Action: "INSERT", Category: "database", Cardinality: "exclusive", IsPermanent: 0.9, Confidence: 0.95}
	if _, err := stack.mem.CommitMemoryFull(ctx, "Use go-sqlite3 CGO driver in dev builds", "database", "sqlite", "", scope); err != nil {
		t.Fatal(err)
	}
	locals, err := stack.mem.LoadActiveFacts(ctx, scope, scope, "")
	if err != nil || len(locals) != 1 {
		t.Fatalf("expected 1 local fact, got %+v err=%v", locals, err)
	}
	localID := locals[0].ID

	docBytes, err := rulepack.InsertRule(nil, scope, domain.RulePlacementProposal{
		Category: "database",
		Title:    "Always use pure-Go modernc.org/sqlite with CGO_ENABLED=0",
		AddedBy:  "Alice",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, domain.RulePackRelPath), docBytes, 0644); err != nil {
		t.Fatal(err)
	}

	ev.dec = domain.MutationDecision{
		Action:          "OVERWRITE",
		ConflictFactID:  localID,
		ConflictFactIDs: []int64{localID},
		Cardinality:     "exclusive",
		Confidence:      0.94,
	}

	startPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-team-hook",
		"cwd":        repoDir,
	})
	hookOut, code, err := stack.gr.ExecuteHook(ctx, "claude-start", startPayload, repoDir)
	if err != nil || code != 0 {
		t.Fatalf("claude-start RunHook failed: code=%d err=%v out=%s", code, err, hookOut)
	}
	if !strings.Contains(hookOut, "[TEAM project:teamrepo/database]") || !strings.Contains(hookOut, "TEAM RULE CONFLICT DETECTED") {
		t.Fatalf("expected hook output to deliver TEAM rule and conflict notice, got:\n%s", hookOut)
	}
	if strings.Contains(hookOut, "TEAM:r-") {
		t.Fatalf("did not expect internal hash key exposed in hook output, got:\n%s", hookOut)
	}

	teamKey := domain.ComputeRuleKey("database", "Always use pure-Go modernc.org/sqlite with CGO_ENABLED=0")

	ev.verifyFn = func(plan string, rules []domain.Fact) []domain.PlanViolation {
		for _, r := range rules {
			if r.IsTeam() && r.RuleKey == teamKey && strings.Contains(plan, "CGO_ENABLED=1") {
				return []domain.PlanViolation{{Fact: r, Probability: 0.96, Confidence: 0.95}}
			}
		}
		return nil
	}
	vOut, err := stack.gr.VerifyPlan(ctx, "Run CGO_ENABLED=1 with modernc.org/sqlite", scope)
	if err != nil {
		t.Fatalf("VerifyPlan failed: %v", err)
	}
	if !strings.Contains(vOut, "database TEAM]") || !strings.Contains(vOut, "modernc.org/sqlite") {
		t.Fatalf("expected VerifyPlan violation to cite TEAM rule cleanly, got:\n%s", vOut)
	}

	toolPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-team-hook",
		"cwd":        repoDir,
		"tool_name":  "Bash",
		"tool_input": map[string]any{
			"command": "CGO_ENABLED=1 go test ./internal/infra/sqlite/...",
		},
	})
	stack.mem.MarkTurnScopeMutatedCtx(ctx, scope)
	gOut, gCode, err := stack.gr.ExecuteHook(ctx, "claude-pretool", toolPayload, repoDir)
	if err != nil || gCode != 2 {
		t.Fatalf("claude-pretool RunHook expected exit code 2 (block): code=%d err=%v out=%s", gCode, err, gOut)
	}
	if !strings.Contains(gOut, "block") || !strings.Contains(gOut, "database TEAM") {
		t.Fatalf("expected PreToolUse guardrail block mentioning TEAM rule, got:\n%s", gOut)
	}

	if err := rp.ResolveConflict(ctx, scope, teamKey, "local_override", "Testing override"); err != nil {
		t.Fatalf("ResolveConflict failed: %v", err)
	}
	vAfterOverride, err := stack.gr.VerifyPlan(ctx, "Run CGO_ENABLED=1 with modernc.org/sqlite", scope)
	if err != nil {
		t.Fatalf("VerifyPlan after local_override failed: %v", err)
	}
	if strings.Contains(vAfterOverride, "modernc.org/sqlite") {
		t.Fatalf("expected local_override to stop enforcing overridden TEAM rule, got:\n%s", vAfterOverride)
	}
}

type remoteViolEval struct {
	ruleMu            sync.Mutex
	lastVerifiedRules []domain.Fact
	stubEval
}

func (e *remoteViolEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	e.ruleMu.Lock()
	e.lastVerifiedRules = append([]domain.Fact(nil), rules...)
	e.ruleMu.Unlock()
	var out []domain.PlanViolation
	for _, r := range rules {
		if strings.Contains(r.Content, "PERSONAL_ONLY_TOKEN") && strings.Contains(plan, "PERSONAL_ONLY_TOKEN") {
			out = append(out, domain.PlanViolation{
				Fact:        r,
				Probability: 0.95,
				Confidence:  0.95,
			})
		}
		if strings.Contains(r.Content, "PROJECT_FORBIDDEN_TOKEN") && strings.Contains(plan, "PROJECT_FORBIDDEN_TOKEN") {
			out = append(out, domain.PlanViolation{
				Fact:        r,
				Probability: 0.95,
				Confidence:  0.95,
			})
		}
	}
	return out, nil
}

func TestRemoteThinClientIsolationAndHooks(t *testing.T) {
	eval := &remoteViolEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{Action: "INSERT", Category: "security", IsPermanent: 0.95, IsGlobalInvariant: 0.95, Confidence: 0.95},
		},
	}
	st := newTestStack(t, "", eval)
	rp := rulepack.New(st.mem)
	st.gr.AttachRulePack(rp)

	ctxAlice := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "alice",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxBob := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "bob",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxAdmin := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "admin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})

	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PROJECT_FORBIDDEN_TOKEN", "security", "", "", "project:teamapp"); err != nil {
		t.Fatalf("alice commit project rule: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PERSONAL_ONLY_TOKEN", "preference", "", "", "personal"); err != nil {
		t.Fatalf("alice commit personal rule: %v", err)
	}

	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call GLOBAL_PRIVATE_TOKEN", "architecture", "", "", "global"); err != nil {
		t.Fatalf("alice commit own global rule: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctxAdmin, "Always run CGO_ENABLED=0 builds", "build", "", "", "global"); err != nil {
		t.Fatalf("admin commit own global rule: %v", err)
	}

	for _, sc := range []string{"personal", "global"} {
		for name, callerCtx := range map[string]context.Context{"bob": ctxBob, "admin": ctxAdmin} {
			page, err := st.mem.QueryRecordsPage(callerCtx, domain.RecordQuery{Scope: sc, State: "all"})
			if err != nil {
				t.Fatalf("%s QueryRecordsPage(%s): %v", name, sc, err)
			}
			for _, rec := range page.Records {
				if rec.Account == "alice" {
					t.Fatalf("%s unexpectedly saw Alice %s memory: %+v", name, sc, rec)
				}
			}
		}
	}

	turnPayload, _ := json.Marshal(map[string]any{
		"session_id":  "s1",
		"user_prompt": "implement endpoint",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			TeamRuleDocs: map[string]string{
				"project:teamapp": "# Team Rules\n\n## Architecture (`architecture`)\n- Use parameterized queries. · `@alice` · `2026-11-18`\n",
			},
			GitHeadByScope: map[string]string{"project:teamapp": "abc1234"},
			GitCommitsByScope: map[string][]domain.GitCommitBrief{
				"project:teamapp": {{SHA: "abc1234", Date: "2026-11-18", Subject: "feat: remote client sync"}},
			},
		},
	})
	if out, code, err := st.gr.ExecuteHook(ctxAlice, "claude-turn", turnPayload, ""); err != nil || code != 0 || out == "" {
		t.Fatalf("claude-turn failed: code=%d err=%v out=%s", code, err, out)
	}
	st.mem.MarkTurnScopeMutatedCtx(ctxAlice, "project:teamapp")
	eval.dec.Action = "IGNORE"
	if rules, err := rp.ListTeamRules(ctxAlice, "project:teamapp"); err != nil || len(rules) != 1 || !strings.Contains(rules[0].Title+" "+rules[0].How, "parameterized") {
		t.Fatalf("expected remote TeamRuleDocs ingested cleanly, got %+v (err=%v)", rules, err)
	}
	ov, err := st.prog.GetProgressOverviewByTrack(ctxAlice, "project:teamapp", "", "", "")
	if err != nil || len(ov.Records) == 0 {
		t.Fatalf("expected remote GitCommitsByScope synced into progress, got len=%d err=%v", len(ov.Records), err)
	}
	foundCommit := false
	for _, r := range ov.Records {
		if r.CommitSHA == "abc1234" && r.Account == "alice" {
			foundCommit = true
		}
	}
	if !foundCommit {
		t.Fatalf("expected git commit abc1234 attributed to alice in progress records: %+v", ov.Records)
	}

	pretoolPayload, _ := json.Marshal(map[string]any{
		"session_id": "s1",
		"tool_name":  "edit_file",
		"tool_input": map[string]any{"file_path": "/nonexistent/client/app.go"},
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes:   []string{"project:teamapp"},
			TargetScope:       "project:teamapp",
			TargetRelPath:     "app.go",
			RemoteManagedEdit: true,
		},
	})
	if _, code, err := st.gr.ExecuteHook(ctxAlice, "claude-pretool", pretoolPayload, ""); err != nil || code != 0 {
		t.Fatalf("expected remote pretool edit to succeed without host filesystem, code=%d err=%v", code, err)
	}

	if err := st.mem.SaveConfig(ctxAdmin, map[string]string{"include_personal_in_project": "1"}); err != nil {
		t.Fatalf("SaveConfig include_personal_in_project: %v", err)
	}
	stopPersonalPayload, _ := json.Marshal(map[string]any{
		"session_id": "s1",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			DiffSections: []domain.HookDiffSection{{
				Scope: "project:teamapp",
				Label: "app.go",
				Body:  "File: app.go\nAdded:\nPERSONAL_ONLY_TOKEN()",
			}},
		},
	})
	outStopPers, _, err := st.gr.ExecuteHook(ctxAlice, "claude-stop", stopPersonalPayload, "")
	if err != nil || !strings.Contains(outStopPers, "block") {
		t.Fatalf("expected Stop hook to block on personal violation, out=%s err=%v", outStopPers, err)
	}

	aliceLogs, err := st.gr.QueryGuardrailLogsPage(ctxAlice, domain.GuardrailLogQuery{State: "all", PageSize: 20})
	if err != nil || aliceLogs.TotalCount != 1 || !aliceLogs.Records[0].IsPrivate || aliceLogs.Records[0].Account != "alice" {
		t.Fatalf("expected 1 private guardrail log for alice, got %+v (err=%v)", aliceLogs, err)
	}
	for name, callerCtx := range map[string]context.Context{"bob": ctxBob, "admin": ctxAdmin} {
		otherLogs, err := st.gr.QueryGuardrailLogsPage(callerCtx, domain.GuardrailLogQuery{State: "all", PageSize: 20})
		if err != nil || otherLogs.TotalCount != 0 {
			t.Fatalf("%s unexpectedly saw alice's private guardrail log: %+v (err=%v)", name, otherLogs, err)
		}
	}

	stopProjPayload, _ := json.Marshal(map[string]any{
		"session_id": "s2",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			DiffSections: []domain.HookDiffSection{{
				Scope: "project:teamapp",
				Label: "app.go",
				Body:  "File: app.go\nAdded:\nPROJECT_FORBIDDEN_TOKEN()",
			}},
		},
	})
	st.mem.MarkTurnScopeMutatedCtx(ctxAlice, "project:teamapp")
	outStopProj, _, err := st.gr.ExecuteHook(ctxAlice, "claude-stop", stopProjPayload, "")
	if err != nil || !strings.Contains(outStopProj, "block") {
		t.Fatalf("expected Stop hook to block on project violation, out=%s err=%v", outStopProj, err)
	}
	adminLogs, _ := st.gr.QueryGuardrailLogsPage(ctxAdmin, domain.GuardrailLogQuery{State: "all", PageSize: 20})
	if adminLogs.TotalCount != 1 || adminLogs.Records[0].IsPrivate || adminLogs.Records[0].Account != "alice" {
		t.Fatalf("expected admin to see 1 non-private alice log only, got %+v", adminLogs)
	}
	bobLogs, _ := st.gr.QueryGuardrailLogsPage(ctxBob, domain.GuardrailLogQuery{State: "all", PageSize: 20})
	if bobLogs.TotalCount != 0 {
		t.Fatalf("expected member Bob to see 0 logs (member isolated to own logs), got %+v", bobLogs)
	}

	preCommitCtx := domain.WithClientHookContext(ctxAlice, &domain.ClientHookContext{
		CanonicalScopes: []string{"project:teamapp"},
		TargetScope:     "project:teamapp",
		StagedDiffs: []domain.HookDiffSection{{
			Scope: "project:teamapp",
			Label: "staged.go",
			Body:  "+PROJECT_FORBIDDEN_TOKEN()",
		}},
	})
	_, pcCode, err := st.gr.ExecuteHook(preCommitCtx, "git-precommit", nil, "")
	if err != nil || pcCode == 0 {
		t.Fatalf("expected git-precommit to fail on remote StagedDiffs violation, code=%d err=%v", pcCode, err)
	}
}

func TestRemoteServerTeamRuleDocsPerAccountIsolation(t *testing.T) {
	eval := &remoteViolEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{Action: "IGNORE", Category: "architecture", Confidence: 0.9},
		},
	}
	st := newTestStack(t, "", eval)
	rp := rulepack.New(st.mem)
	st.gr.AttachRulePack(rp)

	ctxAlice := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "alice",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxBob := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "bob",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxAdmin := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "admin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})

	aliceTurn, _ := json.Marshal(map[string]any{
		"session_id":  "s-alice",
		"user_prompt": "check alice rules",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			TeamRuleDocs: map[string]string{
				"project:teamapp": "# Team Rules\n\n## Architecture (`architecture`)\n- Never skip Alice check. · `@alice` · `2026-11-18`\n",
			},
		},
	})
	if _, code, err := st.gr.ExecuteHook(ctxAlice, "claude-turn", aliceTurn, ""); err != nil || code != 0 {
		t.Fatalf("alice turn hook failed: code=%d err=%v", code, err)
	}

	bobTurn, _ := json.Marshal(map[string]any{
		"session_id":  "s-bob",
		"user_prompt": "check bob rules",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			TeamRuleDocs: map[string]string{
				"project:teamapp": "# Team Rules\n\n## Architecture (`architecture`)\n- Bob branch rule only. · `@bob` · `2026-11-18`\n",
			},
		},
	})
	if _, code, err := st.gr.ExecuteHook(ctxBob, "claude-turn", bobTurn, ""); err != nil || code != 0 {
		t.Fatalf("bob turn hook failed: code=%d err=%v", code, err)
	}

	aliceRules, err := rp.ListTeamRules(ctxAlice, "project:teamapp")
	if err != nil || len(aliceRules) != 1 || !strings.Contains(aliceRules[0].Title+" "+aliceRules[0].How, "Alice check") {
		t.Fatalf("expected Alice cache isolated from Bob's TeamRuleDocs, got %+v (err=%v)", aliceRules, err)
	}
	bobRules, err := rp.ListTeamRules(ctxBob, "project:teamapp")
	if err != nil || len(bobRules) != 1 || !strings.Contains(bobRules[0].Title+" "+bobRules[0].How, "Bob branch") {
		t.Fatalf("expected Bob cache isolated from Alice's TeamRuleDocs, got %+v (err=%v)", bobRules, err)
	}

	bobClear, _ := json.Marshal(map[string]any{
		"session_id":  "s-bob-clear",
		"user_prompt": "clear bob rules",
		"client_ctx": domain.ClientHookContext{
			CanonicalScopes: []string{"project:teamapp"},
			TargetScope:     "project:teamapp",
			TeamRuleDocs:    map[string]string{"project:teamapp": ""},
		},
	})
	if _, code, err := st.gr.ExecuteHook(ctxBob, "claude-turn", bobClear, ""); err != nil || code != 0 {
		t.Fatalf("bob clear turn failed: code=%d err=%v", code, err)
	}
	if gotBob, _ := rp.ListTeamRules(ctxBob, "project:teamapp"); len(gotBob) != 0 {
		t.Fatalf("expected Bob cache cleared cleanly, got %+v", gotBob)
	}
	if gotAlice, _ := rp.ListTeamRules(ctxAlice, "project:teamapp"); len(gotAlice) != 1 {
		t.Fatalf("expected Alice cache unaffected by Bob clear, got %+v", gotAlice)
	}

	if err := rp.SetAuthorName(ctxBob, "@bob"); err == nil {
		t.Fatalf("expected SetAuthorName by member to be rejected in ServerMode")
	}
	if err := rp.SetAuthorName(ctxAdmin, "@lead"); err != nil {
		t.Fatalf("expected SetAuthorName by admin to succeed: %v", err)
	}

	evalTeam := &remoteViolEval{
		stubEval: stubEval{dec: domain.MutationDecision{Action: "INSERT", Category: "security", IsPermanent: 0.95, IsGlobalInvariant: 0.95, Confidence: 0.95}},
	}
	stPrio := newTestStack(t, "", evalTeam)
	rpPrio := rulepack.New(stPrio.mem)
	stPrio.mem.AttachExtraFactsProvider(rpPrio)
	stPrio.gr.AttachRulePack(rpPrio)

	if _, err := stPrio.mem.CommitMemoryFull(ctxAlice, "Never call PROJECT_FORBIDDEN_TOKEN", "security", "", "", "project:teamapp"); err != nil {
		t.Fatal(err)
	}
	if _, err := stPrio.mem.CommitMemoryFull(ctxAlice, "Never call PERSONAL_ONLY_TOKEN", "architecture", "", "", "global"); err != nil {
		t.Fatal(err)
	}
	teamDoc := "# Team Rules\n\n## Architecture (`architecture`)\n- Never call PROJECT_FORBIDDEN_TOKEN from team rule. · `@lead` · `2026-11-18`\n"
	if err := rpPrio.IngestTeamRuleDoc(ctxAlice, "project:teamapp", teamDoc); err != nil {
		t.Fatal(err)
	}
	rulesAlice, err := rpPrio.ListTeamRules(ctxAlice, "project:teamapp")
	if err != nil || len(rulesAlice) != 1 {
		t.Fatalf("expected 1 ingested team rule, got %+v (err=%v)", rulesAlice, err)
	}
	if err := rpPrio.ResolveConflict(ctxAlice, "project:teamapp", rulesAlice[0].RuleKey, "local_override", "try override"); err == nil {
		t.Fatalf("expected local_override resolution rejected in ServerMode")
	}
	if err := rpPrio.UpsertConflictState(ctxAlice, "project:teamapp", rulesAlice[0].RuleKey, domain.RuleStateLocalOverride, 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	factsAlice, err := rpPrio.ListTeamRuleFacts(ctxAlice, "project:teamapp")
	if err != nil || len(factsAlice) != 1 {
		t.Fatalf("expected Team Rule Pack still enforced in ServerMode despite local_override row, got %+v (err=%v)", factsAlice, err)
	}

	ctxSession := domain.WithSessionID(ctxAlice, "s-prio")
	qOut, err := stPrio.mem.QuerySessionMemory(ctxSession, "s-prio", "write code", "", "project:teamapp", false)
	if err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}
	idxTeam := strings.Index(qOut, "TEAM]")
	if idxTeam < 0 {
		idxTeam = strings.Index(qOut, "TEAM ")
	}
	idxProj := strings.Index(qOut, "Never call PROJECT_FORBIDDEN_TOKEN")
	idxGlob := strings.Index(qOut, "Never call PERSONAL_ONLY_TOKEN")
	if idxTeam < 0 || idxProj < 0 || idxGlob < 0 || !(idxTeam < idxProj && idxProj < idxGlob) {
		t.Fatalf("expected Team -> project -> global injection order, got:\n%s", qOut)
	}

	verOut, err := stPrio.gr.VerifyPlan(ctxAlice, "PROJECT_FORBIDDEN_TOKEN() and PERSONAL_ONLY_TOKEN()", "project:teamapp")
	if err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}
	vTeam := strings.Index(verOut, "TEAM]")
	vPriv := strings.Index(verOut, "PERSONAL_ONLY_TOKEN")
	if vTeam < 0 || vPriv < 0 || vTeam >= vPriv {
		t.Fatalf("expected VerifyPlan TEAM violation listed before global/personal violation, got:\n%s", verOut)
	}
	privLogs, _ := stPrio.gr.QueryGuardrailLogsPage(ctxAlice, domain.GuardrailLogQuery{State: "all", PageSize: 20})
	otherLogs, _ := stPrio.gr.QueryGuardrailLogsPage(ctxAdmin, domain.GuardrailLogQuery{State: "all", PageSize: 20})
	if privLogs.TotalCount != 1 || !privLogs.Records[0].IsPrivate || otherLogs.TotalCount != 0 {
		t.Fatalf("expected Alice global violation logged IsPrivate=true and hidden from admin: alice=%+v admin=%+v", privLogs, otherLogs)
	}

	evalKV := &remoteViolEval{
		stubEval: stubEval{dec: domain.MutationDecision{Action: "INSERT", Category: "architecture", IsPermanent: 0.95, Confidence: 0.95}},
	}
	stKV := newTestStack(t, "", evalKV)
	rpKV := rulepack.New(stKV.mem)
	stKV.gr.AttachRulePack(rpKV)
	if _, err := stKV.mem.CommitMemoryFull(ctxAlice, "Alice older-ID private global rule", "architecture", "", "", "global"); err != nil {
		t.Fatal(err)
	}
	if _, err := stKV.mem.CommitMemoryFull(ctxAlice, "Shared newer-ID project rule", "architecture", "", "", "project:teamapp"); err != nil {
		t.Fatal(err)
	}
	if err := rpKV.IngestTeamRuleDoc(ctxAlice, "project:teamapp", teamDoc); err != nil {
		t.Fatal(err)
	}
	if err := rpKV.IngestTeamRuleDoc(ctxBob, "project:teamapp", teamDoc); err != nil {
		t.Fatal(err)
	}
	if _, err := stKV.gr.VerifyPlan(ctxAlice, "safe code", "project:teamapp"); err != nil {
		t.Fatal(err)
	}
	aliceRulesPassed := append([]domain.Fact(nil), evalKV.lastVerifiedRules...)
	if _, err := stKV.gr.VerifyPlan(ctxBob, "safe code", "project:teamapp"); err != nil {
		t.Fatal(err)
	}
	bobRulesPassed := append([]domain.Fact(nil), evalKV.lastVerifiedRules...)
	if len(aliceRulesPassed) != 3 || len(bobRulesPassed) != 2 ||
		!aliceRulesPassed[0].IsTeam() || aliceRulesPassed[1].Scope != "project:teamapp" || aliceRulesPassed[2].Scope != "global" ||
		aliceRulesPassed[0].ID != bobRulesPassed[0].ID || aliceRulesPassed[1].ID != bobRulesPassed[1].ID {
		t.Fatalf("expected shared [TEAM, project:*] KV prefix before private global tail:\nalice=%+v\nbob=%+v", aliceRulesPassed, bobRulesPassed)
	}

	stKV.mem.RecordTurnState(ctxAlice, "s-iso", "project:teamapp", "alice prompt")
	stKV.mem.RecordTurnState(ctxBob, "s-iso", "project:teamapp", "bob prompt")
	stKV.mem.MarkTurnScopeMutatedCtx(ctxAlice, "global")
	if !stKV.mem.IsTurnMutated(ctxAlice, "s-iso", "project:teamapp") {
		t.Fatalf("expected alice global mutation to mark alice's own turn mutated")
	}
	if stKV.mem.IsTurnMutated(ctxBob, "s-iso", "project:teamapp") {
		t.Fatalf("expected alice global mutation NOT to mark bob's turn mutated in ServerMode")
	}
	if err := stKV.gr.SkipOnboarding(ctxAlice, "project:teamapp"); err != nil {
		t.Fatalf("SkipOnboarding alice: %v", err)
	}
	if stKV.mem.IsTurnMutated(ctxBob, "s-iso", "project:teamapp") {
		t.Fatalf("expected alice SkipOnboarding NOT to mark bob's turn mutated in ServerMode")
	}
}
