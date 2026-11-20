---
name: memory-curator
description: >-
  Curation playbook for praxis. Activate when running cold-start project
  onboarding, manual Git Baseline Onboarding / Incremental Git Sync, or
  consolidating fragmented memories.
---

# Memory Curator Playbook

## 1. Golden Filter: Commit vs. Drop
- **Commit Before Mutating (`commit_memory` / `manage_memory`):** Decompose user decisions, resolved contextual approvals, and architectural choices into atomic behavioral items (`1` behavior = `1` item in `candidate_facts`) in the user's current chat language. Update existing `#ID` rules directly via `manage_memory(action="update", id=<ID>, content="...")`.
- **Evolve Stale Rules Explicitly:** Never silently bypass or self-revoke active rules, and never force a worse design just to satisfy an outdated rule; explain why `#ID` (or a Team Rule) limits a better approach, get explicit user approval, and update/forget it first.
- **Never Commit:** Code-obvious structure, unverified WIP experiments, raw debug logs, or micromanaged one-off UI/doc layout details that would ossify future redesigns.

## 2. Cold-Start Project Onboarding (`[ONBOARDING AVAILABLE]`)
- **Ask First:** When `[ONBOARDING AVAILABLE]` appears, ask the user whether to onboard initial rules; if declined, call `manage_memory(action="skip_onboarding")`.
- **Resolve Conflicts Before Saving:** Inspect key repo files directly. If conflicting or rule-violating implementations coexist (or conflict with active `global` rules), ask the user which pattern takes priority instead of silently picking one.
- **Commit & Remediate:** Save user-confirmed priority rules via `commit_memory` (noting superseded legacy patterns in the rule text so agents never imitate them), then offer or proceed to refactor the violating files.

## 3. Manual Git Onboarding & Incremental Sync
- **`[Git Baseline Onboarding — HEAD <sha>]`:** Inspect only `HEAD` manifests/rule docs (never pre-baseline history) and call `commit_memory(scope="project:<repo>", candidate_facts=[...], synced_commit="<sha>")` once.
- **`[Incremental Git Sync — <old>..<new>]`:** Review listed commits chronologically; commit any rule/dependency changes with `synced_commit="<new>"`, or pass `synced_commit="<new>"` alone if no rules changed.

## 4. Consolidation
- On demand, group semantically similar or overlapping memories in the same scope/category and merge 2+ items via `manage_memory(action="consolidate", ids=[...], content="...")` only when the merged rule stays concise (`<= 180` characters); keep items separate if merging would exceed `180` chars or bundle unrelated behaviors.

## 5. Project Activity History
- Optionally call `project_progress(action="record", track="...", summary="...")` after completing notable work milestones or non-commit tasks to record them in the chronological project activity timeline alongside automatic memory and Git commit sync entries.

## 6. Team Rule Pack & Team Server Mode
- Use `team_rules` (`list`, `sync`, `add`, `promote`, `update`, `remove`, `resolve_conflict`, `duplicates`, `clean_duplicates`, `set_config`) for shared `.praxis/TEAM_RULES.md` rules; NEVER hand-edit `.praxis/TEAM_RULES.md` or mix uncommitted feature code into Team Rule commits (`praxis` commits and pushes strictly via an isolated `git worktree`).
- On `[TEAM RULE CONFLICT DETECTED]` or `conflict_pending`, pause and ask the user to adopt (`adopt_team`) or keep their local override (`local_override`).
- In authenticated Team Server Mode (`praxis serve`), Team Rules always take top priority (`local_override` and server-host Git mutations are disabled), `project:*` rules are shared across teammates, `global` is isolated per user for cross-project dev rules, and `personal` is isolated per user for non-dev notes.
