# Team Collaboration & Shared Rules Guide

[한국어 (TEAM.ko.md)](./TEAM.ko.md) · [Back to README](./README.md) · [Architecture Reference](./ARCHITECTURE.md)

`memorize` supports two team collaboration models—use **Git-based `.memorize/TEAM_RULES.md`** alone on local daemons, or add **Shared Team Server Mode (`memorize serve`)** so teammates share one SQLite server and inference engine.

---

## 1. Git-Based Declarative Team Rule Pack (`.memorize/TEAM_RULES.md`)

- **Auto-Sync into Memory**: On chat turns and hooks (or manually via **`Sync Remote`** / `team_rules(action="sync")`), `memorize` detects changes to **`.memorize/TEAM_RULES.md`** and injects active team rules into queries and guardrails (`PreToolUse` · `Stop` · `git pre-commit`).
- **Always Edit via Web Console or MCP `team_rules`**:

> [!IMPORTANT]
> **Do NOT edit `.memorize/TEAM_RULES.md` manually.**
> Create, edit, promote, or remove shared rules through the **Web Console** or **MCP `team_rules`** so `memorize` commits and pushes only `.memorize/TEAM_RULES.md` from an isolated temporary `git worktree` without mixing in uncommitted branch code *(returns an explicit error if the remote branch is protected)*.

- **Web Console**:
  - **Memories tab → `Promote`**: Convert a `project:*` local memory into a concise 1-line team rule.
  - **Team Rules tab**: Add, edit, delete, sync remote rules, clean duplicate local memories (`Check Duplicates`), set your `@git-handle`, and resolve conflicts.
- **MCP Tool (`team_rules`)**: `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`

---

## 2. Shared Team Server Mode (`v1.2+` · `memorize serve` & `memorize connect`)

Run one authenticated `memorize` server for your team (single binary + SQLite `WAL` concurrent read pool) and connect developer workstations as lightweight thin clients with **no local DB or GGUF model required**.

### Host Setup & User Management

```bash
# 1. Start the authenticated team server (default 0.0.0.0:18095)
#    Prints the initial bootstrap admin token ONCE on first startup.
memorize serve --addr 0.0.0.0:18095

# 2. Issue tokens for teammates (or manage accounts in Web Console → Accounts)
memorize user add alice --role member
memorize user list
memorize user rotate alice
memorize user disable alice
```

> [!TIP]
> When exposing `memorize serve` beyond a trusted LAN, terminate TLS with a reverse proxy (Caddy, Nginx, or Traefik) on loopback or a private network so session cookies (`Secure`) and Bearer tokens travel over HTTPS.

### Teammate Thin-Client Connection

Teammates connect via CLI **or** the local Web Console (**`Remote Servers`** button in the top bar):

```bash
# Global default remote server (all repositories)
memorize connect --url https://memorize.company.com --token mem_...

# Or bind a single repository only (other repos stay on your local daemon)
memorize connect --url https://memorize.company.com --token mem_... --repo .

# Disconnect later
memorize disconnect          # or: memorize disconnect --repo .
```

- **Zero Host Filesystem Exposure**: Thin clients resolve local Git diffs, repository roots, and `.memorize/TEAM_RULES.md` snapshots locally and forward them per request; the team server never reads its own host filesystem for client repos.
- **Direct MCP Clients**: Clients without hooks can connect directly to `https://<host>/mcp` with `Authorization: Bearer <token>` (snippet generated in the **Accounts** modal).

### 4-Tier Rule Priority & Isolation (Server Mode)

In Team Server Mode, scopes are isolated and ordered so **team standards always take precedence** while teammates share the same static KV cache prefix:

| Priority | Scope | Visibility & Write Access |
| :--- | :--- | :--- |
| **1 (Highest)** | **Team Rule Pack** (`.memorize/TEAM_RULES.md`) | Shared team standard; `local_override` is disabled in Server Mode |
| **2** | **`project:<repo>`** | Shared across the team; moving `project:*` memories into `global`/`personal` is blocked |
| **3** | **`global`** | **Isolated per account** — each teammate's own cross-project dev rules (writable by every member) |
| **4** | **`personal`** | **Isolated per account** — private non-dev preferences/notes (opt-in overlay in projects) |

- **RBAC**: `admin` manages inference engines, thresholds, retention, and user accounts; `member` collaborates on `project:*` memories/progress and freely manages their own `global` and `personal` rules.
- **Multi-User KV Prefix Caching**: Both retrieval and guardrail evaluations sort active facts deterministically as **`TEAM` → `project:*` → `global` → `personal`**, maximizing shared KV prefix hits across concurrent teammates.

---

*Tip: Keep repository `origin` remote names consistent across teammates so `project:<repo>` scopes match automatically, and optionally run `memorize -install git-hook` for local pre-commit checks.*
