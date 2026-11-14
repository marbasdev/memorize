---
name: memory-curator
description: >-
  Curation playbook for memorize. Activate when running cold-start project
  onboarding, manual Git Baseline Onboarding / Incremental Git Sync, or
  consolidating fragmented memories.
---

# Memory Curator Playbook

## 1. Golden Filter: Commit vs. Drop
- **Commit Before Mutating (`commit_memory` / `manage_memory`):** Decompose user decisions, resolved contextual approvals, and architectural choices into atomic behavioral items (`1` behavior = `1` item in `candidate_facts`) in the user's current chat language. Update existing `#ID` rules directly via `manage_memory(action="update", id=<ID>, content="...")`.
- **Never Commit:** Code-obvious structure, unverified WIP experiments, or raw debug logs.

## 2. Cold-Start Project Onboarding (`[ONBOARDING AVAILABLE]`)
- **Ask First:** When `[ONBOARDING AVAILABLE]` appears, ask the user whether to onboard initial rules; if declined, call `manage_memory(action="skip_onboarding")`.
- **Resolve Conflicts Before Saving:** Inspect key repo files directly. If conflicting or rule-violating implementations coexist (or conflict with active `global` rules), ask the user which pattern takes priority instead of silently picking one.
- **Commit & Remediate:** Save user-confirmed priority rules via `commit_memory` (noting superseded legacy patterns in the rule text so agents never imitate them), then offer or proceed to refactor the violating files.

## 3. Manual Git Onboarding & Incremental Sync
- **`[Git Baseline Onboarding — HEAD <sha>]`:** Inspect only `HEAD` manifests/rule docs (never pre-baseline history) and call `commit_memory(scope="project:<repo>", candidate_facts=[...], synced_commit="<sha>")` once.
- **`[Incremental Git Sync — <old>..<new>]`:** Review listed commits chronologically; commit any rule/dependency changes with `synced_commit="<new>"`, or pass `synced_commit="<new>"` alone if no rules changed.

## 4. Consolidation
- On demand, group semantically similar or overlapping memories in the same scope/category and merge 2+ items via `manage_memory(action="consolidate", ids=[...], content="...")` only when the merged rule stays concise (`<= 180` characters); keep items separate if merging would exceed `180` chars or bundle unrelated behaviors.

## 5. Progress Tracking
- Keep `project_progress` accurate across all 4 lifecycle states:
  - `planned`: agreed or backlog work that has not started yet (set `next_step` to the planned first action).
  - `in_progress`: work actively executing in the current session.
  - `blocked`: work halted waiting on user input, credentials, approvals, or an external blocker (record the exact blocker in `next_step`).
  - `done`: verified and completed work.
