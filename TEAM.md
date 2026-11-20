# Team & Multi-Device Guide

[한국어 (TEAM.ko.md)](./TEAM.ko.md) · [Back to README](./README.md) · [Architecture Reference](./ARCHITECTURE.md)

Use either collaboration mode on its own or combine both:

| Mode | Best For | Infrastructure Needed |
| :--- | :--- | :--- |
| **1. Git Team Rules (`.praxis/TEAM_RULES.md`)** | Sharing team invariants alongside repo code with zero server ops | None (each developer runs a local daemon) |
| **2. Shared Team Server (`praxis serve` + `connect`)** | Sharing a central inference engine & live project memory across a team — or **syncing your own rules across multiple machines** | 1 server binary · zero local DB/models on client machines |

---

## 1. Git-Backed Team Rules (`.praxis/TEAM_RULES.md`)

Changes to `.praxis/TEAM_RULES.md` are automatically detected on turns and hooks, taking top priority across rule injection and guardrails (`PreToolUse` · `Stop` · `git pre-commit`).

> [!IMPORTANT]
> **Never hand-edit `.praxis/TEAM_RULES.md` directly.**
> Always modify shared rules via the **Web Console (`Team Rules` tab / `Promote`)** or the **MCP `team_rules`** tool so `praxis` commits and pushes inside an isolated `git worktree` without polluting your working branch.

- **Web Console**: **`Promote`** any local rule from the `Memories` tab into a concise 1-line team rule, or add, edit, `Sync Remote`, clean duplicates, and resolve conflicts in the **`Team Rules`** tab.
- **MCP Tool (`team_rules`)**: `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`

---

## 2. Shared Team Server & Multi-Device Sync (`praxis serve` · `connect`)

Run an authenticated server from the same single binary + SQLite (`WAL`), while teammates (or your secondary machines) connect as lightweight thin clients.

### Start Server & Manage Accounts
```bash
praxis serve --addr 0.0.0.0:18095        # Prints initial admin token once on first boot
praxis user add alice --role member      # Also supports list | rotate | disable (or Web Console 'Accounts' modal)
```
> **Network Exposure**: Terminate HTTPS (TLS) behind a private network or reverse proxy (Caddy · Nginx · Traefik) to protect Bearer tokens and session cookies.

### Connect Clients & Switch Accounts
```bash
# Connect to a team server (also configurable via Web Console 'Remote Servers' modal)
praxis connect --global --url https://praxis.company.com --token prx_...
praxis connect --url https://praxis.company.com --token prx_... --repo .   # Current repo only
praxis disconnect --global                                                 # Detach (or --repo .)

# Instant multi-account switching (work <-> personal, no IDE restart required)
praxis switch                            # List saved profiles
praxis switch alice                      # Switch globally (add --repo . for repo-local switch)
```
- **Zero Host Filesystem Traversal**: Git diffs and `.praxis/TEAM_RULES.md` snapshots are collected client-side; the server never inspects client disks.
- **Direct Generic MCP Access**: Hookless clients can connect straight to `https://<host>/mcp` with `Authorization: Bearer <token>`.
- **Web Console Account Switcher**: Click the top **`@account · role ▾`** badge to switch across up to 10 signed-cookie sessions (`HttpOnly`) without exposing raw tokens to browser JS.

### Local-to-Server Migration & Portable Bundles (`push` · `export` · `import`)
```bash
praxis push --scope global --dry-run                       # Preview upload
praxis push --scope project:app --mode merge               # exact (default 1:1) or merge (AI deduplication)
praxis export --scope global -o global-rules.json          # Export portable JSON bundle
praxis import global-rules.json --mode merge --dry-run     # Preview & import bundle
```
*(Also available in the Web Console `Memories` tab via **`Export`** and **`Import / Merge...`**.)*

### 4-Tier Priority & Per-Account Privacy

Team standards and project context are shared while personal conventions stay isolated per account, sorted in deterministic order to share prefix KV caches:

| Priority | Scope | Visibility & Ownership |
| :--- | :--- | :--- |
| **1 (Highest)** | **Team Rules** (`.praxis/TEAM_RULES.md`) | Shared team standard (local overrides disabled in server mode) |
| **2** | **`project:<repo>`** | Shared project rules & activity feed (matched automatically via `origin` remote) |
| **3** | **`global`** | **Per-account isolated** — each user's cross-project coding rules |
| **4** | **`personal`** | **Per-account isolated** — non-coding personal notes (optional overlay) |
