# Team Collaboration & Shared Rules Guide

[한국어 (TEAM.ko.md)](./TEAM.ko.md) · [Back to README](./README.md) · [Architecture Reference](./ARCHITECTURE.md)

`praxis` supports two team collaboration models—use **Git-based `.praxis/TEAM_RULES.md`** alone on local daemons, or add **Shared Team Server Mode (`praxis serve`)** so teammates share one SQLite server and inference engine.

---

## 1. Git-Based Declarative Team Rule Pack (`.praxis/TEAM_RULES.md`)

- **Auto-Sync into Memory**: On chat turns and hooks (or manually via **`Sync Remote`** / `team_rules(action="sync")`), `praxis` detects changes to **`.praxis/TEAM_RULES.md`** and injects active team rules into queries and guardrails (`PreToolUse` · `Stop` · `git pre-commit`).
- **Always Edit via Web Console or MCP `team_rules`**:

> [!IMPORTANT]
> **Do NOT edit `.praxis/TEAM_RULES.md` manually.**
> Create, edit, promote, or remove shared rules through the **Web Console** or **MCP `team_rules`** so `praxis` commits and pushes only `.praxis/TEAM_RULES.md` from an isolated temporary `git worktree` without mixing in uncommitted branch code *(returns an explicit error if the remote branch is protected)*.

- **Web Console**:
  - **Memories tab → `Promote`**: Convert a `project:*` local memory into a concise 1-line team rule.
  - **Team Rules tab**: Add, edit, delete, sync remote rules, clean duplicate local memories (`Check Duplicates`), set your `@git-handle`, and resolve conflicts.
- **MCP Tool (`team_rules`)**: `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`

---

## 2. Shared Team Server Mode (`v1.2+` · `praxis serve` & `praxis connect`)

Run one authenticated `praxis` server for your team (single binary + SQLite `WAL` concurrent read pool) and connect developer workstations as lightweight thin clients with **no local DB or GGUF model required**.

### Host Setup & User Management

```bash
# 1. Start the authenticated team server (default 0.0.0.0:18095)
#    Prints the initial bootstrap admin token ONCE on first startup.
praxis serve --addr 0.0.0.0:18095

# 2. Issue tokens for teammates (or manage accounts in Web Console → Accounts)
praxis user add alice --role member
praxis user list
praxis user rotate alice
praxis user disable alice
```

> [!TIP]
> When exposing `praxis serve` beyond a trusted LAN, terminate TLS with a reverse proxy (Caddy, Nginx, or Traefik) on loopback or a private network so session cookies (`Secure`) and Bearer tokens travel over HTTPS.

### Teammate Thin-Client Connection

Teammates connect via CLI **or** the local Web Console (**`Remote Servers`** button in the top bar):

```bash
# Global default remote server (all repositories)
praxis connect --global --url https://praxis.company.com --token prx_...

# Or bind a single repository only (other repos stay on your local daemon)
praxis connect --url https://praxis.company.com --token prx_... --repo .

# Disconnect later
praxis disconnect --global # or: praxis disconnect --repo .
```

- **Zero Host Filesystem Exposure**: Thin clients resolve local Git diffs, repository roots, and `.praxis/TEAM_RULES.md` snapshots locally and forward them per request; the team server never reads its own host filesystem for client repos.
- **Direct MCP Clients**: Clients without hooks can connect directly to `https://<host>/mcp` with `Authorization: Bearer <token>` (snippet generated in the **Accounts** modal).
- **Single-User Multi-Device & Host-as-Client**: One user can also run `praxis serve` on a primary workstation (connecting locally to `http://127.0.0.1:18095`) and connect secondary machines with the same account token to share `global`, `project:*`, and `personal` memories across devices.

### Multi-Account Switching (CLI & Web Console)

Anyone holding valid access tokens for multiple accounts (for example personal vs. work profiles) can switch active identities without breaking per-account privacy isolation or restarting IDEs:

```bash
praxis switch                # List saved account profiles from remotes.json
praxis switch alice          # Switch active account immediately (no IDE restart)
praxis switch alice --repo . # Override active account for the current repo only
praxis switch --remove bob   # Remove a saved profile from remotes.json
```

- **CLI Profiles**: Every `praxis connect` caches the verified profile in `~/.praxis/remotes.json` (`0600`). Because MCP `stdio` bridges and lifecycle hooks re-read `remotes.json` on each call, switching takes effect on the next turn.
- **Web Console Switcher**: Click the top-bar **`@account · role ▾`** badge to open the **Switch Account** modal—add another account by token, switch in 1 click, remove an individual account, or sign out of all accounts at once. Stored accounts are kept in an HMAC-SHA256 signed `HttpOnly` cookie (`praxis_accounts`, up to 10 accounts, no plaintext tokens in browser JS) and re-verified against SQLite (`active=1` + `token_hint`) on every switch.

### Local-to-Server Migration & Portable Bundles (`push` · `export` · `import`)

Move existing local Solo memories into a Team Server or exchange scoped JSON bundles (`praxis.bundle.v1`) safely without touching SQLite files directly:

```bash
# Push local DB memories to the connected Team Server (--scope or --all required)
praxis push --scope global --dry-run                 # Preview first
praxis push --scope global                           # Default exact 1:1 migration
praxis push --scope project:app --mode merge         # AI semantic dedup/overwrite merge
praxis push --all --to alice                         # Push all scopes to a saved profile

# Export & import portable JSON bundles (local DB or connected Team Server)
praxis export --scope global -o global-rules.json
praxis import global-rules.json --mode merge --dry-run
praxis import global-rules.json --mode exact --scope project:app
```

- **Web Console (`Memories` tab)**: Use **`Export Filtered`** / **`Export All`** to download a portable bundle, and **`Import / Merge...`** to upload or paste JSON with **`Preview`** (`dry_run`) before applying in **`AI Smart Merge`** (20-item chunked semantic dedup/overwrite) or **`Exact`** mode (1:1 faithful preservation of category, sub-category, tags, pinned flag, and confidence).
- **Safe Isolation**: Exports strip internal IDs and account metadata and honor Server Mode visibility; imports enforce scope RBAC (`CanModifyScope`) and security guards before writing.

### 4-Tier Rule Priority & Isolation (Server Mode)

In Team Server Mode, scopes are isolated and ordered so **team standards always take precedence** while teammates share the same static KV cache prefix:

| Priority | Scope | Visibility & Write Access |
| :--- | :--- | :--- |
| **1 (Highest)** | **Team Rule Pack** (`.praxis/TEAM_RULES.md`) | Shared team standard; `local_override` is disabled in Server Mode |
| **2** | **`project:<repo>`** | Shared across the team; moving `project:*` memories into `global`/`personal` is blocked |
| **3** | **`global`** | **Isolated per account** — each teammate's own cross-project dev rules (writable by every member) |
| **4** | **`personal`** | **Isolated per account** — private non-dev preferences/notes (opt-in overlay in projects) |

- **RBAC**: `admin` manages inference engines, thresholds, retention, and user accounts; `member` collaborates on `project:*` memories/progress and freely manages their own `global` and `personal` rules.
- **Multi-User KV Prefix Caching**: Both retrieval and guardrail evaluations sort active facts deterministically as **`TEAM` → `project:*` → `global` → `personal`**, maximizing shared KV prefix hits across concurrent teammates.

---

*Tip: Keep repository `origin` remote names consistent across teammates so `project:<repo>` scopes match automatically, and optionally run `praxis -install git-hook` for local pre-commit checks.*
