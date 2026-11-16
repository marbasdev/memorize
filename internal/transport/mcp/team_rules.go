package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/domain"
	"memorize/internal/modules/rulepack"
)

func registerTeamRuleTool(srv *server.MCPServer, deps Deps, resolver *ScopeResolver) {
	scopeDesc := "Project scope ('project:<name>', auto-detected from repo if omitted)."
	accountDesc := "User account key fallback (defaults to MEMORY_ACCOUNT/AGY_ACCOUNT/OS user)."

	srv.AddTool(mcp.NewTool("team_rules",
		mcp.WithDescription("Manage declarative Git Team Rule Pack (.memorize/TEAM_RULES.md): 'list', 'sync', 'add', 'promote', 'update', 'remove', 'resolve_conflict', 'duplicates', 'clean_duplicates', 'set_config'."),
		mcp.WithString("action", mcp.Required(), mcp.Description("'list', 'sync', 'add', 'promote', 'update', 'remove', 'resolve_conflict', 'duplicates', 'clean_duplicates', or 'set_config'.")),
		mcp.WithNumber("id", mcp.Description("1-based rule/conflict index from 'list' (for update/remove/resolve_conflict), or local memory ID (for promote/clean_duplicates).")),
		mcp.WithArray("ids", mcp.WithNumberItems(), mcp.Description("Local memory IDs (for batch clean_duplicates).")),
		mcp.WithString("content", mcp.Description("Rule description (for add/update), or trimmed_content override (for clean_duplicates).")),
		mcp.WithString("decision", mcp.Description("Conflict decision ('adopt_team', 'local_override', 'mark_updated') or cleanup action ('delete', 'trim').")),
		mcp.WithString("note", mcp.Description("Optional override rationale note.")),
		mcp.WithString("category", mcp.Description("Optional category or 'category/sub_category'.")),
		mcp.WithString("sub_category", mcp.Description("Optional sub-category slug.")),
		mcp.WithString("author", mcp.Description("Git handle for attribution (e.g. '@username'; defaults to local Git identity).")),
		mcp.WithString("target_branch", mcp.Description("Target base branch (auto-detected if omitted).")),
		mcp.WithBoolean("retire_local", mcp.Description("Deactivate promoted local memory after remote push completes (default false).")),
		mcp.WithString("scope", mcp.Description(scopeDesc)),
		mcp.WithString("repo_path", mcp.Description("Git repository path (auto-detected if omitted).")),
		mcp.WithString("account", mcp.Description(accountDesc)),
	), handleTeamRuleTool(deps, resolver))
}

func handleTeamRuleTool(deps Deps, resolver *ScopeResolver) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.RulePack == nil {
			return mcp.NewToolResultError("team rule pack service is not initialized"), nil
		}
		ctx, _, sc, repoDir := resolveMCPRequest(ctx, req, resolver, deps.Progress, true)
		if !strings.HasPrefix(sc, "project:") {
			return mcp.NewToolResultError(fmt.Sprintf("team_rules requires a project:<repo> scope (resolved %q)", sc)), nil
		}

		action := strings.ToLower(trimmedArg(req, "action"))
		args := req.GetArguments()
		rp := deps.RulePack
		singleID, ids := parseIDs(req)
		cat := trimmedArg(req, "category")
		sub := trimmedArg(req, "sub_category")
		if strings.Contains(cat, "/") && sub == "" {
			cat, sub = domain.SplitCategoryAndSub(sc, cat, "")
		}

		switch action {
		case "list", "status":
			return mcpResult(formatTeamRuleListMD(ctx, rp, sc, repoDir), nil)

		case "sync", "pull", "fetch":
			st, err := rp.SyncExplicit(ctx, sc, repoDir, true)
			if err != nil {
				return mcpResult("", err)
			}
			if st.Changed && len(st.ChangedKeys) > 0 {
				if _, serr := rp.ScanConflictsForSyncedRules(ctx, sc, st.ChangedKeys); serr == nil {
					st.PendingConflicts = rp.CountPendingConflicts(ctx, sc)
				}
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "TEAM_RULES_SYNCED (scope=%s, source=%s, rules=%d, changed=%t, pending_conflicts=%d, hash=%s)",
				st.Scope, st.SyncedFrom, st.RuleCount, st.Changed, st.PendingConflicts, shortHash(st.FileHash))
			if notice := strings.TrimSpace(rp.FormatPendingConflictNotice(ctx, sc)); notice != "" {
				sb.WriteString("\n\n")
				sb.WriteString(notice)
			}
			return mcpResult(sb.String(), nil)

		case "add", "create":
			rawText := trimmedArg(req, "content")
			if rawText == "" {
				return mcp.NewToolResultError("provide content describing the team rule to draft and publish"), nil
			}
			prop, err := rp.DraftProposalFromText(ctx, sc, repoDir, rawText, cat, sub)
			if err != nil {
				return mcpResult("", err)
			}
			applyTeamRuleOverrides(&prop, sc, cat, sub, req)
			res, err := rp.PublishWithDefaults(ctx, rulepack.PublishInput{
				Scope:        sc,
				RepoDir:      repoDir,
				Proposal:     prop,
				TargetBranch: mcp.ParseString(req, "target_branch", ""),
			}, false)
			return mcpResult(formatPublishResultMD(res), err)

		case "promote", "export":
			if singleID <= 0 {
				return mcp.NewToolResultError("provide local memory id to promote"), nil
			}
			prop, _, err := rp.PromoteLocalMemory(ctx, singleID, sc, repoDir)
			if err != nil {
				return mcpResult("", err)
			}
			applyTeamRuleOverrides(&prop, sc, cat, sub, req)
			res, err := rp.PublishWithDefaults(ctx, rulepack.PublishInput{
				Scope:         sc,
				RepoDir:       repoDir,
				Proposal:      prop,
				TargetBranch:  mcp.ParseString(req, "target_branch", ""),
				LocalMemoryID: singleID,
			}, mcp.ParseBoolean(req, "retire_local", false))
			return mcpResult(formatPublishResultMD(res), err)

		case "update", "edit":
			target, err := resolveTeamRuleTarget(ctx, rp, sc, singleID, mcp.ParseString(req, "rule_key", ""), "")
			if err != nil {
				return mcpResult("", err)
			}
			rawText := trimmedArg(req, "content")
			_, hasSub := args["sub_category"]
			authorOverride := trimmedArg(req, "author")
			if rawText == "" && cat == "" && !hasSub && sub == "" && authorOverride == "" {
				return mcp.NewToolResultError("provide updated content, category, or sub_category for update"), nil
			}
			updCat := target.Category
			if cat != "" {
				updCat = domain.NormalizeCategoryForScope(sc, cat)
			}
			updSub := target.SubCategory
			if hasSub || sub != "" {
				updSub = domain.NormalizeSubCategory(sub)
			}
			updTitle := target.Title
			if rawText != "" {
				if prop, derr := rp.DraftProposalFromText(ctx, sc, repoDir, rawText, updCat, updSub); derr == nil && strings.TrimSpace(prop.Title) != "" {
					updTitle = prop.Title
					if cat == "" && strings.TrimSpace(prop.Category) != "" {
						updCat = prop.Category
					}
					if !hasSub && sub == "" && strings.TrimSpace(prop.SubCategory) != "" {
						updSub = prop.SubCategory
					}
				} else {
					updTitle = strings.Join(strings.Fields(rawText), " ")
				}
			}
			updAuthor := target.AddedBy
			if author := domain.NormalizeGitHandle(authorOverride); author != "" {
				updAuthor = author
			}
			res, err := rp.UpdateWithDefaults(ctx, rulepack.UpdateInput{
				Scope:     sc,
				RepoDir:   repoDir,
				TargetKey: target.RuleKey,
				Updated: domain.TeamRule{
					Category:    updCat,
					SubCategory: updSub,
					Title:       updTitle,
					AddedBy:     updAuthor,
				},
				TargetBranch: mcp.ParseString(req, "target_branch", ""),
			})
			return mcpResult(formatPublishResultMD(res), err)

		case "remove", "delete":
			target, err := resolveTeamRuleTarget(ctx, rp, sc, singleID, mcp.ParseString(req, "rule_key", ""), mcp.ParseString(req, "content", ""))
			if err != nil {
				return mcpResult("", err)
			}
			res, err := rp.RemoveRuleRemote(ctx, rulepack.RemoveInput{
				Scope:        sc,
				RepoDir:      repoDir,
				TargetKey:    target.RuleKey,
				TargetBranch: mcp.ParseString(req, "target_branch", ""),
			})
			return mcpResult(formatPublishResultMD(res), err)

		case "resolve_conflict", "resolve":
			conflict, err := resolveConflictTarget(ctx, rp, sc, singleID, mcp.ParseString(req, "rule_key", ""))
			if err != nil {
				return mcpResult("", err)
			}
			decision := trimmedArg(req, "decision")
			if err := rp.ResolveConflict(ctx, sc, conflict.RuleKey, decision, trimmedArg(req, "note")); err != nil {
				return mcpResult("", err)
			}
			return mcpResult(fmt.Sprintf("CONFLICT_RESOLVED (scope=%s, title=%q, decision=%s, pending=%d)",
				sc, conflict.TeamRule.Title, decision, rp.CountPendingConflicts(ctx, sc)), nil)

		case "duplicates", "scan_duplicates":
			sugs, src, err := rp.DetectRedundantLocalMemories(ctx, sc)
			if err != nil {
				return mcpResult("", err)
			}
			return mcpResult(formatDuplicateSuggestionsMD(sc, src, sugs), nil)

		case "clean_duplicates", "cleanup_duplicates":
			return handleCleanDuplicatesAction(ctx, rp, sc, singleID, ids, req)

		case "set_config", "config":
			var changes []string
			if _, hasAuthor := args["author"]; hasAuthor {
				if err := rp.SetAuthorName(ctx, trimmedArg(req, "author")); err != nil {
					return mcpResult("", err)
				}
				changes = append(changes, "author="+rp.AuthorName(ctx, "", repoDir))
			}
			if len(changes) == 0 {
				return mcp.NewToolResultError("provide author for set_config"), nil
			}
			return mcpResult(fmt.Sprintf("TEAM_RULE_CONFIG_UPDATED (scope=%s, %s)", sc, strings.Join(changes, ", ")), nil)

		default:
			return mcp.NewToolResultError("unknown action: expected list, sync, add, promote, update, remove, resolve_conflict, duplicates, clean_duplicates, or set_config"), nil
		}
	}
}

func resolveTeamRuleTarget(ctx context.Context, rp *rulepack.Service, sc string, singleID int64, explicitKey, contentHint string) (domain.TeamRule, error) {
	rules, err := rp.ListTeamRules(ctx, sc)
	if err != nil {
		return domain.TeamRule{}, err
	}
	if len(rules) == 0 {
		return domain.TeamRule{}, fmt.Errorf("no team rules mirrored for %s", sc)
	}
	if rk := domain.NormalizeRuleKey(explicitKey); rk != "" {
		for _, r := range rules {
			if r.RuleKey == rk {
				return r, nil
			}
		}
		return domain.TeamRule{}, fmt.Errorf("team rule not found for rule_key %q", explicitKey)
	}
	if singleID >= 1 && int(singleID) <= len(rules) {
		return rules[singleID-1], nil
	}
	if needle := strings.ToLower(strings.TrimSpace(contentHint)); needle != "" {
		var matches []domain.TeamRule
		for _, r := range rules {
			if strings.Contains(strings.ToLower(r.Title), needle) {
				matches = append(matches, r)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return domain.TeamRule{}, fmt.Errorf("multiple team rules match %q; pass 1-based id from team_rules(action='list')", contentHint)
		}
	}
	return domain.TeamRule{}, fmt.Errorf("provide 1-based rule id (1..%d) from team_rules(action='list')", len(rules))
}

func resolveConflictTarget(ctx context.Context, rp *rulepack.Service, sc string, singleID int64, explicitKey string) (domain.RulePackConflict, error) {
	conflicts, err := rp.ListConflicts(ctx, sc, false)
	if err != nil {
		return domain.RulePackConflict{}, err
	}
	if rk := domain.NormalizeRuleKey(explicitKey); rk != "" {
		for _, c := range conflicts {
			if c.RuleKey == rk {
				return c, nil
			}
		}
		return domain.RulePackConflict{RuleKey: rk, Scope: sc}, nil
	}
	if len(conflicts) == 0 {
		return domain.RulePackConflict{}, fmt.Errorf("no pending conflicts for %s", sc)
	}
	if singleID >= 1 && int(singleID) <= len(conflicts) {
		return conflicts[singleID-1], nil
	}
	return domain.RulePackConflict{}, fmt.Errorf("provide 1-based conflict id (1..%d) from team_rules(action='list')", len(conflicts))
}

func applyTeamRuleOverrides(prop *domain.RulePlacementProposal, scope, cat, sub string, req mcp.CallToolRequest) {
	if cat != "" {
		prop.Category = domain.NormalizeCategoryForScope(scope, cat)
	}
	if sub != "" {
		prop.SubCategory = domain.NormalizeSubCategory(sub)
	}
	if author := domain.NormalizeGitHandle(mcp.ParseString(req, "author", "")); author != "" {
		prop.AddedBy = author
	}
	prop.RuleKey = domain.ComputeRuleKey(prop.Category, prop.Title)
	prop.Pinned = true
}

func handleCleanDuplicatesAction(ctx context.Context, rp *rulepack.Service, sc string, singleID int64, ids []int64, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	decision := strings.ToLower(trimmedArg(req, "decision"))
	overrideTrim := trimmedArg(req, "content")

	var actions []rulepack.DuplicateCleanupAction
	if singleID > 0 && (decision == "delete" || decision == "forget" || decision == "trim" || decision == "reduce") {
		actKind := "delete"
		if decision == "trim" || decision == "reduce" {
			actKind = "trim"
		}
		actions = append(actions, rulepack.DuplicateCleanupAction{
			LocalMemoryID:  singleID,
			Action:         actKind,
			TrimmedContent: overrideTrim,
		})
	} else {
		if len(ids) == 0 {
			return mcp.NewToolResultError("provide id/ids (or decision='delete'/'trim' with id) to clean duplicates"), nil
		}
		sugs, _, err := rp.DetectRedundantLocalMemories(ctx, sc)
		if err != nil {
			return mcpResult("", err)
		}
		byID := make(map[int64]domain.RuleDuplicateSuggestion, len(sugs))
		for _, s := range sugs {
			byID[s.LocalMemoryID] = s
		}
		for _, id := range ids {
			sug, ok := byID[id]
			if !ok {
				continue
			}
			act := rulepack.DuplicateCleanupAction{LocalMemoryID: id, Action: "delete"}
			if sug.Kind == "partial_overlap" {
				act.Action = "trim"
				act.TrimmedContent = sug.TrimmedContent
				if overrideTrim != "" && id == singleID {
					act.TrimmedContent = overrideTrim
				}
			}
			actions = append(actions, act)
		}
	}
	if len(actions) == 0 {
		return mcpResult("NO_MATCHING_DUPLICATES (0 local memories modified)", nil)
	}
	res, err := rp.ApplyDuplicateCleanup(ctx, sc, actions)
	return mcpResult(fmt.Sprintf("DUPLICATES_CLEANED (scope=%s, deleted=%v, trimmed=%v)", sc, res.DeletedIDs, res.TrimmedIDs), err)
}

func formatTeamRuleListMD(ctx context.Context, rp *rulepack.Service, sc, repoDir string) string {
	rules, err := rp.ListTeamRules(ctx, sc)
	if err != nil {
		return "error: " + err.Error()
	}
	conflicts, _ := rp.ListConflicts(ctx, sc, false)
	author := rp.AuthorName(ctx, "", repoDir)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Team Rule Pack [%s] (rules=%d | author=%s | pending_conflicts=%d)\n",
		sc, len(rules), author, len(conflicts))
	if len(conflicts) > 0 {
		sb.WriteByte('\n')
		sb.WriteString(rp.FormatPendingConflictNotice(ctx, sc))
	}
	if len(rules) == 0 {
		sb.WriteString("\nNo Team Rules mirrored yet. Use `team_rules(action='add'|'promote'|'sync')`.")
		return sb.String()
	}
	for i, r := range rules {
		catLabel := r.Category
		if r.SubCategory != "" {
			catLabel += "/" + r.SubCategory
		}
		statusBadge := ""
		if r.Resolution != "" && r.Resolution != domain.RuleStateAdopted {
			statusBadge = " [" + r.Resolution + "]"
		}
		fmt.Fprintf(&sb, "%d. (%s)%s %s · `%s` · `%s`\n",
			i+1, catLabel, statusBadge, r.Title, r.AddedBy, r.AddedAt)
	}
	return strings.TrimSpace(sb.String())
}

func formatPublishResultMD(res rulepack.PublishResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "TEAM_RULE_%s (title=%q, attempts=%d",
		strings.ToUpper(res.Status), res.Rule.Title, res.Attempts)
	if res.TargetBranch != "" {
		fmt.Fprintf(&sb, ", target_branch=%s", res.TargetBranch)
	}
	if res.BranchPushed != "" {
		fmt.Fprintf(&sb, ", branch_pushed=%s", res.BranchPushed)
	}
	if res.CommitSHA != "" {
		fmt.Fprintf(&sb, ", commit=%s", res.CommitSHA)
	}
	if res.ConflictReason != "" {
		fmt.Fprintf(&sb, ", conflict=%q", res.ConflictReason)
	}
	sb.WriteByte(')')
	if res.Status == "conflict_pending" {
		sb.WriteString("\nPublish paused because latest remote `.memorize/TEAM_RULES.md` contains a conflicting rule. Discuss with user and adjust before retrying.")
	}
	return sb.String()
}

func formatDuplicateSuggestionsMD(sc, src string, sugs []domain.RuleDuplicateSuggestion) string {
	if len(sugs) == 0 {
		return fmt.Sprintf("NO_REDUNDANT_LOCAL_MEMORIES (scope=%s)", sc)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Local Memories Overlapping Team Rules [%s] (count=%d | engine=%s)\n", sc, len(sugs), src)
	for _, s := range sugs {
		fmt.Fprintf(&sb, "- Local `#%d` (%s) <-> Team (%q) [%s]: %s\n",
			s.LocalMemoryID, s.LocalCategory, s.TeamRuleTitle, s.Kind, s.Reason)
		if s.Kind == "partial_overlap" && s.TrimmedContent != "" {
			fmt.Fprintf(&sb, "  Suggested trimmed local content: %s\n", s.TrimmedContent)
		}
	}
	return strings.TrimSpace(sb.String())
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
