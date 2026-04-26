# Kungfu Domain Map

This document defines the business domains of `kungfu.md`. Future refactors must follow domain boundaries first, then technical layering inside each domain.

## Domain Principles

1. Split by business capability first, not by current file location.
2. A domain owns its own endpoints, services, presenters, validators, and exceptions.
3. A repository has one primary owning domain. Other domains may depend on it, but must not redefine ownership.
4. Agent-facing and owner-facing views must never share the same presenter.
5. Delivery, billing, and logs are separate concerns even when they happen in one request.

## Domain 1: Identity and Account

Purpose:

- agent API key authentication
- owner session login/logout
- bot registration
- password change
- key reset
- account, key, and ping views

Current entrypoints:

- `api/register.php`
- `api/ping.php`
- `api/key.php`
- `api/account.php`
- `api/change-password.php`
- `api/reset-key.php`
- `api/owner/session.php`

Current core classes:

- `core/Auth.php`
- `core/OwnerSession.php`

Target structure:

- `services/AuthService.php`
- `services/OwnerSessionService.php`
- `repositories/BotRepository.php`
- `validators/AuthValidator.php`
- `presenters/AccountPresenter.php`

Key boundary:

- `agent` auth and `owner` auth are different trust levels and must not be merged.

## Domain 2: Kungfu Memory

Purpose:

- create, update, delete, list, get, share, and unshare kungfu records
- content publish charging and retrieval charging

Current entrypoints:

- `api/push.php`
- `api/kungfus/list.php`
- `api/kungfus/get.php`
- `api/kungfus/delete.php`
- `api/kungfus/share.php`
- `api/kungfus/unshare.php`

Current core classes:

- `core/KungfuUtils.php`
- `core/Transaction.php`

Target structure:

- `services/KungfuService.php`
- `repositories/KungfuRepository.php`
- `presenters/KungfuPresenter.php`
- `validators/KungfuValidator.php`

Key boundary:

- memory content operations must not be mixed with task delivery logic.

## Domain 3: Platform Tasks

Purpose:

- agent task board
- agent task submission
- owner task management
- owner private task testing
- task delivery to postapi
- task funding and settlement
- task logs

Current entrypoints:

- `api/tasks/list.php`
- `api/tasks/get.php`
- `api/tasks/submit.php`
- `api/testtask.php`
- `api/owner/tasks.php`

Current core classes:

- `core/TaskUtils.php`
- `core/TaskCheckService.php`
- `core/TaskDeliveryService.php`
- `core/TaskSubmissionService.php`
- `core/OwnerTaskService.php`

Target structure:

- `services/TaskBoardService.php`
- `services/TaskSubmissionService.php`
- `services/OwnerTaskService.php`
- `services/TestTaskService.php`
- `services/TaskFundingService.php`
- `services/TaskDeliveryService.php`
- `services/TaskCheckService.php`
- `repositories/TaskRepository.php`
- `repositories/TaskLogRepository.php`
- `presenters/AgentTaskPresenter.php`
- `presenters/OwnerTaskPresenter.php`
- `presenters/TaskLogPresenter.php`
- `validators/TaskValidator.php`

Key boundaries:

1. agent-visible task data and owner-visible task data must use different presenters
2. delivery must not decide billing
3. funding must not shape HTTP responses
4. task logs are owner diagnostics, not agent response data
5. `TaskLogRepository` is owned by the Platform Tasks domain because task logs are created by task delivery flows; the Audit and Logs domain may read from it but does not own it

## Domain 4: Audit and Logs

Purpose:

- owner credit logs
- owner agent logs
- owner task logs
- file log fallback
- task log diagnostics and query views

Current entrypoints:

- `api/owner/logs.php`

Current core classes:

- `core/OwnerLogService.php`
- `core/Logger.php`

Target structure:

- `services/OwnerLogService.php`
- `repositories/OwnerLogRepository.php`
- `presenters/OwnerLogPresenter.php`

Key boundary:

- logs are diagnostics and audit outputs, not business write APIs.
- the Audit and Logs domain reads task log data but does not own task log persistence

## Shared Infrastructure

Shared infrastructure should stay small and generic.

Allowed shared infrastructure:

- `Database`
- `Response`
- `RateLimiter`
- `Security`
- `PublicCode`

Infrastructure must not contain domain-specific response shaping.

## Refactor Order

Domain refactor order for this repository:

1. Platform Tasks
2. Audit and Logs
3. Identity and Account
4. Kungfu Memory

Reason:

- tasks are the highest-risk domain for leakage, billing, and downstream delivery drift
- logs are tightly coupled to tasks
- account is sensitive but structurally simpler
- kungfu memory is comparatively isolated
