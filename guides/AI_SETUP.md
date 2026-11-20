# AI-First Setup Guide (`praxis`)

[한국어 (AI_SETUP.ko.md)](./AI_SETUP.ko.md) · [Back to README](../README.md)

> **Runbook for AI Coding Agents (Claude Code, Antigravity, Cursor, Windsurf, etc.)**
> When a developer asks you to install and configure `praxis` for a project, execute the 5 steps below in order.

## 1-Line Prompt for Developers

```text
Please set up and configure praxis for this project following guides/AI_SETUP.md. Run praxis doctor to verify health, confirm the inference engine, and guide me through initial onboarding.
```
*(When running from outside the `praxis` repo, replace `guides/AI_SETUP.md` with `https://github.com/marbasdev/praxis/blob/main/guides/AI_SETUP.md`.)*

---

## Agent Execution Steps

### Step 1: Detect OS & Client
- **OS**: Detect `darwin`, `linux`, or `windows` (on `windows`, built-in GGUF is disabled; guide the user toward a Custom Local URL or API key).
- **Client**: Check whether running in **Antigravity** (`agy` CLI), **Claude Code** (`/plugin`), or a **generic MCP client** (Cursor, Windsurf, Claude Desktop, etc.).

### Step 2: Install & Register

- **Antigravity (`agy`)**
  ```bash
  agy plugin install ./plugins/praxis && agy plugin list
  ```
- **Claude Code**
  ```bash
  /plugin marketplace add https://github.com/marbasdev/praxis.git
  /plugin install praxis@praxis-marketplace
  ```
- **Standalone Binary · Windows · Generic MCP Clients**
  ```bash
  CGO_ENABLED=0 go build -trimpath -o bin/praxis ./cmd/praxis
  ```
  Register `praxis` in the client's MCP config (`.cursor/mcp.json`, `claude_desktop_config.json`, etc.; on macOS/Linux, prefer `./plugins/praxis/scripts/run-praxis.sh` for auto-syncing updates):
  ```json
  {
    "mcpServers": {
      "praxis": {
        "command": "/absolute/path/to/praxis/bin/praxis"
      }
    }
  }
  ```

### Step 3: Run Self-Diagnosis (`praxis doctor`)
```bash
praxis doctor --json   # Or human-readable: praxis doctor / ./bin/praxis doctor
```
Inspect checks (`binary`, `daemon`, `storage`, `git_scope`, `inference`, `remote`) and follow the remediation `hint` on any `[WARN]` or `[FAIL]`.

### Step 4: Confirm Inference Engine
- If `inference` reports `[WARN]`, inform the user that rule extraction, search, and guardrails require an active engine, and offer:
  1. Opening **`http://127.0.0.1:18095`** (**Engines** tab) for a **1-click Local GGUF download** (macOS/Linux) or Custom Local URL / API key setup
  2. Setting an environment variable (e.g., `export GEMINI_API_KEY="your-api-key"`) or connecting to a Team Server (`praxis connect`)

### Step 5: Initial Project Onboarding
1. Call `query_project_memory` and `team_rules(action="list")` to check whether `project:<repo>` already has active rules.
2. If `0` active memories exist, ask the developer whether to proceed with initial onboarding.
3. **If approved** (following `praxis://playbook/memory-curator`):
   - Inspect key manifests, architecture docs, and entrypoints (`go.mod` / `package.json`, `README.md`, config files).
   - If conflicting legacy vs. newer patterns exist in code, **ask the developer which convention wins before saving**.
   - Commit confirmed rules as atomic entries via `commit_memory(candidate_facts=[...])` and record completion via `project_progress(action="record")`.
4. **If declined**: Call `manage_memory(action="skip_onboarding")` to suppress repeated prompts.
