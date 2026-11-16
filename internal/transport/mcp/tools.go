package mcp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/domain"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/modules/rulepack"
)

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), n > 0
	case int:
		return int64(n), n > 0
	case int64:
		return n, n > 0
	case string:
		p, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return p, err == nil && p > 0
	default:
		return 0, false
	}
}

func parseInt64Slice(raw any) []int64 {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(list))
	for _, item := range list {
		if id, ok := toInt64(item); ok {
			out = append(out, id)
		}
	}
	return out
}

func parseIDs(req mcp.CallToolRequest) (int64, []int64) {
	args := req.GetArguments()
	single, _ := toInt64(args["id"])
	ids := parseInt64Slice(args["ids"])
	if len(ids) == 0 && single > 0 {
		ids = []int64{single}
	}
	if single == 0 && len(ids) > 0 {
		single = ids[0]
	}
	return single, ids
}

func parseMemoryIDListArg(req mcp.CallToolRequest) ([]int64, bool) {
	raw, exists := req.GetArguments()["memory_ids"]
	if !exists {
		return nil, false
	}
	ids := parseInt64Slice(raw)
	if ids == nil {
		return []int64{}, true
	}
	return ids, true
}

func combineCategory(cat, sub string) string {
	cat = strings.TrimSpace(cat)
	sub = strings.TrimSpace(sub)
	if sub == "" || strings.Contains(cat, "/") {
		return cat
	}
	if cat == "" {
		return "general/" + sub
	}
	return cat + "/" + sub
}

func trimmedArg(req mcp.CallToolRequest, key string) string {
	return strings.TrimSpace(mcp.ParseString(req, key, ""))
}

func mcpResult(text string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(text), nil
}

func resolveMCPRequest(ctx context.Context, req mcp.CallToolRequest, resolver *ScopeResolver, prog *progress.Service, rememberRepo bool) (context.Context, string, string, string) {
	if acc := trimmedArg(req, "account"); acc != "" {
		ctx = domain.WithAccount(ctx, acc)
	}
	sid := sessionIDFrom(ctx)
	ctx = domain.WithSessionID(ctx, sid)
	sc, repoDir := resolver.Resolve(ctx, mcp.ParseString(req, "scope", ""), trimmedArg(req, "repo_path"))
	if rememberRepo && repoDir != "" && prog != nil {
		prog.RememberRepoPath(ctx, sc, repoDir)
	}
	return ctx, sid, sc, repoDir
}

// RegisterResourcesAndPrompts exposes the embedded Memory Curator playbook resource and prompt.
func RegisterResourcesAndPrompts(srv *server.MCPServer) {
	srv.AddResource(
		mcp.NewResource(
			memory.PlaybookURI,
			"Memory Curator Operating Playbook",
			mcp.WithMIMEType("text/markdown"),
			mcp.WithResourceDescription("Operating playbook for high-purity memory curation, Git HEAD baseline onboarding, incremental commit sync, and consolidation."),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      memory.PlaybookURI,
					MIMEType: "text/markdown",
					Text:     memory.PlaybookBodyMD(),
				},
			}, nil
		},
	)

	srv.AddPrompt(
		mcp.NewPrompt(
			"memory-curator",
			mcp.WithPromptDescription("Load the memorize Memory Curator operating playbook."),
		),
		func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return mcp.NewGetPromptResult(
				"Memory Curator Operating Playbook",
				[]mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(memory.PlaybookBodyMD())),
				},
			), nil
		},
	)
}

// RegisterTools binds all 5 memorize MCP tools onto srv.
func RegisterTools(srv *server.MCPServer, deps Deps, resolver *ScopeResolver) {
	scopeDesc := "Scope: 'project:<name>' (auto-detected if omitted), 'global', or 'personal'."
	accountDesc := "User account key isolating 'personal' scope (defaults to MEMORY_ACCOUNT/AGY_ACCOUNT/OS user)."

	srv.AddTool(mcp.NewTool("query_project_memory",
		mcp.WithDescription("Query active memories relevant to the current task with session delta deduplication."),
		mcp.WithString("current_task", mcp.Required(), mcp.Description("Current task or question.")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category' filter.")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
		mcp.WithString("repo_path", mcp.Description("Git repository path (auto-detected if omitted).")),
		mcp.WithString("session_id", mcp.Description("Session ID to isolate delta deduplication.")),
		mcp.WithBoolean("refresh", mcp.Description("Re-fetch active memories after compaction.")),
	), handleQueryTool(deps.Memory, deps.Progress, deps.RulePack, resolver))

	srv.AddTool(mcp.NewTool("commit_memory",
		mcp.WithDescription("Evaluate and store rules/decisions via System 1 Gatekeeper (commit, 1:N overwrite, or skip), optionally advancing synced_commit."),
		mcp.WithString("candidate_fact", mcp.Description("Single rule/preference in the user's chat language.")),
		mcp.WithArray("candidate_facts", mcp.WithStringItems(), mcp.Description("Batch list of atomic rules in the user's chat language.")),
		mcp.WithString("synced_commit", mcp.Description("Git commit SHA to record as synced.")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category'; auto-classified if omitted.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category slug.")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
		mcp.WithString("repo_path", mcp.Description("Git repository path for commit normalization.")),
	), handleCommitTool(deps.Memory, deps.Progress, resolver))

	srv.AddTool(mcp.NewTool("verify_plan_against_memory",
		mcp.WithDescription("Check whether a proposed plan violates active rules in scope."),
		mcp.WithString("proposed_plan", mcp.Required(), mcp.Description("Proposed plan or action.")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
	), handleVerifyTool(deps.Guardrail, deps.Progress, resolver))

	srv.AddTool(mcp.NewTool("manage_memory",
		mcp.WithDescription("Manage memories: 'forget', 'undo', 'consolidate' (<= 180 chars), 'update', 'purge', 'skip_onboarding', 'reset_onboarding'."),
		mcp.WithString("action", mcp.Required(), mcp.Description("'forget', 'undo', 'consolidate', 'update', 'purge', 'skip_onboarding', or 'reset_onboarding'.")),
		mcp.WithNumber("id", mcp.Description("Target memory ID.")),
		mcp.WithArray("ids", mcp.WithNumberItems(), mcp.Description("Target IDs (batch forget or 2+ consolidate).")),
		mcp.WithString("content", mcp.Description("Updated or merged text (<= 180 chars for consolidate).")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category'.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category.")),
		mcp.WithBoolean("pinned", mcp.Description("Pin as Core Rule (always included in context).")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
	), handleManageTool(deps.Memory, deps.Guardrail, deps.Progress, resolver))

	srv.AddTool(mcp.NewTool("project_progress",
		mcp.WithDescription("Manage project activity history: 'record', 'update', 'list', 'sync', or 'delete'."),
		mcp.WithString("action", mcp.Required(), mcp.Description("'record', 'update', 'list', 'sync', or 'delete'.")),
		mcp.WithNumber("id", mcp.Description("Progress entry ID (update/delete).")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("session_id", mcp.Description("Session ID (auto-detected if omitted).")),
		mcp.WithString("track", mcp.Description("Work track title.")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category'.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category.")),
		mcp.WithString("summary", mcp.Description("Summary in user's chat language (required for record).")),
		mcp.WithString("details", mcp.Description("Newline-separated details.")),
		mcp.WithString("next_step", mcp.Description("Optional follow-up note.")),
		mcp.WithArray("memory_ids", mcp.WithNumberItems(), mcp.Description("Related memory #IDs.")),
		mcp.WithString("commit_sha", mcp.Description("Related Git commit SHA.")),
		mcp.WithString("repo_path", mcp.Description("Git repository path.")),
	), handleProgressTool(deps.Progress, resolver))

	registerTeamRuleTool(srv, deps, resolver)
}

func handleQueryTool(mem *memory.Service, prog *progress.Service, rp *rulepack.Service, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, sid, sc, repoDir := resolveMCPRequest(ctx, req, resolver, prog, true)
		if explicitSID := trimmedArg(req, "session_id"); explicitSID != "" {
			sid = explicitSID
		}
		refresh := mcp.ParseBoolean(req, "refresh", false)
		rpNotice := ""
		if rp != nil && strings.HasPrefix(sc, "project:") {
			rpNotice, _ = rp.SyncTurnNotice(ctx, sc, repoDir)
		}
		syncHint := ""
		if os.Getenv("MEMORY_GIT_SYNC") == "1" || os.Getenv("MEMORY_AUTO_GIT_SYNC") == "1" {
			syncHint = mem.InspectRepoSync(ctx, sid, sc, repoDir, refresh)
		}
		out, err := mem.QuerySessionMemory(ctx,
			sid,
			mcp.ParseString(req, "current_task", ""),
			mcp.ParseString(req, "category", ""),
			sc,
			refresh,
		)
		if err != nil {
			return mcpResult("", err)
		}
		if notice := strings.TrimSpace(rpNotice); notice != "" {
			out += "\n" + notice
		}
		if syncHint != "" {
			out += "\n" + syncHint
		}
		return mcpResult(out, nil)
	}
}

func handleCommitTool(mem *memory.Service, prog *progress.Service, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, _, sc, repoDir := resolveMCPRequest(ctx, req, resolver, prog, false)
		var facts []string
		if single := trimmedArg(req, "candidate_fact"); single != "" {
			facts = append(facts, single)
		}
		facts = append(facts, req.GetStringSlice("candidate_facts", nil)...)

		cat := combineCategory(mcp.ParseString(req, "category", ""), mcp.ParseString(req, "sub_category", ""))
		out, err := mem.CommitBatch(ctx,
			facts,
			cat,
			sc,
			repoDir,
			mcp.ParseString(req, "synced_commit", ""),
		)
		return mcpResult(out, err)
	}
}

func handleVerifyTool(gr *guardrail.Service, prog *progress.Service, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, _, sc, _ := resolveMCPRequest(ctx, req, resolver, prog, false)
		out, err := gr.VerifyPlan(ctx, mcp.ParseString(req, "proposed_plan", ""), sc)
		return mcpResult(out, err)
	}
}

func handleManageTool(mem *memory.Service, gr *guardrail.Service, prog *progress.Service, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, _, sc, _ := resolveMCPRequest(ctx, req, resolver, prog, false)
		action := strings.ToLower(trimmedArg(req, "action"))
		singleID, ids := parseIDs(req)

		switch action {
		case "forget":
			if len(ids) == 0 {
				return mcp.NewToolResultError("provide id or ids to forget"), nil
			}
			n, err := mem.ForgetMemories(ctx, ids)
			return mcpResult(fmt.Sprintf("FORGOTTEN (deactivated=%d, ids=%v)", n, ids), err)
		case "undo":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to undo"), nil
			}
			msg, err := mem.UndoMemory(ctx, singleID)
			return mcpResult(msg, err)
		case "consolidate":
			cat := combineCategory(mcp.ParseString(req, "category", ""), mcp.ParseString(req, "sub_category", ""))
			newID, err := mem.ConsolidateMemories(ctx,
				sc,
				cat,
				ids,
				mcp.ParseString(req, "content", ""),
				mcp.ParseBoolean(req, "pinned", false),
			)
			return mcpResult(fmt.Sprintf("CONSOLIDATED (new_id=%d superseded %v, scope=%s)", newID, ids, sc), err)
		case "update", "set_pin":
			return handleManageUpdate(ctx, mem, req, singleID, action)
		case "purge":
			n, err := mem.PurgeTombstones(ctx, sc)
			return mcpResult(fmt.Sprintf("PURGED (deleted_tombstones=%d, scope=%s)", n, sc), err)
		case "skip_onboarding":
			return mcpResult(fmt.Sprintf("ONBOARDING_SKIPPED (scope=%s)", sc), gr.SkipOnboarding(ctx, sc))
		case "reset_onboarding":
			return mcpResult(fmt.Sprintf("ONBOARDING_RESET (scope=%s, status=pending)", sc), gr.ResetOnboarding(ctx, sc))
		default:
			return mcp.NewToolResultError("unknown action: expected forget, undo, consolidate, update, purge, skip_onboarding, or reset_onboarding"), nil
		}
	}
}

func handleManageUpdate(ctx context.Context, mem *memory.Service, req mcp.CallToolRequest, singleID int64, action string) (*mcp.CallToolResult, error) {
	if singleID <= 0 {
		return mcp.NewToolResultError("provide id to update"), nil
	}
	var subPtr *string
	if _, hasSub := req.GetArguments()["sub_category"]; hasSub {
		v := mcp.ParseString(req, "sub_category", "")
		subPtr = &v
	}
	var pinnedPtr *bool
	if _, hasPin := req.GetArguments()["pinned"]; hasPin || action == "set_pin" {
		v := mcp.ParseBoolean(req, "pinned", false)
		pinnedPtr = &v
	}
	err := mem.UpdateMemoryFull(ctx,
		singleID,
		trimmedArg(req, "scope"),
		mcp.ParseString(req, "category", ""),
		subPtr,
		mcp.ParseString(req, "content", ""),
		pinnedPtr,
	)
	if err != nil {
		return mcpResult("", err)
	}
	if _, hasAcc := req.GetArguments()["account"]; hasAcc {
		if err := mem.UpdateMemoryAccount(ctx, singleID, mcp.ParseString(req, "account", "")); err != nil {
			return mcpResult("", err)
		}
	}
	return mcpResult(fmt.Sprintf("UPDATED (id=%d)", singleID), nil)
}

func handleProgressTool(prog *progress.Service, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, _, sc, repoDir := resolveMCPRequest(ctx, req, resolver, prog, true)
		action := strings.ToLower(trimmedArg(req, "action"))
		singleID, _ := parseIDs(req)
		explicitSID := trimmedArg(req, "session_id")
		track := trimmedArg(req, "track")
		cat := trimmedArg(req, "category")
		sub := trimmedArg(req, "sub_category")
		summary := trimmedArg(req, "summary")
		details := trimmedArg(req, "details")
		nextStep := trimmedArg(req, "next_step")
		commitSHA := trimmedArg(req, "commit_sha")
		memIDs, hasMemIDs := parseMemoryIDListArg(req)

		switch action {
		case "record", "add", "create":
			rec, err := prog.RecordProgress(ctx, domain.ProgressInput{
				Scope:       sc,
				SessionID:   explicitSID,
				Track:       track,
				Category:    cat,
				SubCategory: sub,
				Summary:     summary,
				Details:     details,
				NextStep:    nextStep,
				MemoryIDs:   memIDs,
				CommitSHA:   commitSHA,
				Source:      "agent",
			})
			return mcpResult(fmt.Sprintf("PROGRESS_RECORDED (id=%d, scope=%s, track=%s)", rec.ID, rec.Scope, rec.Track), err)

		case "update":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to update progress"), nil
			}
			cur, err := prog.GetProgressByID(ctx, singleID)
			if err != nil {
				return mcpResult("", err)
			}
			nextVal := cur.NextStep
			if _, hasNext := req.GetArguments()["next_step"]; hasNext {
				nextVal = nextStep
			}
			var updMems []int64
			if hasMemIDs {
				updMems = memIDs
			}
			err = prog.UpdateProgress(ctx, singleID, domain.ProgressInput{
				Scope:       trimmedArg(req, "scope"),
				SessionID:   explicitSID,
				Track:       track,
				Category:    cat,
				SubCategory: sub,
				Summary:     summary,
				Details:     details,
				NextStep:    nextVal,
				MemoryIDs:   updMems,
				CommitSHA:   commitSHA,
			})
			return mcpResult(fmt.Sprintf("PROGRESS_UPDATED (id=%d)", singleID), err)

		case "delete", "remove":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to delete progress"), nil
			}
			return mcpResult(fmt.Sprintf("PROGRESS_DELETED (id=%d)", singleID), prog.DeleteProgress(ctx, singleID))

		case "sync":
			res, err := prog.SyncProjectProgress(ctx, sc, repoDir, "")
			return mcpResult(fmt.Sprintf("PROGRESS_SYNCED (scope=%s, head=%s, synced_memories=%d, synced_commits=%d)",
				res.Scope, res.HeadCommit, res.SyncedMemories, res.SyncedCommits), err)

		case "list", "overview":
			ov, err := prog.GetProgressOverviewByTrack(ctx, sc, track, cat, repoDir)
			if err != nil {
				return mcpResult("", err)
			}
			return mcpResult(formatProgressOverviewMD(ov), nil)

		default:
			return mcp.NewToolResultError("unknown action: expected record, update, list, sync, or delete"), nil
		}
	}
}

func formatProgressOverviewMD(ov domain.ProgressOverview) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Project Progress [%s] (total=%d | in_sync=%v | unlinked_memories=%d | pending_commits=%d)\n",
		ov.Scope, ov.TotalCount, ov.Sync.InSync, ov.Sync.UnlinkedMemoryCount, len(ov.Sync.PendingCommits))
	if len(ov.Records) > 0 {
		sb.WriteString("\n### Recent History\n")
		limit := min(len(ov.Records), 20)
		for _, r := range ov.Records[:limit] {
			fmt.Fprintf(&sb, "- [#%d] (%s | %s | [%s]) %s", r.ID, r.Date, r.Kind, r.Track, r.Summary)
			if len(r.MemoryIDs) > 0 {
				fmt.Fprintf(&sb, " (memories: %v)", r.MemoryIDs)
			}
			if r.CommitSHA != "" {
				fmt.Fprintf(&sb, " (commit: %s)", r.CommitSHA)
			}
			if r.NextStep != "" {
				fmt.Fprintf(&sb, " | Note: %s", r.NextStep)
			}
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSpace(sb.String())
}
