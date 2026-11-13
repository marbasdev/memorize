# Security Policy

## Supported Versions

Security updates are applied to the latest release tag on `main`:

| Version | Supported |
| :--- | :--- |
| `1.0.x` (`main`) | Yes |
| `< 1.0.0` | No |

## Threat Model & Built-in Mitigations

`memorize` is designed to run locally on a developer workstation:

- **Loopback Binding & CSRF Protection**: The unified daemon binds to `127.0.0.1` by default and validates `Host`, `Origin`, and `Sec-Fetch-Site` on mutating endpoints (`/ui/*` requires `HX-Request`).
- **SQLite Storage**: Local state is stored in SQLite (`WAL` mode) with `0600` file permissions and `0700` data/model directories.
- **Credential Storage & Redaction**: API keys may be provided via environment variables or stored in local SQLite config (never echoed raw in API responses or SSR HTML, and secret-redacted in `guardrail_logs`).
- **Path & Download Safety**: Managed GGUF operations validate safe `.gguf` filenames, reject path traversal, restrict remote model downloads to `https://` (or loopback `http://127.0.0.1|localhost`), and verify `SHA256SUMS` during release binary bootstrapping.

## Reporting a Vulnerability

Please **do not post vulnerability details, exploit steps, or proof-of-concept payloads in a public GitHub issue**.

If you need to report a security concern, open a minimal issue asking for a private channel without disclosing technical details. Once details can be shared safely, include:
1. A clear description of the vulnerability and affected component.
2. Step-by-step reproduction instructions or a minimal proof of concept.
3. Any potential impact or suggested mitigation.

Any reported security issue is reviewed and addressed immediately once seen.
