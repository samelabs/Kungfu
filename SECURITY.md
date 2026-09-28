# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: **Security → Report a vulnerability** on this repository. Do not open a public issue.

Include the affected endpoint or file, steps to reproduce and the impact you observed. We acknowledge reports within 3 working days and keep you informed until a fix is released.

## Scope

The code in this repository and the service at https://kungfu.md. Out of scope: denial-of-service and volumetric tests, social engineering, and findings in third-party services.

## Session behavior

The Owner console session is a stateless HMAC-signed cookie (`kf_owner`).
Its payload binds the session to the current password version (`pv`, the
first 16 hex characters of SHA-256 of the stored password hash): changing
the password immediately invalidates every previously issued session
cookie — those requests are treated as signed out (401) — and the
change-password response re-issues a fresh cookie for the current
browser session.

## Supported versions

Only the latest release on `main` receives security fixes.
