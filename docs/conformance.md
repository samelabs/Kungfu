# Kungfu 3.0 — Conformance Statement

| | |
|---|---|
| Specification | Kungfu Protocol 1.0 — Draft ([`kungfu.md`](../kungfu.md)) |
| Implementation | Kungfu 3.0, branch `feat/room-face` |
| Profiles claimed | **Memory**, **Thread** |
| Profiles not yet claimed | **Task**, **Full** — gaps listed below |

This statement follows §10.3 of the specification. It claims only what the evidence supports. Being the reference implementation does not make Kungfu 3.0 conformant by default; every gap is listed, none is hidden.

## Memory profile — claimed

| Rule | Evidence |
|---|---|
| §5 immutable versions, update, publish, unpublish, withdraw | `internal/service/kungfu_revision_test.go` |
| §5 / §9 pinned references: own pins survive withdrawal, others' pins follow public status | `TestComposePinVisibilityMatrix` (`internal/service/composition_test.go`), `TestExpandedEntryServesPinnedRevision` (`assign_test.go`) |
| §9 delivered Memory versions readable by current members | `TestDeliveredMemoryReadable` (`closeout_test.go`), `TestCloseoutDeliveredMemoryBothDoors` (`internal/mcpserver/closeout_mcp_test.go`) |

## Thread profile — claimed

| Rule | Evidence |
|---|---|
| §6.1 single live admission, one-time secret, void on reissue and close | `thread_kernel_test.go`, `TestComposeRotationJoinClose` |
| §6.2 roles, last governor, membership end voids and forfeits | `TestAssignMembershipAndDeparture`, `TestJudgeForfeitSurvivesRejoin` |
| §6.3 responders fixed by the four rules; reply ends own response; handle; retract; note readable only by the parties | `thread_state_test.go`, `TestHandleNoteParties` |
| §6.4 assignment lifecycle, deadlines win, undecided not held against the taker | `assign_test.go`, `TestComposeCloseAndJudgeDeadline` |
| §6.5 closing is terminal; delivered work stays judgeable | `TestAssignClosedRoom`, `TestComposeCloseAndJudgeDeadline` |
| §8 turn for responses and assignments; discovery of untaken assignments | `todo_test.go`, `pm002_test.go` (`TestPM002InviteDiscoveryAtScale`, `TestPM003MineOpenLocator`) |
| L3 replay and receipts | `TestPostIdempotency`, `TestComposeReplayAfterExpiry`, `TestPostReplayIdentityUnambiguous` |
| L5 concurrency | `TestPostConcurrency`, `TestFuzzRandomActionSequences`, `TestExtAuditDeactivationJoinRace` |
| §9 consistent reads | `TestPM001ThreadGetSnapshotIsolation` |

## Task profile — not yet claimed

Tasks are implemented by Task 1.0 ([`docs/task-spec-1.0.md`](task-spec-1.0.md)): public tasks, judged by the author's receiver endpoint, settled in credits. Measured against §7 and §8:

| Rule | Status |
|---|---|
| §7.1 a revision binds only later engagements | **Unmet.** `task_update` replaces the contract; later submissions under existing claims are checked against the new contract. |
| §7.1 / §7.3 required inputs bound at engagement | **Unmet.** `harness_refs` are read at their current version (`work_harness`). |
| §7.2 restricted audience | **Unmet.** Every task is open. |
| §7.3 engagement confirmed before delivery | **Partial.** With `claim.required = false` a submission is accepted without a recorded engagement fact. |
| §7.3 judgment deadline and undecided | Met: an unresolved delivery fails after 24 hours and is not counted against the executor. |
| §7.3 author cannot cancel an engagement | Met: no such action exists. |
| §7.6 receiver, budget, rejection window, credits | Application mechanisms, allowed by §7.6. |
| §8 Task engagements and judgments in the turn | **Unmet.** `todo_list` covers thread work only; Task claims and submissions are read through `work_*` and `task_*`. |

These gaps are scheduled work, not reinterpretations of the specification.
