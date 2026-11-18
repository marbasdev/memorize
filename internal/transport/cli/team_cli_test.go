package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

type cliTeamEval struct{}

func (cliTeamEval) Name() string { return "cli-team-eval" }
func (cliTeamEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}
func (cliTeamEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{Action: "INSERT", Category: "security", IsPermanent: 0.95, IsGlobalInvariant: 0.95, Confidence: 0.95}, nil
}
func (cliTeamEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if strings.Contains(plan, "BAD_TEAM_TOKEN") && len(rules) > 0 {
		return []domain.PlanViolation{{
			Fact:        rules[0],
			Probability: 0.95,
			Confidence:  0.95,
		}}, nil
	}
	return nil, nil
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	_ = r.Close()
	return code, string(b)
}

func TestCLIUserConnectAndThinClientEndToEnd(t *testing.T) {
	tmpRoot := t.TempDir()
	srvDBPath := filepath.Join(tmpRoot, "server", "memories.db")
	cliDataDir := filepath.Join(tmpRoot, "client")
	cliDBPath := filepath.Join(cliDataDir, "memories.db")
	teamRepo := filepath.Join(tmpRoot, "repos", "team-service")
	soloRepo := filepath.Join(tmpRoot, "repos", "solo-notes")
	if err := os.MkdirAll(teamRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(soloRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEMORY_DATA_DIR", "")
	t.Setenv("MEMORY_DB_PATH", srvDBPath)

	// 1. Admin CLI user lifecycle against server DB.
	code, out := captureStdout(t, func() int {
		return executeArgs([]string{"user", "add", "hostadmin", "--role", "admin", "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 {
		t.Fatalf("user add hostadmin failed (%d): %s", code, out)
	}
	if code, _ := captureStdout(t, func() int {
		return executeArgs([]string{"user", "disable", "hostadmin"}, "test", RuntimeHooks{})
	}); code == 0 {
		t.Fatalf("expected disabling sole admin to exit non-zero")
	}

	code, aliceOut := captureStdout(t, func() int {
		return executeArgs([]string{"user", "add", "alice", "--role", "member", "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 {
		t.Fatalf("user add alice failed (%d): %s", code, aliceOut)
	}
	var aliceIssue domain.UserTokenIssue
	if err := json.Unmarshal([]byte(aliceOut), &aliceIssue); err != nil || aliceIssue.PlainToken == "" {
		t.Fatalf("decode alice token issue failed: %v (%s)", err, aliceOut)
	}

	// 2. Start real in-process HTTP team server backed by srvDBPath.
	mem, err := memory.Open(srvDBPath, cliTeamEval{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)

	ctxAdmin := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account: "hostadmin", Role: domain.RoleAdmin, ServerMode: true, Authenticated: true,
	})
	if _, err := mem.CommitMemoryFull(ctxAdmin, "Never write BAD_TEAM_TOKEN in source files", "security", "", "", "project:team-service"); err != nil {
		t.Fatal(err)
	}

	dashH := dashboard.NewHandler(dashboard.Deps{
		ServerMode: true, DB: mem.DB(), Memory: mem, Progress: prog, Guardrail: gr,
	})
	srv := httptest.NewServer(thttp.NewDaemonMux(thttp.DaemonDeps{
		ServerMode:    true,
		DB:            mem.DB(),
		Memory:        mem,
		Guardrail:     gr,
		Dashboard:     dashH,
		BuildStatusFn: func() thttp.DaemonStatus { return thttp.DaemonStatus{Running: true, Version: "test"} },
	}))
	defer srv.Close()

	// 3. Client binds teamRepo via `memorize connect` using isolated client MEMORY_DB_PATH.
	t.Setenv("MEMORY_DB_PATH", cliDBPath)
	code, connOut := captureStdout(t, func() int {
		return executeArgs([]string{"connect", "--url", srv.URL, "--token", aliceIssue.PlainToken, "--repo", teamRepo, "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 || !strings.Contains(connOut, `"account":"alice"`) {
		t.Fatalf("connect failed: code=%d out=%s", code, connOut)
	}
	if _, ok := config.ResolveRemoteTarget(cliDataDir, soloRepo); ok {
		t.Fatalf("unbound personal repo should remain local solo")
	}

	// 4. Thin Client PreToolUse -> Stop diff roundtrip against team server.
	targetFile := filepath.Join(teamRepo, "handler.go")
	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mark server turn mutated cleanly so Stop runs diff verification.
	ctxAlice := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account: "alice", Role: domain.RoleMember, ServerMode: true, Authenticated: true,
	})
	mem.MarkTurnScopeMutated(domain.ScopedSessionID(ctxAlice, "cli-s1"))

	preJSON, _ := json.Marshal(map[string]any{
		"session_id":      "cli-s1",
		"workspace_roots": []string{teamRepo},
		"tool_name":       "edit_file",
		"tool_input":      map[string]any{"file_path": targetFile},
	})
	_, _, handled := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-pretool", preJSON, time.Now())
	if !handled {
		t.Fatalf("expected remote pretool handled")
	}

	// Simulate local edit inserting forbidden token; remote Stop hook must block & attribute log to alice.
	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() { BAD_TEAM_TOKEN() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stopJSON, _ := json.Marshal(map[string]any{
		"session_id":      "cli-s1",
		"workspace_roots": []string{teamRepo},
	})
	outStop, _, handled := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-stop", stopJSON, time.Now())
	if !handled || !strings.Contains(outStop, "block") {
		t.Fatalf("expected remote Thin Client Stop hook to block BAD_TEAM_TOKEN, out=%s", outStop)
	}
	logs, err := gr.QueryGuardrailLogsPage(ctxAlice, domain.GuardrailLogQuery{State: "all", PageSize: 10})
	if err != nil || logs.TotalCount != 1 || logs.Records[0].Account != "alice" {
		t.Fatalf("expected guardrail log attributed to alice, got %+v (err=%v)", logs, err)
	}
	// Baseline must NOT be wiped while Stop blocked.
	if rem := config.ConsumeClientTurnDiffs(cliDataDir, "cli-s1", false); len(rem) == 0 {
		t.Fatalf("expected client baseline kept while Stop blocked")
	}

	// Fix file -> Stop passes and clears client baseline.
	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() { cleanCall() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outClean, _, _ := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-stop", stopJSON, time.Now())
	if strings.Contains(outClean, "block") {
		t.Fatalf("expected clean Stop to pass, got %s", outClean)
	}
	if rem := config.ConsumeClientTurnDiffs(cliDataDir, "cli-s1", false); len(rem) != 0 {
		t.Fatalf("expected client baseline cleared after passing Stop, got %d", len(rem))
	}

	// 5. Disconnect reverts teamRepo back to local mode.
	code, discOut := captureStdout(t, func() int {
		return executeArgs([]string{"disconnect", "--repo", teamRepo, "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 || !strings.Contains(discOut, `"disconnected":true`) {
		t.Fatalf("disconnect failed: code=%d out=%s", code, discOut)
	}
	if _, ok := config.ResolveRemoteTarget(cliDataDir, teamRepo); ok {
		t.Fatalf("expected teamRepo unbound after disconnect")
	}
}
