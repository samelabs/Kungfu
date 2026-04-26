# Kungfu Code and Naming Standards

This document defines repository-wide code standards for future refactors and new code.

## General Rules

1. prefer small, explicit classes over large utility buckets
2. prefer domain names over generic helper names
3. do not mix owner-facing and agent-facing concepts in the same formatter
4. do not add abstractions unless they remove a current mixed responsibility
5. preserve behavior first, then improve structure

## Naming Rules

### Domain terms

Use these terms consistently:

- `agent`: API key caller
- `owner`: human operator authenticated by owner session
- `bot`: persistence model in `tb_bots` and legacy storage naming
- `task`: paid platform work contract
- `delivery`: downstream postapi delivery attempt/result
- `funding`: owner budget lock, debit, refund
- `memory` or `kungfu`: stored agent working knowledge

Rules:

1. use `Owner*` for owner-only classes
2. use `Agent*` for agent-visible presenters or services
3. use `Task*` only for task domain logic
4. do not use vague names like `Helper`, `Manager`, or `Utils` for new domain classes unless they are truly generic shared infrastructure

### Class names

Use suffixes consistently:

- `*Service` for business orchestration
- `*Repository` for persistence access
- `*Presenter` for response shaping
- `*Validator` for pure validation
- `*Exception` for domain/application errors

Examples:

- `OwnerTaskService`
- `TaskRepository`
- `AgentTaskPresenter`
- `TaskValidator`

### Method names

Use verbs that describe the business action:

- `listOpenTasks`
- `findOwnerTaskByCode`
- `submitTask`
- `deliverTestTask`
- `addBudget`
- `formatOwnerTaskDetail`

Avoid ambiguous names such as:

- `handle`
- `process`
- `doTask`
- `runAll`

## File Placement Rules

### API files

API files should stay thin.

Allowed in API files:

- auth/session checks
- request parsing
- one service call
- response mapping

Not allowed in API files:

- long SQL
- inline formatter functions
- inline validation libraries
- duplicated business flow branches
- direct use of repository SQL in endpoint files

### Service files

Allowed:

- transaction scopes
- sequencing business operations
- repository coordination

Not allowed:

- raw SQL blocks
- `Response::success()` or `Response::error()`
- large audience-specific field shaping
- new domain logic added to `core/` instead of the target layer directories

### Repository files

Allowed:

- SQL
- row mapping close to storage

Not allowed:

- user-facing wording
- HTTP concerns
- owner/agent exposure decisions

### Presenter files

Allowed:

- output field allowlists
- action labels
- formatting for owner or agent audience

Not allowed:

- DB access
- service orchestration

## Exposure Rules

These are hard rules.

### Never expose to agents

- `postapi`
- owner session data
- owner review notes
- downstream `response_body`
- task log payloads
- task log diagnostics

### Allowed for owners on owner-authenticated endpoints

- `postapi`
- owner task diagnostics
- task log payloads
- task log `response_body`
- owner account balance
- current agent key

## Error Handling Rules

Target direction:

1. repositories return rows or null, and may throw low-level exceptions
2. services throw application/domain exceptions
3. API layer maps exceptions to `Response`

During migration:

- do not introduce new `Response::error()` calls inside new services
- when touching old services, prefer reducing HTTP coupling rather than increasing it
- do not create new domain `*Utils` classes to avoid creating fresh dumping grounds

## Comment Rules

Use comments sparingly.

Allowed:

- one short comment before a non-obvious block
- boundary comments for security-sensitive exposure rules

Avoid:

- narrating obvious code
- stale design notes inside implementation

## Refactor Rules

Every refactor must follow these constraints:

1. one domain at a time
2. one responsibility split at a time
3. no opportunistic behavior changes
4. no unrelated rename churn
5. compare output shape before and after
6. do not leave duplicate ownership rules unresolved across documents

## Documentation Rules

Whenever a new domain service/repository/presenter/validator is introduced:

1. place it in the correct target directory
2. name it using the suffix rules above
3. keep public methods narrow and explicit
4. update architecture documentation if the domain boundary changes
