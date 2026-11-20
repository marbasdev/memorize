# Architecture & Technical Reference

[한국어 (ARCHITECTURE.ko.md)](./ARCHITECTURE.ko.md) · [Back to README](./README.md) · [Team Guide](./TEAM.md)

## 1. Execution Pipeline & Core Mechanisms

```mermaid
flowchart LR
    A["MCP Stdio / HTTP (/mcp) / Lifecycle Hooks"] --> B["Unified Local Daemon (:18095) or Team Server (praxis serve)"]
    B --> C["Write Gate (noise drop · atomic 1:N overwrite)"]
    B --> D["Parallel Relevance Eval (Prefix KV Cache · System 1)"]
    D --> E["Session Delta Injection & [REVOKED] Broadcast"]
    D --> F["Hybrid Guardrail (PreToolUse · Stop Auto-Fix · pre-commit)"]
```

| Subsystem | How It Works | Key State & Endpoints |
| :--- | :--- | :--- |
| **Unified Daemon & Team Server** | Auto-spawns on first MCP/hook call locally; `praxis serve` runs an authenticated multi-user server with concurrent SQLite `WAL` reads and SSE domain push. | `/mcp`, `/healthz`, `/api/daemon/{status,mcp,hook,export,import}`, `/api/events` |
| **Write Gate & Bundles** | Drops transient chatter, atomically retires conflicting rules (`1:N`), and exports/imports portable JSON bundles (`praxis.bundle.v1`) with AI merge. | `memories` (`pinned`, lineage `superseded_by`) |
| **Session Delta & KV Prefix** | Skips already-delivered rules (`0` eval on repeat turns) and evaluates unsent rules in `TEAM > project:* > global > personal` order to maximize KV cache hits. | `session_sent`, `session_updates`, `session_compacts` (bounded at `256` sessions) |
| **Per-Task Model Routing** | Routes fast judgment (`relevance`, `mutation`, `verify`, `calibrate`) to `0`-output-token engines and synthesis (`consolidate`, `progress`, `taxonomy`) to companion models. | 7 `EvalTask` routes, `eval_telemetry` metrics, `429`/`503` fallback |
| **Auth & 4-Tier Isolation** | Authenticates via SHA-256 tokens (`prx_*`) and HMAC-SHA256 `HttpOnly` session/switcher cookies; shares team/project rules while isolating `global` & `personal` per account. | `users`, `internal_secrets`, in-memory `.praxis/TEAM_RULES.md` cache |

### Hybrid Guardrail Flow (`PreToolUse` · `Stop` · `git pre-commit`)
1. **Read-Only Bypass & Pre-Mutate Gate (`PreToolUse`)**: Read-only tools pass with `0` inference calls; if the current prompt introduced an unsaved rule change, mutations pause until memory is updated first.
2. **Upfront Shell Block (`PreToolUse`)**: Irreversible shell commands and explicit plan checks are verified immediately against active local and team rules before running.
3. **Zero-Stall Edit Snapshots (`PreToolUse`)**: Interactive file edits proceed without per-edit inference latency, snapshotting each touched file's baseline once in `turn_pending_edits`.
4. **Turn-End Auto-Fix (`Stop`) & Commit Gate**: When the turn ends, cumulative diffs are verified once per scope so the agent self-corrects violations before finishing, backed by `git-precommit` at commit time.

---

## 2. Package Layering (Strict Acyclic DAG)

**Dependency Direction**: `cmd/praxis → app → transport → modules → inference → infra → domain`  
*(Upward imports, raw SQL inside transport packages, and `git` execution outside `internal/infra/git` are blocked by `TestArchitectureBoundaries` AST checks.)*

| Layer | Packages | Responsibility & Boundary |
| :--- | :--- | :--- |
| **1. Domain** | `internal/domain` | Stdlib-only domain models, normalization, and secret redaction (`Fact`, `Scope`, `RulePack`, `MemoryBundle`) |
| **2. Infra** | `internal/infra/{config,sqlite,git,eventbus}` | Config files (`ports.json`, `remotes.json`), SQLite `WAL` & auth store, isolated `git worktree` ops, SSE event bus |
| **3. Inference** | `internal/inference/{router,providers,discovery,localmodel}` | Per-task router & cooldown fallback, `GGUF` · `Gemini` · `System 1` · OpenAI-compatible adapters, `llama-server` lifecycle |
| **4. Modules** | `internal/modules/{memory,progress,rulepack,guardrail}` | Rule lifecycle/consolidation/bundles, activity feed (`project_progress`), `.praxis/TEAM_RULES.md` sync, guardrails & onboarding |
| **5. Transport** | `internal/transport/{cli,mcp,http,dashboard}` | CLI commands, MCP protocol bridge, loopback CSRF & proxy-guarded HTTP mux, 6-tab HTMX + Alpine SSR Web Console |
| **6. App & Entry** | `cmd/praxis`, `internal/app/localdaemon` | Single-binary entrypoint, auto-daemon & team server lifecycle, graceful shutdown, port fallback, binary handover |

---

## 3. CLI & MCP Reference

### CLI Commands
| Command | Purpose |
| :--- | :--- |
| `praxis doctor [--json]` | End-to-end health check across binary, daemon, SQLite `WAL`, Git scope, inference engine, and remote server |
| `praxis daemon <subcmd>` | Manage local daemon: `status` · `start` · `stop` · `restart` · `logs` · `port [<daemon> <gguf> \| reset]` |
| `praxis serve [--addr host:port]` | Run authenticated multi-user Team Server (prints initial `admin` token once on first boot) |
| `praxis user <add\|list\|rotate\|disable>` | Create, list, rotate, or disable Team Server accounts (`--role admin\|member`) |
| `praxis connect` / `disconnect` / `switch` | Attach to a Team Server (`--global` or `--repo .`), detach, or switch saved account profiles live |
| `praxis push` / `export` / `import` | Migrate local rules to a server or export/import portable JSON bundles (`--scope`, `--all`, `--mode exact\|merge`, `--dry-run`) |
| `praxis -install git-hook` | Install the `.git/hooks/pre-commit` guardrail hook in the current repository |

### MCP Tools & Resources
| Name | Purpose |
| :--- | :--- |
| `query_project_memory` | Evaluate active rules (`TEAM → project:<repo> → global → personal`) and return unsent session deltas |
| `commit_memory` | Evaluate single/batch candidate rules (`INSERT`, `1:N` overwrite, or skip) and optionally advance `synced_commit` |
| `verify_plan_against_memory` | Check a proposed command or implementation plan against active local and team rules before execution |
| `manage_memory` | `update` · `forget` · `undo` · `consolidate` (`<= 180` chars) · `purge` · `skip_onboarding` · `reset_onboarding` |
| `project_progress` | Manage the chronological project activity feed (`record` · `update` · `list` · `sync` · `delete`) |
| `team_rules` | Manage `.praxis/TEAM_RULES.md` (`list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`) |
| `praxis://playbook/memory-curator` | Built-in onboarding and curation playbook resource (paired with the `memory-curator` prompt) |

---

## 4. Environment Variables & CI/CD

| Variable | Default | Description |
| :--- | :--- | :--- |
| `PRAXIS_DATA_DIR` / `PRAXIS_DB_PATH` | `~/.praxis` · `memories.db` | Runtime data directory (`%USERPROFILE%\.praxis` on Windows) and SQLite path |
| `PRAXIS_MODELS_DIR` | `~/.praxis/models` | Managed local GGUF models and `llama-server` directory (`macOS`/`Linux`) |
| `PRAXIS_PROVIDER` | `gguf` *(Unix)* / `local` *(Win)* | Active inference provider (`gguf` · `gemini` · `jev`/`flide` · `local`) |
| `GEMINI_API_KEY` / `GEMINI_MODEL` | *(empty)* / `gemini-3.5-flash-lite` | Gemini API key and default model (`gemini-decision-1` uses `/v1beta/decisions`) |
| `LOCAL_MODEL_URL` / `LOCAL_MODEL_NAME` | `http://127.0.0.1:11434/v1` / *(auto)* | OpenAI-compatible local server (`Ollama`, `LM Studio`, `vLLM`) |
| `JEV_API_URL` / `JEV_MODEL` | `http://127.0.0.1:8000` / `jev-latest` | System 1 judgment endpoint and model (`TYPESAFE_API_KEY` optional) |
| `PRAXIS_REMOTE_URL` / `PRAXIS_REMOTE_TOKEN` | *(empty)* | Explicit Team Server URL and Bearer token override (supersedes `remotes.json`) |
| `PRAXIS_DAEMON_ADDR` / `PRAXIS_HOOK_TIMEOUT` | `:18095` / *(manifest)* | Explicit daemon bind address and hook timeout (`8s`, disable with `0`/`off`) |
| `PRAXIS_INCLUDE_PERSONAL` / `PRAXIS_ACCOUNT` | `0` / *(OS user)* | Overlay `personal` memories in projects; set account key (`AGY_ACCOUNT` alias) |
| `PRAXIS_GIT_SYNC` / `PRAXIS_GUARDRAIL_FAIL_CLOSED` | `0` / `0` | Enable incremental Git commit rule sync / fail closed (`1`) when engine is unavailable |

- **CI/CD**: [ci.yml](./.github/workflows/ci.yml) · [release.yml](./.github/workflows/release.yml) · [.gitlab-ci.yml](./.gitlab-ci.yml) verify `gofmt`, `staticcheck`, `deadcode`, 5-target `CGO_ENABLED=0` builds (`darwin/arm64|amd64`, `linux/arm64|amd64`, `windows/amd64`), and `-race -count=1` tests on Go `1.26`, publishing `SHA256SUMS` archives on `v*.*.*` tags.
