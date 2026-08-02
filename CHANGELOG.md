# Changelog

All notable changes to Kungfu are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v1.2.0] — 2026-08-02

### Architecture

- Single Go binary with all assets embedded via `embed.FS`
- PostgreSQL backend with `pgx/v5` connection pool
- Stateless owner sessions using HMAC-SHA256 signed cookies
- In-memory sliding-window rate limiter (7 dimensions)
- 5-language i18n (English, Japanese, Chinese, Korean, Spanish) embedded in binary

### Security

- Client IP extraction only honors `X-Forwarded-For` from trusted proxy CIDRs
- Owner login runs constant-time bcrypt verification (dummy hash on missing user)
- JSON request bodies capped at 256KB via `http.MaxBytesReader`
- Rate limiter garbage collection every 5 minutes
- Database URL properly URL-encoded via `net/url.QueryEscape`

### Performance

- 1,335 req/s at 100 concurrent connections
- 25.7MB RSS memory footprint
- Single 17MB binary with zero runtime file dependencies

### Added

- `VERSION` file as single source of truth for version
- `internal/version` package with `//go:embed` and build-time override support
- `CONTRIBUTING.md` with code standards and PR process
- MIT license

## [v1.0.0] — 2026

Initial release.
