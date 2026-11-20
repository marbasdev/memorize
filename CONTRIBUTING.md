# Feedback & Contribution Policy

## Pull Requests Are Not Accepted

**praxis** does not accept external pull requests or code contributions at this time. Please do not open PRs, as they will be closed without merging.

## Feedback, Ideas, and Bug Reports Are Welcome

Even though code contributions are closed, opinions, suggestions, and bug reports are welcome—share what you noticed in an issue, and reports are reviewed and addressed immediately once seen:

- **Feedback & Suggestions**: Open a [GitHub Issue](https://github.com/marbasdev/praxis/issues) in English or Korean.
- **Bug Reports**: Include your OS, run mode (`Solo Local Daemon` vs `Team Server` / thin client), MCP client (`Claude Code`, `Antigravity`, etc.), active inference engine (`Local GGUF`, `Gemini`, `System 1`, or `Custom Local URL`), and reproduction steps.
- **Security Vulnerabilities**: Do not post exploit details or vulnerability proofs in a public issue (see [SECURITY.md](./SECURITY.md))—any report is addressed immediately once seen.

## Building & Verifying Locally (Reference)

No external database, C compiler, or Node build pipeline is needed (`CGO_ENABLED=0`, pure Go with embedded dashboard templates):

```bash
gofmt -l .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go vet ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./...
CGO_ENABLED=0 go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
CGO_ENABLED=0 go run golang.org/x/tools/cmd/deadcode@v0.53.0 ./cmd/praxis
CGO_ENABLED=0 go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -o bin/praxis ./cmd/praxis
```
