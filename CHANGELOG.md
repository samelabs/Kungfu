# Changelog

All notable changes to Kungfu are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v1.2.0] — 2026-08-02

### Changed

- **Architecture rewrite**: PHP → Go (single binary, all assets embedded)
- **Database**: MySQL → PostgreSQL (pgx/v5 connection pool)
- **Templates**: PHP `.tpl.php` files → Go server-side rendering with embedded i18n
- **Sessions**: PHP session → stateless HMAC-SHA256 signed cookies
- **Rate limiter**: database-backed → in-memory sliding window (7 dimensions)
- **Static assets**: filesystem → `embed.FS` (compiled into binary)
- **API version** field in responses now sourced from `VERSION` file

### Security

- Client IP extraction only honors `X-Forwarded-For` from trusted proxy CIDRs
- Owner login runs constant-time bcrypt verification (dummy hash on missing user)
- JSON request bodies capped at 256KB via `http.MaxBytesReader`
- Rate limiter garbage collection every 5 minutes (prevents memory exhaustion)
- Database URL properly URL-encoded via `net/url.QueryEscape`

### Performance

- **~2x throughput** vs PHP (1,335 req/s at 100 concurrent connections)
- **84% memory reduction** (25.7MB vs ~160MB RSS)
- Single 17MB binary with zero runtime file dependencies

### Added

- `VERSION` file as single source of truth for version
- `internal/version` package with `//go:embed` and build-time override support
- 5-language i18n (English, Japanese, Chinese, Korean, Spanish) embedded in binary
- `CONTRIBUTING.md` with code standards and PR process
- MIT license

### Removed

- All PHP source code and PHP-FPM dependency
- MySQL dependency
- External template files
- 211 PHP-legacy reference comments from Go source
- Dead code: `scanBalance`, `FileLog`, `parseJSONBody`, `TxRunner`, `FormatMoney`

---

## [v1.0.0] — 2026 (PHP era)

Initial PHP release with MySQL backend.
