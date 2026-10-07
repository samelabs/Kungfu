# Work Order I0 — Idempotent Result Snapshot

## BASE / Authority

- Code BASE: a9e0048dc1b59f4ef8e612ca359b389d47560619
- Paradigm: kungfu.md blob f48c2f90908197b47247ecc0ccc6511399939a77
- PRD: docs/kungfu-prd.md blob c674b8b69f1f8a31e0bfdc7e8e4bf9c4e3939172
- Recovery plan: docs/kungfu-dev-plan.md blob 9e7f152b211d37b489b7b29c5c2de40d228aa43b
- Stage: I0 only

This work order fixes A3 / PRD §8.3 and §12 I2. It does not repair Thread product semantics.

## Allowed code files

Only:

- internal/service/thread_state.go
- internal/service/thread_state_test.go

A new dedicated test file under internal/service is allowed only if keeping I0 tests separate materially improves readability.

No SQL, model, repository, Task, Memory, HTTP, MCP, Web, or migration change is allowed.

## KEEP — do not change

Every current non-replay business rule remains exactly as it is for I0, including rules already known to be wrong and assigned to later stages:

- RootCreatorCanGovernThread behavior stays until C0.
- Current Branch parent-anchor behavior stays until C1.
- Current ReplyThreadState / receipt semantics stay until C2.
- Current partner/key role behavior stays until C0.
- Existing request hashing and conflict identity remain (role, operation, key) + request_hash.
- Existing transaction boundaries remain.
- Existing first-call return types remain.
- No public interface is added or renamed.

If an I0 implementation requires changing one of these, STOP: the proposed I0 design is too broad.

## Problem to fix

thread_idempotency.result_ref is durable TEXT, but thread_state.go stores sparse references such as ThreadID / EntryID / RoleID. On replay, most operations re-read current Thread / ThreadRole state.

Therefore:

1. execute write K1;
2. mutate the object later using K2;
3. replay K1;

can return K2-era state. This violates Kungfu A3 and PRD: same idempotency key + same request must return the first successful action's business result, except one-time raw secret material must not be re-disclosed.

No schema change is needed: result_ref can already hold JSON.

## Required behavior

### 1. Persist replay-safe result snapshots

Replace the sparse-ref replay contract with a persisted replay-safe business-result snapshot.

Equivalent internal designs are allowed, but all of the following must hold:

- completeThreadIdempotency stores enough immutable result data to reconstruct the first successful business result without reading the current business object.
- begin/replay validates request_hash exactly as today.
- same key + different request remains ErrThreadIdempotencyConflict and causes no business side effect.
- a completed replay never re-derives Thread, ThreadRole, ThreadMemory, subject, permission, status, key fingerprint, or similar result fields from current tables.
- failed transactions do not leave a completed result snapshot.

### 2. Cover every current thread_state write path

The snapshot/replay path must cover:

- CreateThreadState
- ReplyThreadState
- BranchThreadState
- HandleThreadInput
- AddThreadParticipant
- JoinThreadState
- ChangeThreadPermission
- RemoveThreadParticipant
- CloseThreadState
- ReopenThreadState
- ResetThreadJoinKeyState
- RevokeThreadJoinKeyState
- UpdateThreadSubjectState

Do not leave a mix where some replay from snapshot and some re-fetch current state.

### 3. One-time secret rule

For operations that can first-return a raw join key:

- CreateThreadState with IssueJoinKey
- BranchThreadState with IssueJoinKey
- ResetThreadJoinKeyState

the durable replay snapshot MUST NOT contain the raw join key.

First successful call:
- may return raw key;
- returns fingerprint.

Replay:
- raw key is empty / absent;
- returns the fingerprint from the original successful action;
- other non-secret business fields come from the original successful snapshot;
- later key reset/revoke must not alter that old replay result.

### 4. Replay metadata

AlreadyApplied (or equivalent internal replay marker) may indicate that the response is a replay. That metadata is not the business result and may differ between first call and replay.

Everything else in the business result that was returned by the first successful action must not drift because the object later changed.

## Required tests

Add focused tests that prove mutation-after-success does not alter old replay.

At minimum:

1. Create snapshot
   - Create Thread with K1.
   - Change subject using another action/key.
   - Replay Create K1.
   - Replayed Thread subject equals the original Create result, not current subject.

2. Add snapshot
   - Add participant with K1.
   - Change that participant permission using K2.
   - Replay Add K1.
   - Replayed role permission equals the permission returned by the original Add.

3. Join snapshot
   - Join with K1.
   - Change joined role permission later.
   - Replay Join K1.
   - Replayed role is the original joined role snapshot.

4. Permission snapshot
   - Change permission with K1.
   - Change again with K2.
   - Replay K1.
   - Return equals the first permission result.

5. Close snapshot
   - Close with K1.
   - Reopen with K2.
   - Replay Close K1.
   - Replayed Thread is closed.

6. Subject snapshot
   - Subject A → B with K1.
   - B → C with K2.
   - Replay K1.
   - Replayed Thread subject is B.

7. Key secret + fingerprint
   - Reset key K1; record fp1/raw1.
   - Reset with K2; fp2 != fp1.
   - Replay K1.
   - raw key not returned.
   - fingerprint == fp1, not fp2.

8. Create/Branch secret replay
   - For existing tests that create/branch with IssueJoinKey, assert replay does not re-disclose raw key and fingerprint remains the first one even after later key changes.

9. Conflict
   - same operation/key + different request still returns ErrThreadIdempotencyConflict.
   - verify no extra Thread/Entry/Role/state transition.

10. Replay path audit
   - all !acquired paths in thread_state.go reconstruct their business result only from the stored idempotency snapshot.
   - no repository FindThread/FindThreadRole/FindThreadMemory call is used to rebuild replay business output.

Existing non-replay tests must continue to pass unchanged except old assertions that explicitly expected replay to drift to current state; those assertions must be corrected to the frozen PRD.

## Forbidden changes

- Do not add DB columns/table/migration.
- Do not change operation names or idempotency key identity.
- Do not change request normalization outside what is necessary to serialize the existing request exactly as before.
- Do not change authorization.
- Do not change Thread/Role/receipt state transitions.
- Do not remove RootCreatorCanGovernThread in I0.
- Do not change Branch or Reply behavior.
- Do not implement join_role, join_source, partner note, leave, LAST_MANAGER, ask, pair, summary, Task, or Todo.
- Do not add an API presenter.

## Stop conditions

STOP and report the exact blocker instead of broadening the diff if:

- replay-safe snapshots cannot be implemented without changing SQL/model/repository;
- a result contains data that cannot safely be persisted without leaking a raw secret;
- preserving first-call business output conflicts with an existing public contract not described in the PRD;
- a test failure requires a business-rule change rather than replay storage/decoding.

## Required report

Return:

- BASE
- new HEAD
- changed files
- line-level summary of the snapshot representation
- list of every replay path converted
- tests added/updated
- exact test commands and results
- CI run URL/id and exact SHA
- git status
- any STOP/blocker

## Gate

I0 is complete only when:

- Allowed-file diff only.
- All current Thread service tests pass.
- Full repository test/build/migration gates pass.
- Exact new HEAD CI is success.
- Reverse audit confirms no business semantics changed.
- No replay path rebuilds business result from current object state.

Only after this gate may C0 be issued.
