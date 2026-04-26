# Kungfu Architecture Baseline

This document is the repository-level architecture contract. New code and refactors must follow it.

## Goals

1. keep agent-visible and owner-visible data boundaries explicit
2. keep business rules independent from HTTP transport
3. keep data access independent from response shaping
4. keep domain logic grouped by business capability

## Document Authority

This document works together with:

- `AGENT.md`
- `docs/domain-map.md`
- `docs/code-standards.md`
- `docs/routing.md`

Authority order:

1. `AGENT.md`
2. `docs/domain-map.md`
3. this document
4. `docs/code-standards.md`
5. `docs/routing.md`

If a refactor discovers a conflict:

1. keep production behavior stable
2. keep exposure boundaries unchanged or tighter
3. resolve the documentation conflict before expanding the refactor

## Layer Model

Each business domain should converge to the following layers.

### 1. API layer

Location:

- `api/`

Responsibilities:

- authenticate request
- parse input
- call one service method
- convert application errors to `Response`

Must not do:

- direct SQL
- large validation blocks
- response field shaping rules
- multi-step business workflows

### 2. Service layer

Location:

- `services/`

Responsibilities:

- business orchestration
- transaction boundaries
- coordination across repositories
- domain rule sequencing

Must not do:

- call `Response::error()` directly
- own raw SQL statements
- decide agent/owner field exposure

### 3. Repository layer

Location:

- `repositories/`

Responsibilities:

- select, insert, update, delete
- return raw rows or stable persistence DTOs

Must not do:

- HTTP handling
- business wording
- presenter logic

### 4. Presenter layer

Location:

- `presenters/`

Responsibilities:

- define exact fields returned to a caller
- format domain rows for a target audience

Must not do:

- query database
- enforce business state transitions

### 5. Validator layer

Location:

- `validators/`

Responsibilities:

- input rule checks
- field-level validation
- reusable business preconditions that are pure checks

Must not do:

- database writes
- HTTP response emission

### 6. Exception layer

Location:

- `exceptions/`

Responsibilities:

- application error types
- validation errors
- state errors
- not-found and permission errors

Must not do:

- emit JSON

## Current Transitional Rule

This repository is not yet fully migrated.

During migration:

1. new extracted code must go into the target layer directories
2. old code may remain in `core/` temporarily
3. each refactor should reduce mixed responsibilities, not move them sideways
4. `core/` must shrink over time and must not receive new domain-heavy classes

## Boundary Rules

### Exposure boundaries

1. agent presenters and owner presenters must be separate classes
2. `postapi`, owner notes, delivery diagnostics, and raw downstream response data must never be returned by agent APIs
3. owner logs may include diagnostic payloads and response bodies only on owner-authenticated endpoints

### Delivery boundaries

1. `TaskDeliveryService` only sends requests and returns delivery results
2. billing and reward settlement belong to funding/settlement services
3. HTTP endpoints must not directly implement curl/stream fallback logic

### Logging boundaries

1. business flows may write logs
2. log querying belongs to log services/repositories
3. log formatting belongs to presenters, not repositories

### Transaction boundaries

1. open transaction scopes only in service layer
2. repository methods must not decide commit/rollback policy
3. funding and settlement operations should eventually centralize in dedicated services

## Recommended Directory Shape

```text
api/
services/
repositories/
presenters/
validators/
exceptions/
core/
views/
public/assets/
```

## Current Mapping

Short-term mapping of existing files:

- `core/Auth.php` -> later split into `services/AuthService.php`, `validators/AuthValidator.php`, `repositories/BotRepository.php`
- `core/OwnerSession.php` -> later `services/OwnerSessionService.php`
- `core/OwnerTaskService.php` -> later task domain service + repositories + presenters + validators
- `core/OwnerLogService.php` -> later log domain service + repository + presenter
- `core/TaskSubmissionService.php` -> later task submission orchestration only
- `api/testtask.php` -> later `services/TestTaskService.php`

## Refactor Acceptance Criteria

A refactor is considered correct only if:

1. business behavior is unchanged unless explicitly intended
2. exposure boundaries are unchanged or tighter
3. endpoint files become thinner
4. SQL moves out of services
5. presenter logic becomes more explicit
6. no unrelated domain is changed as a side effect
