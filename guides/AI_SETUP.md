# AI-First Setup Guide (`praxis`)

> **For AI Coding Agents (Claude Code, Antigravity, Cursor, Windsurf, etc.)**
> If a developer asks you to "install and set up praxis for this project", follow the autonomous steps in this guide.

---

## Quick One-Line Setup Prompts for Developers

Developers can copy and paste one of the following prompts into their agent chat:

```text
Please set up and configure praxis for this project following guides/AI_SETUP.md. Run praxis doctor to verify health, confirm the inference engine, and guide me through initial onboarding.
```

*(Or point your agent directly to this file: `https://github.com/marbasdev/praxis/blob/main/guides/AI_SETUP.md`)*

---

## Agent Autonomous Setup Playbook

Execute the following 5 phases in order:

### Phase 1: Environment & Client Detection

1. **Operating System**: Detect `darwin`, `linux`, or `windows`.
   - On Windows, built-in GGUF is disabled; instruct the user to configure a Custom Local URL (Ollama, LM Studio) or Gemini API key.
2. **Agent Platform**:
   - **Antigravity**: Check if `agy` CLI is available.
   - **Claude Code**: Check if running within Claude Code environment.
   - **Other MCP Client**: Cursor, Windsurf, or Claude Desktop.

---

### Phase 2: Installation

#### Option A: Antigravity (`agy`)
Run the following terminal commands:
```bash
# If not yet cloned or installing from local repository:
agy plugin install ./plugins/praxis
```
Verify plugin installation:
```bash
agy plugin list
```

#### Option B: Claude Code
Execute plugin marketplace commands in the Claude session:
```bash
/plugin marketplace add https://github.com/marbasdev/praxis.git
/plugin install praxis@praxis-marketplace
```

#### Option C: Standalone Binary / Windows / Generic MCP
Build pure Go static binary without CGO:
```bash
CGO_ENABLED=0 go build -trimpath -o bin/praxis ./cmd/praxis
```
Configure your client's MCP configuration (`claude_desktop_config.json`, `.cursor/mcp.json`, etc.):
```json
{
  "mcpServers": {
    "praxis": {
      "command": "/path/to/praxis/bin/praxis"
    }
  }
}
```
*(Or use `./plugins/praxis/scripts/run-praxis.sh` on macOS/Linux for auto-updating wrapper).*

---

### Phase 3: Self-Diagnosis (`praxis doctor`)

Run the self-diagnostic command:
```bash
praxis doctor
# or
./bin/praxis doctor
```
For machine-readable JSON output:
```bash
praxis doctor --json
```

**Diagnostic Verification Checklist**:
1. `Binary & Plugin`: Verify that the running binary version matches the plugin manifest (`v1.2.0`).
2. `Daemon Process`: Check whether daemon is running or ready to auto-spawn on `http://127.0.0.1:18095`.
3. `Storage`: Confirm SQLite WAL mode is enabled and `busy_timeout` is at least 10000ms.
4. `Git & Scope`: Confirm project canonical root and scope `project:<repo>`.
5. `Inference Engine`: Check if an active inference provider (Local GGUF, Gemini, Custom Local URL, or JEV) is configured.
6. `Team Remote`: Check if a remote team server is connected (optional for solo mode).

If any check fails or warns, follow the remediation hint printed by `praxis doctor`.

---

### Phase 4: Inference Engine Verification

Check if an inference model is configured:
- If `praxis doctor` reports `[WARN] Inference Engine`:
  - Inform the user that core memory operations require an active inference engine.
  - Advise them to open the Web Console at **`http://127.0.0.1:18095`** (Engines tab) for a **1-click Local GGUF download** (on macOS/Linux) or to enter an API key / custom local endpoint.
  - Alternatively, the user can supply a Gemini API key via environment variable:
    ```bash
    export GEMINI_API_KEY="your-api-key"
    ```

---

### Phase 5: Project Onboarding & Rules Initialization

1. Inspect project rules by querying `team_rules` or `query_project_memory`.
2. Check if active project memories exist:
   - If `0` memories exist for the current repository, ask the developer:
     > *"No memory records found for this repository. Would you like me to inspect key codebase files and onboard project architecture rules into praxis?"*
3. If the developer approves:
   - Inspect key files (`README.md`, `go.mod` / `package.json`, architecture docs, config files).
   - If conflicting legacy vs new patterns are detected, ask the developer which pattern takes priority *before* storing rules.
   - Commit the extracted rules using `commit_memory` or `manage_memory(action="bootstrap_rules")`.
   - Record completion using `project_progress(action="record")`.
4. If the developer declines, call `manage_memory(action="skip_onboarding")` to suppress repeated prompts.
