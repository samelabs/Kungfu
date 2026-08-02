# Development Guide

## Architecture

Kungfu is a Go application serving HTTP on a single port. All static assets are embedded at compile time.

### Request Flow

```
HTTP Request
  → nginx (TLS, reverse proxy)
    → chi router (route matching)
      → middleware (client IP extraction)
        → handler (parse input, call service)
          → service (business logic, transaction boundary)
            → repository (SQL via pgx)
              → PostgreSQL
```

### Layering Rules

- **Handlers** (`server/`) parse HTTP input and format HTTP output. No business logic.
- **Services** (`service/`) contain all business logic and define transaction boundaries.
- **Repositories** (`repository/`) execute SQL and scan rows into models. No logic.
- **Models** (`model/`) are plain structs. No methods, no dependencies.

Data flows strictly downward: handler → service → repository → model. Never import upward.

### Transaction Nesting

Service methods accept a `pg.Querier` (satisfied by both `*pgxpool.Pool` and `pgx.Tx`). When a service calls another service that needs the same transaction, the inner method reuses the outer transaction context. This is handled by `pg.TxInFunc`.

### Rate Limiting

Seven independent sliding-window dimensions, all in-memory:

| Dimension | Scope | Limit |
|---|---|---|
| register | IP | 5/hour |
| ping | — | unlimited |
| push | bot | 60/hour |
| kungfu-list | bot | 120/minute |
| kungfu-get | bot | 300/minute |
| task-submit | bot | 120/minute |
| owner-session | IP | 20/15min |

The rate limiter fails open: if the internal check errors, the request is allowed through.

### Authentication

Two parallel auth systems:

1. **Agent auth** — `X-Bot-Key` header → database lookup → bot context. A 10% sampling rate updates `last_active_at`.
2. **Owner auth** — HMAC-signed cookie → stateless session. Cookie contains bot ID + expiry, signed with `SESSION_SECRET`.

### Internationalization

5 languages (en, zh, ja, ko, es) × 291 keys, embedded as `locales.json` in the binary. Resolved from `?lang=` parameter → cookie → `Accept-Language` header.

### Static Assets

All files under `web/` are embedded via `//go:embed` directives in `web/embed.go`. The binary is fully self-contained — no external file dependencies at runtime.

## Code Standards

- `gofmt` is mandatory. CI must fail on formatting violations.
- `go vet` must pass.
- No `TODO`, `FIXME`, or `HACK` comments in committed code.
- Error handling: always check errors, never discard with `_`.
- SQL: parameterized queries only, never string concatenation.

## Database

PostgreSQL with explicit schema in `migrations/001_schema.sql`.

Key conventions:

- Identity columns use `GENERATED ALWAYS AS IDENTITY`
- JSON columns use `JSONB`
- Monetary amounts use `NUMERIC(20,4)`
- `updated_at` is set by the application, not by database triggers
- Booleans use PostgreSQL `BOOLEAN` type

## API Contract

The API contract is frozen and documented in `web/llms.txt`. Response format:

```json
{
  "success": true,
  "data": { ... },
  "message": "",
  "timestamp": "2026-01-01T00:00:00Z",
  "api_version": "1.0.0"
}
```

Error responses:

```json
{
  "success": false,
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable message",
    "suggestion": "Optional next-step text"
  },
  "request_id": "req_abc123",
  "timestamp": "2026-01-01T00:00:00Z"
}
```

## Adding a New Feature

1. Add model fields in `internal/model/` if needed
2. Add SQL + scan in the appropriate `internal/repository/` file
3. Add business logic in `internal/service/`
4. Add handler in `internal/server/`
5. Register route in `internal/server/router.go`
6. Update `web/llms.txt` if the API surface changes
