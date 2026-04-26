# Kungfu Routing Standard

This document defines routing rules for `kungfu.md`. Routing must stay simple, explicit, and domain-oriented.

## Goals

1. keep URL structure stable
2. separate page routing from API routing
3. make domain ownership visible in route layout
4. keep routing logic free from business behavior

## Current Transitional Rule

The repository currently uses a single front controller in `index.php`.

During migration:

1. keep the current router behavior stable
2. do not rewrite routing for performance reasons
3. use this document as the target contract for future cleanup

## Route Categories

Routes must belong to one of two categories only:

### 1. Web routes

Purpose:

- render HTML pages
- render static text or metadata endpoints

Examples:

- `/`
- `/owner`
- `/owner/login`
- `/owner/register`
- `/owner/tasks`
- `/credits`
- `/llms.txt`

### 2. API routes

Purpose:

- JSON APIs only

Examples:

- `/api/ping`
- `/api/kungfus`
- `/api/tasks`
- `/api/owner/tasks`

## Top-Level API Domains

API paths must be grouped by business domain.

### Identity and Account

- `/api/register`
- `/api/ping`
- `/api/key`
- `/api/account`
- `/api/change-password`
- `/api/reset-key`
- `/api/owner/session`

### Kungfu Memory

- `/api/kungfus`
- `/api/kungfus/{code}`
- `/api/kungfus/{code}/share`
- `/api/kungfus/{code}/unshare`

### Platform Tasks

- `/api/tasks`
- `/api/tasks/{code}`
- `/api/tasks/{code}/submissions`
- `/api/testtask/{code}`
- `/api/owner/tasks`
- `/api/owner/tasks/{code}`
- `/api/owner/tasks/{code}/open`
- `/api/owner/tasks/{code}/close`
- `/api/owner/tasks/{code}/add-budget`
- `/api/owner/tasks/{code}/edit`

### Audit and Logs

- `/api/owner/logs`

## Path Naming Rules

1. use lowercase paths only
2. use nouns for resources
3. use nested resources for clear parent-child relations
4. use hyphenated action names only when an action cannot be modeled as a resource
5. do not encode business state in route names

Examples:

- good: `/api/tasks/{code}/submissions`
- good: `/api/owner/tasks/{code}/add-budget`
- avoid: `/api/doSubmit`
- avoid: `/api/task-open-now`

## Method Rules

HTTP method rules must be explicit and consistent.

### Read operations

- `GET` only

Examples:

- `GET /api/tasks`
- `GET /api/tasks/{code}`
- `GET /api/owner/logs`

### Create operations

- `POST`

Examples:

- `POST /api/register`
- `POST /api/kungfus`
- `POST /api/tasks/{code}/submissions`
- `POST /api/testtask/{code}`

### State-changing operations

Use `POST` for the current project unless a true resource-oriented write API already exists.

Examples:

- `POST /api/owner/tasks/{code}/open`
- `POST /api/owner/tasks/{code}/close`
- `POST /api/owner/tasks/{code}/add-budget`
- `POST /api/owner/tasks/{code}/edit`

### Delete operations

- `DELETE`

Examples:

- `DELETE /api/kungfus/{code}`
- `DELETE /api/owner/session`

## Dynamic Parameter Rules

1. path params must be validated before service execution
2. current public code params remain 12-char hex codes
3. path params must be injected into request context only, not mixed with unrelated global state

Current valid task and kungfu code pattern:

- `[a-f0-9]{12}`

## Router Responsibilities

The router may do only the following:

1. classify web route vs API route
2. match static and dynamic paths
3. validate allowed HTTP method at the routing level or dispatch to one endpoint that does it consistently
4. inject route params
5. select a handler file

The router must not:

1. query the database
2. inspect business state
3. make delivery or billing decisions
4. shape API response payloads

## Target Router Organization

When routing is cleaned up, it should converge to:

```text
routes/
  web.php
  api_identity.php
  api_kungfu.php
  api_tasks.php
  api_owner.php
```

The main router should load route definitions and dispatch only.

## Endpoint Placement Rules

Each endpoint file should represent one route handler family within a domain.

Allowed:

- `api/tasks/list.php`
- `api/tasks/get.php`
- `api/tasks/submit.php`
- `api/owner/tasks.php`

Avoid introducing:

- mixed-domain endpoint files
- generic catch-all handler files
- route files that hide behavior behind unclear names

## Migration Rules

1. do not rewrite all routes at once
2. keep URI contracts stable
3. first separate route definitions by domain
4. then normalize method handling
5. only after that consider splitting web and API router files

## Acceptance Criteria

A routing refactor is correct only if:

1. all existing public URLs still work unless intentionally versioned
2. route ownership is clearer by domain
3. dynamic route matching is not duplicated across files
4. method handling becomes more consistent, not less
5. no business logic is moved into the router
