package guardrail_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/rulepack"
)

type remoteViolEval struct {
	matchRuleContent  string
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

	// Alice adds a shared project rule and a private personal rule.
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PROJECT_FORBIDDEN_TOKEN", "security", "", "", "project:teamapp"); err != nil {
		t.Fatalf("alice commit project rule: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call PERSONAL_ONLY_TOKEN", "preference", "", "", "personal"); err != nil {
		t.Fatalf("alice commit personal rule: %v", err)
	}

	// Global scope represents each user's cross-project dev rules in ServerMode (writable by members, isolated per account).
	if _, err := st.mem.CommitMemoryFull(ctxAlice, "Never call GLOBAL_PRIVATE_TOKEN", "architecture", "", "", "global"); err != nil {
		t.Fatalf("alice commit own global rule: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctxAdmin, "Always run CGO_ENABLED=0 builds", "build", "", "", "global"); err != nil {
		t.Fatalf("admin commit own global rule: %v", err)
	}

	// Strict personal AND global isolation: neither Bob nor Admin can see Alice's personal or global memories.
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

	// Clearing Bob's local TEAM_RULES.md (empty string) clears only Bob's cache entry, leaving Alice intact.
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

	// Shared SetAuthorName must reject members and permit admin.
	if err := rp.SetAuthorName(ctxBob, "@bob"); err == nil {
		t.Fatalf("expected SetAuthorName by member to be rejected in ServerMode")
	}
	if err := rp.SetAuthorName(ctxAdmin, "@lead"); err != nil {
		t.Fatalf("expected SetAuthorName by admin to succeed: %v", err)
	}

	// Team Rule Pack precedence: in ServerMode, local_override is blocked and ignored,
	// QuerySessionMemory injects TEAM rules before project/global/personal, and VerifyPlan lists TEAM violations first.
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
	// Even if team_rule_states contains a legacy local_override row, ServerMode still enforces the Team Rule.
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

	// VerifyPlan sorts TEAM violations ahead of private global rule violations and marks global violation IsPrivate=true.
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

	// KV prefix stability across teammates: even when Alice commits her private global rule (#earlier ID)
	// BEFORE a shared project:* rule (#later ID), VerifyPlan emits [TEAM, shared project:*, private global]
	// so Alice and Bob share the exact same [TEAM + shared project:*] prompt/KV cache prefix.
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
}
