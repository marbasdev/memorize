# Security Policy

## Supported Versions

Security updates are applied to the latest release tag on `main`:

| Version | Supported |
| :--- | :--- |
| `1.x` (`main`) | Yes |
| `< 1.0.0` | No |

## Threat Model & Built-in Mitigations

`memorize` runs locally on a developer workstation by default, and also supports an authenticated multi-user Team Server Mode (`memorize serve`):

- **Loopback Binding, Origin & CSRF Protection**: The local daemon binds to `127.0.0.1` by default and validates `Host`, `Origin`, and `Sec-Fetch-Site` on mutating endpoints (`/ui/*` requires `HX-Request`; `/api/daemon/*` validates JSON POST). In Team Server Mode, `X-Forwarded-Host`, `X-Forwarded-Proto`, and `X-Forwarded-For` are honored only from trusted loopback or private-network reverse proxies.
- **Team Server Authentication & Rate Limiting**: Access tokens (`mem_*`) are stored strictly as SHA-256 hashes (shown once on creation/rotation). Browser sessions use HMAC-SHA256 signed `HttpOnly` cookies backed by a dedicated `internal_secrets` SQLite table, and `/login` enforces per-IP brute-force lockout (`429 Too Many Requests`).
- **Role-Based Access & Scope Isolation**: Team Server Mode enforces `admin` vs `member` authorization across engine/threshold/retention/user administration, isolates `personal` and `global` scopes per authenticated account, blocks moving shared `project:*` rules into private scopes, and never probes the server host filesystem for client Git repositories.
- **SQLite Storage**: State is stored in SQLite (`WAL` mode) with `0600` file permissions and `0700` data/model directories.
- **Credential Storage & Redaction**: API keys may be provided via environment variables or stored in SQLite config (never echoed raw in API responses or SSR HTML, strictly host-matched via `u.Hostname()` on dispatch, and secret-redacted in `guardrail_logs`).
- **Path, Download & Worktree Safety**: Managed GGUF operations validate safe `.gguf` filenames, reject path traversal, restrict remote model downloads to `https://` (or loopback `http://127.0.0.1|localhost`), verify `SHA256SUMS` during release binary bootstrapping, and isolate `.memorize/TEAM_RULES.md` commits inside temporary `git worktree` directories with `GIT_TERMINAL_PROMPT=0`.

## Reporting a Vulnerability

Please **do not post vulnerability details, exploit steps, or proof-of-concept payloads in a public GitHub issue**.

If you need to report a security concern, open a minimal issue asking for a private channel without disclosing technical details. Once details can be shared safely, include:
1. A clear description of the vulnerability and affected component.
2. Step-by-step reproduction instructions or a minimal proof of concept.
3. Any potential impact or suggested mitigation.

Any reported security issue is reviewed and addressed immediately once seen.
