# Team Collaboration & Shared Rules Guide

[한국어 (TEAM.ko.md)](./TEAM.ko.md) · [Back to README](./README.md) · [Architecture Reference](./ARCHITECTURE.md)

Share agreed team rules across repositories through **`.memorize/TEAM_RULES.md`** in Git, while keeping every developer's local daemon and personal memories separate.

---

## 1. How It Works

- **Personal & WIP Stay Local**: Your local daemon (`127.0.0.1:18095`) and `~/.memorize/memories.db` stay private on your machine so half-finished branch context and personal preferences never leak to teammates.
- **Auto-Sync into Memory**: On chat turns and hooks (or manually via **`Sync Remote`** / `team_rules(action="sync")`), `memorize` detects changes to **`.memorize/TEAM_RULES.md`**, loads active team rules into queries and guardrails (`PreToolUse` · `Stop` · `git pre-commit`), and pauses for your choice (`adopt_team` · `local_override` · `mark_updated`) if a shared rule conflicts with a local memory.

---

## 2. Important: Always Use the Dashboard or MCP `team_rules`

> [!IMPORTANT]
> **Do NOT edit `.memorize/TEAM_RULES.md` manually.**
> Always create, edit, promote, or remove shared rules through the **Web Console** or **MCP `team_rules`** so `memorize` can commit and push cleanly in an isolated Git worktree without mixing in your uncommitted branch changes.

- **Web Console (`http://127.0.0.1:18095`)**:
  - **Memories tab → `Promote`**: Convert a `project:*` local memory into a concise 1-line team rule (with optional local cleanup).
  - **Team Rules tab**: Add, edit, delete, sync remote rules, clean duplicate local memories (`Check Duplicates`), set your `@git-handle`, and resolve conflicts.
- **MCP Tool (`team_rules`)**:
  - `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`
- **Push Behavior**: Mutations commit only `.memorize/TEAM_RULES.md` from a temporary `git worktree` and push directly to the target branch with remote-change retry protection *(returns an explicit error if the remote branch is protected)*.

---

*Tip: Keep repository folder names consistent across teammates so `project:<repo>` scopes match, and optionally run `memorize -install git-hook` for local pre-commit checks. Cross-device sync for one user's own machines remains gated off in `v1.1` as a later roadmap item.*
