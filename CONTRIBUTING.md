# Feedback & Contribution Policy

## Pull Requests Are Not Accepted

**memorize** does not accept external pull requests or code contributions at this time. Please do not open PRs, as they will be closed without merging.

## Feedback, Ideas, and Bug Reports Are Welcome

Even though code contributions are closed, opinions, suggestions, and bug reports are welcome—share what you noticed in an issue, and reports are reviewed and addressed immediately once seen:

- **Feedback & Suggestions**: Open a [GitHub Issue](https://github.com/marbasdev/memorize/issues) in English or Korean.
- **Bug Reports**: Include your OS, MCP client (`Claude Code`, `Antigravity`, etc.), active inference engine (`Local GGUF`, `Gemini`, `System 1`, or `Custom Local URL`), and reproduction steps.
- **Security Vulnerabilities**: Do not post exploit details or vulnerability proofs in a public issue (see [SECURITY.md](./SECURITY.md))—any report is addressed immediately once seen.

## Building & Verifying Locally (Reference)

No external database, C compiler, or Node build pipeline is needed (`CGO_ENABLED=0`, pure Go with embedded dashboard templates):

```bash
gofmt -l .
CGO_ENABLED=0 GOOS=darwin go vet ./...
CGO_ENABLED=0 GOOS=linux go vet ./...
CGO_ENABLED=0 GOOS=windows go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -o bin/memorize ./cmd/memorize
```
