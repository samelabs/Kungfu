# Kungfu AI Development Contract

This file is the execution contract for AI-assisted development in this repository.

AI agents must follow this file before making any code changes.

## Primary Goal

Preserve business behavior while converging the codebase toward the repository architecture defined in:

- `docs/domain-map.md`
- `docs/architecture.md`
- `docs/code-standards.md`
- `docs/routing.md`

The goal is not abstract elegance. The goal is:

1. stable behavior
2. explicit exposure boundaries
3. less mixed responsibility
4. less future drift

## Document Precedence

When multiple documents are relevant, use this precedence order:

1. `AGENT.md`
2. `docs/domain-map.md`
3. `docs/architecture.md`
4. `docs/code-standards.md`
5. `docs/routing.md`
6. `README.md`

If two documents appear to conflict:

1. preserve current production behavior
2. prefer stricter exposure boundaries
3. prefer clearer domain ownership
4. update the conflicting docs before or with the code change

## Hard Rules

These rules are mandatory.

1. Do not change business behavior unless the task explicitly requires it.
2. Do not widen agent-visible data exposure.
3. Do not move owner-only fields into agent APIs.
4. Do not add new domain-heavy logic into `core/`.
5. Do not create new `*Utils`, `*Helper`, or `*Manager` classes for domain logic.
6. Do not mix multiple business domains in one refactor.
7. Do not change routing contracts unless the task explicitly requires a route change.
8. Do not rewrite for style alone.

## Exposure Rules

### Never expose to agents

- `postapi`
- owner session data
- owner review notes
- downstream `response_body`
- task log payloads
- task delivery diagnostics
- owner-only audit data

### Allowed to owners on owner-authenticated endpoints

- `postapi`
- task diagnostics
- task `response_body`
- task log payloads
- owner account balance
- current agent key

If unsure whether a field is safe for agents, default to **not exposing it**.

## Domain-First Refactor Rule

All refactors must start by selecting exactly one business domain:

1. Identity and Account
2. Kungfu Memory
3. Platform Tasks
4. Audit and Logs

Within that domain, changes should then follow layer boundaries:

- API
- Service
- Repository
- Presenter
- Validator
- Exception

Do not start by “cleaning files”. Start by naming the domain.

## Migration Rules

This repository is in transition.

### Allowed

- keep old code in `core/` temporarily
- extract new code into target directories
- reduce one mixed responsibility at a time

### Not allowed

- adding fresh domain logic to `core/`
- duplicating the same responsibility in old and new locations without a removal plan
- moving SQL from one mixed class into another mixed class and calling that refactor done

## New Code Placement

New extracted code must go to:

- `services/`
- `repositories/`
- `presenters/`
- `validators/`
- `exceptions/`

Only shared technical infrastructure may remain or be added in `core/`, such as:

- `Database`
- `Response`
- `RateLimiter`
- `Security`
- `PublicCode`

## Routing Rules

Routing is not a performance problem. Treat it as a structure problem.

Router code may:

- match path
- match method
- inject route params
- dispatch handler

Router code may not:

- query DB
- decide billing
- inspect task state
- shape API payloads

Do not move business logic into routing code.

## Service Rules

Service classes should:

- orchestrate business steps
- define transaction boundaries
- coordinate repositories
- call validators
- call presenters indirectly through API response mapping or service return models

Service classes should not:

- emit `Response::error()` or `Response::success()`
- hold large raw SQL blocks
- decide owner vs agent field allowlists inline

## Repository Rules

Repositories should:

- own SQL and persistence access
- return rows or stable persistence DTOs

Repositories should not:

- emit HTTP errors
- contain user-facing wording
- decide exposure boundaries

Each repository has one primary owning domain.

Current critical ownership rule:

- `TaskLogRepository` belongs to the Platform Tasks domain
- the Audit and Logs domain may read it, but does not own it

## Presenter Rules

Presenters are mandatory where exposure differs by audience.

At minimum, keep these presenter boundaries explicit:

- `AgentTaskPresenter`
- `OwnerTaskPresenter`
- `OwnerLogPresenter`

Never reuse an owner presenter for an agent response.

## Naming Rules

Use explicit domain names:

- `agent`
- `owner`
- `bot`
- `task`
- `delivery`
- `funding`
- `kungfu`

Use explicit class suffixes:

- `*Service`
- `*Repository`
- `*Presenter`
- `*Validator`
- `*Exception`

Avoid vague class names for domain logic:

- `Utils`
- `Helper`
- `Manager`
- `Processor`

## Change Discipline

Every implementation task should follow this order:

1. identify the domain
2. identify the mixed responsibility being removed
3. preserve output shape and behavior
4. compare before/after exposure boundaries
5. update docs if boundaries or ownership change

## When Writing Plans or PR Notes

Always state:

1. selected business domain
2. selected responsibility split
3. behavior expected to remain unchanged
4. exposure boundary expected to remain unchanged or become stricter

## Failure Conditions

An AI refactor is considered incorrect if it does any of the following:

1. widens agent-visible data
2. introduces new mixed-domain files
3. adds new domain logic to `core/`
4. introduces new service-layer `Response::error()` usage
5. changes route contracts without explicit intent
6. changes behavior without calling it out

## Final Reminder

Kungfu is a small codebase. That is an advantage.

Do not use that advantage to justify casual structure.
Use it to establish strict boundaries early, while the refactor cost is still low.
