package guardrail_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/rulepack"
)

type remoteViolEval struct {
	matchRuleContent string
	stubEval
}

func (e *remoteViolEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
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

	// Alice adds a shared project rule and a private personal rule.
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PROJECT_FORBIDDEN_TOKEN", "security", "", "", "project:teamapp"); err != nil {
		t.Fatalf("alice commit project rule: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PERSONAL_ONLY_TOKEN", "preference", "", "", "personal"); err != nil {
		t.Fatalf("alice commit personal rule: %v", err)
	}

	// Global scope mutation rejected for member Alice, allowed for admin.
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Member cannot change global", "architecture", "", "", "global"); err == nil {
		t.Fatalf("expected member CommitMemoryFull on global scope to fail")
	}
	if _, err := st.mem.CommitMemoryFull(ctxAdmin, "Always run CGO_ENABLED=0 builds", "build", "", "", "global"); err != nil {
		t.Fatalf("admin commit global rule: %v", err)
	}

	// Strict personal isolation: neither Bob nor Admin can see Alice's personal memory.
	for name, callerCtx := range map[string]context.Context{"bob": ctxBob, "admin": ctxAdmin} {
		page, err := st.mem.QueryRecordsPage(callerCtx, domain.RecordQuery{Scope: "personal", State: "all"})
		if err != nil {
			t.Fatalf("%s QueryRecordsPage(personal): %v", name, err)
		}
		if len(page.Records) != 0 {
			t.Fatalf("%s unexpectedly saw Alice personal memories: %+v", name, page.Records)
		}
	}

	// Turn hook from Alice ingests remote client TEAM_RULES.md and Git commits attributed to Alice.
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
	// Mark turn mutated and configure evaluator to treat coding turn prompt as already committed / non-rule.
	st.mem.MarkTurnScopeMutated(domain.ScopedSessionID(ctxAlice, "s1"))
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

	// PreToolUse edit with RemoteManagedEdit=true returns allow without host disk access.
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

	// Enable personal-in-project inclusion so Alice's own personal rule triggers a private guardrail violation.
	if err := st.mem.SaveConfig(ctxAdmin, map[string]string{"include_personal_in_project": "1"}); err != nil {
		t.Fatalf("SaveConfig include_personal_in_project: %v", err)
	}
	// Stop hook hitting Alice's PERSONAL rule logs as IsPrivate=true (hidden from both Bob and Admin).
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

	// Alice sees her private log; Bob and Admin do NOT see Alice's private log.
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

	// Stop hook hitting shared PROJECT rule logs IsPrivate=false (visible to Admin, hidden from member Bob).
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
	st.mem.MarkTurnScopeMutated(domain.ScopedSessionID(ctxAlice, "s2"))
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

	// git-precommit via ClientHookContext in ctx with StagedDiffs blocks violations without host git.
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
