package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/memory"
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

func registerMCPResourcesAndPrompts(srv *server.MCPServer) {
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

func registerMCPTools(srv *server.MCPServer, svc *memory.Service, resolver *scopeResolver) {
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
	), handleQueryTool(svc, resolver))

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
	), handleCommitTool(svc, resolver))

	srv.AddTool(mcp.NewTool("verify_plan_against_memory",
		mcp.WithDescription("Check whether a proposed plan violates active rules in scope."),
		mcp.WithString("proposed_plan", mcp.Required(), mcp.Description("Proposed plan or action.")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
	), handleVerifyTool(svc, resolver))

	srv.AddTool(mcp.NewTool("manage_memory",
		mcp.WithDescription("Manage memories: 'forget', 'undo', 'consolidate' (<= 180 chars), 'update', 'purge', 'skip_onboarding', 'reset_onboarding'."),
		mcp.WithString("action", mcp.Required(), mcp.Description("'forget', 'undo', 'consolidate', 'update', 'purge', 'skip_onboarding', or 'reset_onboarding'.")),
		mcp.WithNumber("id", mcp.Description("Target memory ID.")),
		mcp.WithArray("ids", mcp.WithNumberItems(), mcp.Description("Target IDs (batch forget or 2+ consolidate).")),
		mcp.WithString("content", mcp.Description("Updated or merged text (<= 180 chars for consolidate).")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category'.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category.")),
		mcp.WithBoolean("pinned", mcp.Description("Pin as Core Invariant.")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("account", mcp.Description(accountDesc)),
	), handleManageTool(svc, resolver))

	srv.AddTool(mcp.NewTool("project_progress",
		mcp.WithDescription("Manage separated work-track progress: 'record', 'update', 'list', 'sync', or 'delete'."),
		mcp.WithString("action", mcp.Required(), mcp.Description("'record', 'update', 'list', 'sync', or 'delete'.")),
		mcp.WithNumber("id", mcp.Description("Progress entry ID (update/delete).")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("session_id", mcp.Description("Session ID (auto-detected if omitted).")),
		mcp.WithString("track", mcp.Description("Work track title.")),
		mcp.WithString("category", mcp.Description("Category or 'category/sub_category'.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category.")),
		mcp.WithString("status", mcp.Description("'planned' (unstarted), 'in_progress' (active), 'blocked' (halted on blocker/user), or 'done'.")),
		mcp.WithString("summary", mcp.Description("Summary in user's chat language (required for record).")),
		mcp.WithString("details", mcp.Description("Newline-separated details.")),
		mcp.WithString("next_step", mcp.Description("Next action or blocker reason.")),
		mcp.WithArray("memory_ids", mcp.WithNumberItems(), mcp.Description("Related memory #IDs.")),
		mcp.WithString("commit_sha", mcp.Description("Related Git commit SHA.")),
		mcp.WithString("repo_path", mcp.Description("Git repository path.")),
	), handleProgressTool(svc, resolver))
}

func touchMCPLiveSession(ctx context.Context, svc *memory.Service, sc, explicitSID, toolName string) {
	sid := strings.TrimSpace(explicitSID)
	if sid == "" {
		sid = memory.SessionIDFromContext(ctx)
	}
	if sid == "" || strings.HasPrefix(sid, "stdio-") || strings.HasPrefix(sid, "bridge-") {
		return
	}
	svc.TouchLiveSession(sc, sid, toolName, "")
}

func handleQueryTool(svc *memory.Service, resolver *scopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if acc := strings.TrimSpace(mcp.ParseString(req, "account", "")); acc != "" {
			ctx = memory.WithAccount(ctx, acc)
		}
		sid := strings.TrimSpace(mcp.ParseString(req, "session_id", ""))
		if sid == "" {
			sid = sessionIDFrom(ctx)
		}
		refresh := mcp.ParseBoolean(req, "refresh", false)
		sc, repoDir := resolver.resolve(ctx, mcp.ParseString(req, "scope", ""), mcp.ParseString(req, "repo_path", ""))
		if repoDir != "" {
			svc.RememberRepoPath(ctx, sc, repoDir)
		}
		touchMCPLiveSession(ctx, svc, sc, sid, "query_project_memory")
		syncHint := ""
		if os.Getenv("MEMORY_GIT_SYNC") == "1" || os.Getenv("MEMORY_AUTO_GIT_SYNC") == "1" {
			syncHint = svc.InspectRepoSync(ctx, sid, sc, repoDir, refresh)
		}
		out, err := svc.QuerySessionMemory(ctx,
			sid,
			mcp.ParseString(req, "current_task", ""),
			mcp.ParseString(req, "category", ""),
			sc,
			refresh,
		)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if syncHint != "" {
			out += "\n" + syncHint
		}
		return mcp.NewToolResultText(out), nil
	}
}

func handleCommitTool(svc *memory.Service, resolver *scopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if acc := strings.TrimSpace(mcp.ParseString(req, "account", "")); acc != "" {
			ctx = memory.WithAccount(ctx, acc)
		}
		ctx = memory.WithSessionID(ctx, sessionIDFrom(ctx))
		sc, repoDir := resolver.resolve(ctx, mcp.ParseString(req, "scope", ""), mcp.ParseString(req, "repo_path", ""))
		var facts []string
		if single := strings.TrimSpace(mcp.ParseString(req, "candidate_fact", "")); single != "" {
			facts = append(facts, single)
		}
		facts = append(facts, req.GetStringSlice("candidate_facts", nil)...)
		touchMCPLiveSession(ctx, svc, sc, "", "commit_memory")

		cat := combineCategory(mcp.ParseString(req, "category", ""), mcp.ParseString(req, "sub_category", ""))
		out, err := svc.CommitBatch(ctx,
			facts,
			cat,
			sc,
			repoDir,
			mcp.ParseString(req, "synced_commit", ""),
		)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}

func handleVerifyTool(svc *memory.Service, resolver *scopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if acc := strings.TrimSpace(mcp.ParseString(req, "account", "")); acc != "" {
			ctx = memory.WithAccount(ctx, acc)
		}
		sc, _ := resolver.resolve(ctx, mcp.ParseString(req, "scope", ""), "")
		planText := mcp.ParseString(req, "proposed_plan", "")
		touchMCPLiveSession(ctx, svc, sc, "", "verify_plan_against_memory")
		out, err := svc.VerifyPlan(ctx,
			planText,
			sc,
		)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}

func handleManageTool(svc *memory.Service, resolver *scopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if acc := strings.TrimSpace(mcp.ParseString(req, "account", "")); acc != "" {
			ctx = memory.WithAccount(ctx, acc)
		}
		ctx = memory.WithSessionID(ctx, sessionIDFrom(ctx))
		action := strings.ToLower(strings.TrimSpace(mcp.ParseString(req, "action", "")))
		singleID, ids := parseIDs(req)
		sc, _ := resolver.resolve(ctx, mcp.ParseString(req, "scope", ""), "")

		switch action {
		case "forget":
			if len(ids) == 0 {
				return mcp.NewToolResultError("provide id or ids to forget"), nil
			}
			n, err := svc.ForgetMemories(ctx, ids)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("FORGOTTEN (deactivated=%d, ids=%v)", n, ids)), nil
		case "undo":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to undo"), nil
			}
			msg, err := svc.UndoMemory(ctx, singleID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(msg), nil
		case "consolidate":
			cat := combineCategory(mcp.ParseString(req, "category", ""), mcp.ParseString(req, "sub_category", ""))
			newID, err := svc.ConsolidateMemories(ctx,
				sc,
				cat,
				ids,
				mcp.ParseString(req, "content", ""),
				mcp.ParseBoolean(req, "pinned", false),
			)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("CONSOLIDATED (new_id=%d superseded %v, scope=%s)", newID, ids, sc)), nil
		case "update", "set_pin":
			return handleManageUpdate(ctx, svc, req, singleID, action)
		case "purge":
			n, err := svc.PurgeTombstones(ctx, sc)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("PURGED (deleted_tombstones=%d, scope=%s)", n, sc)), nil
		case "skip_onboarding":
			if err := svc.SkipOnboarding(ctx, sc); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ONBOARDING_SKIPPED (scope=%s)", sc)), nil
		case "reset_onboarding":
			if err := svc.ResetOnboarding(ctx, sc); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ONBOARDING_RESET (scope=%s, status=pending)", sc)), nil
		default:
			return mcp.NewToolResultError("unknown action: expected forget, undo, consolidate, update, purge, skip_onboarding, or reset_onboarding"), nil
		}
	}
}

func handleManageUpdate(ctx context.Context, svc *memory.Service, req mcp.CallToolRequest, singleID int64, action string) (*mcp.CallToolResult, error) {
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
	explicitScope := strings.TrimSpace(mcp.ParseString(req, "scope", ""))
	err := svc.UpdateMemoryFull(ctx,
		singleID,
		explicitScope,
		mcp.ParseString(req, "category", ""),
		subPtr,
		mcp.ParseString(req, "content", ""),
		pinnedPtr,
	)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if _, hasAcc := req.GetArguments()["account"]; hasAcc {
		if err := svc.UpdateMemoryAccount(ctx, singleID, mcp.ParseString(req, "account", "")); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("UPDATED (id=%d)", singleID)), nil
}

func handleProgressTool(svc *memory.Service, resolver *scopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx = memory.WithSessionID(ctx, sessionIDFrom(ctx))
		action := strings.ToLower(strings.TrimSpace(mcp.ParseString(req, "action", "")))
		explicitRepo := strings.TrimSpace(mcp.ParseString(req, "repo_path", ""))
		sc, repoDir := resolver.resolve(ctx, mcp.ParseString(req, "scope", ""), explicitRepo)
		if repoDir != "" {
			svc.RememberRepoPath(ctx, sc, repoDir)
		}
		singleID, _ := parseIDs(req)
		explicitSID := strings.TrimSpace(mcp.ParseString(req, "session_id", ""))
		track := strings.TrimSpace(mcp.ParseString(req, "track", ""))
		cat := strings.TrimSpace(mcp.ParseString(req, "category", ""))
		sub := strings.TrimSpace(mcp.ParseString(req, "sub_category", ""))
		status := strings.TrimSpace(mcp.ParseString(req, "status", ""))
		summary := strings.TrimSpace(mcp.ParseString(req, "summary", ""))
		details := strings.TrimSpace(mcp.ParseString(req, "details", ""))
		nextStep := strings.TrimSpace(mcp.ParseString(req, "next_step", ""))
		commitSHA := strings.TrimSpace(mcp.ParseString(req, "commit_sha", ""))
		memIDs, hasMemIDs := parseMemoryIDListArg(req)

		switch action {
		case "record", "add", "create":
			rec, err := svc.RecordProgress(ctx, memory.ProgressInput{
				Scope:       sc,
				SessionID:   explicitSID,
				Track:       track,
				Category:    cat,
				SubCategory: sub,
				Status:      status,
				Summary:     summary,
				Details:     details,
				NextStep:    nextStep,
				MemoryIDs:   memIDs,
				CommitSHA:   commitSHA,
				Source:      "agent",
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("PROGRESS_RECORDED (id=%d, scope=%s, track=%s, status=%s)", rec.ID, rec.Scope, rec.Track, rec.Status)), nil

		case "update":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to update progress"), nil
			}
			cur, err := svc.GetProgressByID(ctx, singleID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			nextVal := cur.NextStep
			if _, hasNext := req.GetArguments()["next_step"]; hasNext {
				nextVal = nextStep
			}
			var updMems []int64
			if hasMemIDs {
				updMems = memIDs
			}
			explicitScope := strings.TrimSpace(mcp.ParseString(req, "scope", ""))
			if err := svc.UpdateProgress(ctx, singleID, memory.ProgressInput{
				Scope:       explicitScope,
				SessionID:   explicitSID,
				Track:       track,
				Category:    cat,
				SubCategory: sub,
				Status:      status,
				Summary:     summary,
				Details:     details,
				NextStep:    nextVal,
				MemoryIDs:   updMems,
				CommitSHA:   commitSHA,
			}); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("PROGRESS_UPDATED (id=%d)", singleID)), nil

		case "delete", "remove":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide id to delete progress"), nil
			}
			if err := svc.DeleteProgress(ctx, singleID); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("PROGRESS_DELETED (id=%d)", singleID)), nil

		case "sync":
			res, err := svc.SyncProjectProgress(ctx, sc, repoDir, "")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("PROGRESS_SYNCED (scope=%s, head=%s, synced_memories=%d, synced_commits=%d, resolved_needs_sync=%d)",
				res.Scope, res.HeadCommit, res.SyncedMemories, res.SyncedCommits, res.ResolvedSyncs)), nil

		case "list", "overview", "status":
			ov, err := svc.GetProgressOverviewByTrack(ctx, sc, track, cat, status, repoDir)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(formatProgressOverviewMD(ov)), nil

		default:
			return mcp.NewToolResultError("unknown action: expected record, update, list, sync, or delete"), nil
		}
	}
}

func formatProgressOverviewMD(ov memory.ProgressOverview) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Project Progress [%s] (total=%d | active_tracks=%d | in_sync=%v | unlinked_memories=%d | pending_commits=%d)\n",
		ov.Scope, ov.TotalCount, len(ov.ActiveTracks), ov.Sync.InSync, ov.Sync.UnlinkedMemoryCount, len(ov.Sync.PendingCommits))
	if len(ov.ActiveTracks) > 0 {
		sb.WriteString("\n### Active Work Tracks\n")
		for _, t := range ov.ActiveTracks {
			fmt.Fprintf(&sb, "- **%s** [%s] (%d/%d done): %s", t.Track, t.Status, t.DoneCount, t.TotalCount, t.LatestSummary)
			if t.LatestNextStep != "" {
				fmt.Fprintf(&sb, " -> Next: %s", t.LatestNextStep)
			}
			sb.WriteByte('\n')
		}
	}
	if len(ov.Records) > 0 {
		sb.WriteString("\n### Recent History\n")
		limit := min(len(ov.Records), 20)
		for _, r := range ov.Records[:limit] {
			fmt.Fprintf(&sb, "- [#%d] (%s | [%s] | %s) %s", r.ID, r.Date, r.Track, r.Status, r.Summary)
			if len(r.MemoryIDs) > 0 {
				fmt.Fprintf(&sb, " (memories: %v)", r.MemoryIDs)
			}
			if r.CommitSHA != "" {
				fmt.Fprintf(&sb, " (commit: %s)", r.CommitSHA)
			}
			if r.NextStep != "" {
				fmt.Fprintf(&sb, " | Next: %s", r.NextStep)
			}
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSpace(sb.String())
}
