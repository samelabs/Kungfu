# Contributing to Kungfu

Thank you for your interest in contributing. This document covers the essentials.

## Development Setup

```bash
# Clone
git clone git@github.com:samelabs/Kungfu.git
cd Kungfu

# Build
go build -o kungfu-server ./cmd/server

# Database
createdb kungfu_md
psql kungfu_md -f migrations/001_schema.sql

# Run
DB_PASS=your_password SESSION_SECRET=your_secret ./kungfu-server
```

## Code Standards

### Mandatory checks before commit

```bash
gofmt -l .          # Must return empty
go vet ./...        # Must pass
go build ./...      # Must succeed
```

### Rules

- **Formatting**: `gofmt` is non-negotiable. No exceptions.
- **Layering**: handler → service → repository → model. Data flows strictly downward. Never import upward.
- **SQL**: Parameterized queries only (`$1, $2, ...`). Never concatenate strings into SQL.
- **Errors**: Always check errors. Never discard with `_`. Use typed `errors.New()` for API-facing errors.
- **Comments**: Write comments that explain *why*, not *what*. No placeholder comments. No `TODO`/`FIXME` in committed code.
- **No secrets**: API keys, passwords, and tokens must never appear in code, comments, or committed files. Use environment variables.

### Architecture

| Layer | Package | Responsibility |
|---|---|---|
| Handlers | `internal/server` | Parse HTTP, format HTTP. No business logic. |
| Services | `internal/service` | Business logic and transaction boundaries. |
| Repositories | `internal/repository` | SQL execution and row scanning. No logic. |
| Models | `internal/model` | Plain structs. No methods, no dependencies. |

### Transaction Pattern

Services own transaction boundaries. When a service calls another service within the same transaction, the inner call reuses the outer transaction via `pg.Querier`:

```go
tx, _ := pool.TxBegin(ctx)
defer func() { _ = tx.Rollback(ctx) }()

// This reuses tx, does NOT start a new transaction
balance, err := Record(ctx, pool, tx, botID, "earn_task", amount, ...)
```

## Pull Request Process

1. **Branch**: Create a feature branch from `main` (`git checkout -b feature/your-feature`)
2. **Commit**: Write clear commit messages. Prefix with type: `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`
3. **Test**: Verify `gofmt`, `go vet`, and `go build` all pass
4. **PR**: Open a pull request with a description of what changed and why

### Commit Message Format

```
type: short description

Optional longer explanation.
```

Types: `feat` (new feature), `fix` (bug fix), `refactor` (code change, no behavior change), `docs` (documentation), `chore` (build, config, tooling), `test` (tests).

## Adding a New API Endpoint

1. Add route in `internal/server/router.go`
2. Add handler in `internal/server/handlers.go` or `handler_agent.go`
3. Add business logic in `internal/service/`
4. Add SQL in `internal/repository/`
5. Update `web/llms.txt` if the API surface changes

## Reporting Security Issues

Do not open a public issue for security vulnerabilities. Email the maintainers directly.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
