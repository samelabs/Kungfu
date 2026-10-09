package service

// Task 1.1 §7.3 acceptance fact: a claim-less delivery is accepted on
// a recorded engagement — a claim row born used, in the submission's
// own transaction, binding the contract version and the harness
// revisions. The external interface, the ledger and the reservation
// rules are exactly what they were; these tests prove the fact exists
// and that nothing double-counts.

import (
	"context"
	"testing"
	"time"

	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// TestClaimlessSubmissionRecordsEngagementFact: the submission's
// claim_id points at a born-used claim that binds the version, the
// price and the harness revisions; the submission row itself carries
// the same harness snapshot; invariants hold.
func TestClaimlessSubmissionRecordsEngagementFact(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	memCode := pinSeedKungfu(t, pool, publisher, "harness body")
	c := submitContract() // claim not required
	c.HarnessRefs = []string{memCode}
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}

	view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "fact-1", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if view.State != task.SubSettled {
		t.Fatalf("state = %s, want settled (test receiver accepts)", view.State)
	}

	// the submission points at its engagement fact
	sub, err := repository.FindSubmissionByID(ctx, pool, view.SubmissionID.Int64())
	if err != nil || sub == nil {
		t.Fatalf("reload submission: %v", err)
	}
	if sub.ClaimID == nil {
		t.Fatal("claim-less submission has no engagement fact (claim_id is NULL)")
	}
	fact, err := repository.FindClaimByID(ctx, pool, *sub.ClaimID)
	if err != nil || fact == nil {
		t.Fatalf("load fact: %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if fact.TaskID != tr.ID || fact.AgentID != agent || fact.Status != task.ClaimUsed {
		t.Fatalf("fact = %+v, want used claim of agent on this task", fact)
	}
	if fact.Amount != 5 || fact.ContractVersion != tr.ContractVersion {
		t.Fatalf("fact amount/version = %d/%d, want 5/%d", fact.Amount, fact.ContractVersion, tr.ContractVersion)
	}
	// the fact binds the harness revisions (the engagement's inputs)
	if pinned, err := repository.ClaimHarnessRevision(ctx, pool, fact.ClaimID, repositoryID(t, pool, memCode)); err != nil || pinned == nil || *pinned != 1 {
		t.Fatalf("fact harness pin = %v err=%v, want revision 1", pinned, err)
	}
	// and the submission row carries the same snapshot
	var harness []byte
	if err := pool.QueryRow(ctx, `SELECT harness_json FROM tb_task_submission WHERE submission_id = $1`,
		sub.SubmissionID).Scan(&harness); err != nil {
		t.Fatalf("harness_json: %v", err)
	}
	if harness == nil {
		t.Fatal("claim-less submission did not record its harness snapshot")
	}

	// the fact never shows in the turn (todo carries active claims only)
	res, err := TodoList(ctx, pool, agent, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	if rows := taskTodoRows(t, res); len(rows) != 0 {
		t.Fatalf("a born-used fact must not appear as an obligation: %v", rows)
	}

	claimTaskReserved(t, pool, code) // + invariants
}

// TestEngagementFactLedgerZeroDrift: a claim-less delivery (with its
// acceptance fact) and a claim-based delivery on identical tasks move
// exactly the same money — same task counters, same ledger rows, same
// balances. The fact is bookkeeping, not a second reservation.
func TestEngagementFactLedgerZeroDrift(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100_000)
	claimAgent := pubSeedBot(t, pool, 0)
	directAgent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// task A: claim required — the engagement is the claim
	codeA := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim, err := ClaimTask(ctx, pool, claimAgent, codeA, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// task B: claim not required — the engagement is the acceptance fact
	codeB := submitOpenedTask(t, pool, publisher, 1000, nil)

	if _, err := SubmitWork(ctx, pool, claimAgent, SubmitInput{
		Code: codeA, RequestKey: "drift-a", Payload: []byte(submitPayloadOK), ClaimID: &claim.ClaimID,
	}, testAgentRefKey, time.Now()); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	if _, err := SubmitWork(ctx, pool, directAgent, SubmitInput{
		Code: codeB, RequestKey: "drift-b", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now()); err != nil {
		t.Fatalf("submit B: %v", err)
	}

	taskOf := func(code string) *repository.TaskRow {
		tr, err := repository.FindTaskByCode(ctx, pool, code)
		if err != nil || tr == nil {
			t.Fatalf("load %s: %v", code, err)
		}
		return tr
	}
	a, b := taskOf(codeA), taskOf(codeB)
	avail := func(tr *repository.TaskRow) int64 {
		return tr.BudgetLocked - tr.Settled - tr.Reserved - tr.Refunded
	}
	if a.Settled != b.Settled || a.Reserved != b.Reserved || a.BudgetLocked != b.BudgetLocked ||
		a.Refunded != b.Refunded || avail(a) != avail(b) {
		t.Fatalf("task counters diverged: claim-based %+v vs claim-less %+v", a, b)
	}
	if a.Settled != 5 || a.Reserved != 0 {
		t.Fatalf("settled/reserved = %d/%d, want 5/0 on both tasks", a.Settled, a.Reserved)
	}
	// one budget lock per task (§12 sign table: −budget on the
	// publisher), nothing else; each executor earns exactly one price
	if la := ledgerSum(t, pool, publisher, "lock_task"); la != -2000 {
		t.Fatalf("publisher lock_task = %d, want -2000 (two tasks, one budget each)", la)
	}
	if fa := ledgerSum(t, pool, publisher, "fund_task"); fa != 0 {
		t.Fatalf("publisher fund_task = %d, want 0", fa)
	}
	if ea, eb := ledgerSum(t, pool, claimAgent, "earn_task"), ledgerSum(t, pool, directAgent, "earn_task"); ea != eb || ea != 5 {
		t.Fatalf("executor earnings diverged: %d vs %d (want 5 each)", ea, eb)
	}
	// §10.3 audit: exactly one earn_task row per settled submission
	for _, code := range []string{codeA, codeB} {
		if err := task.CheckInvariants(ctx, pool, taskOf(code).ID); err != nil {
			t.Fatalf("CheckInvariants %s: %v", code, err)
		}
	}
}

// TestClaimlessIdempotentTwinCreatesOneFact: replaying the same
// (task, request_key, payload) returns the same submission and never
// mints a second engagement fact or a second reservation.
func TestClaimlessIdempotentTwinCreatesOneFact(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	in := SubmitInput{Code: code, RequestKey: "twin-1", Payload: []byte(submitPayloadOK)}
	first, err := SubmitWork(ctx, pool, agent, in, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := SubmitWork(ctx, pool, agent, in, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first.SubmissionID != second.SubmissionID {
		t.Fatalf("replay created a second submission: %d vs %d", first.SubmissionID, second.SubmissionID)
	}

	var facts int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_task_claim WHERE task_id = (SELECT id FROM tb_task WHERE code = $1)
		  AND agent_id = $2`, code, agent).Scan(&facts); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if facts != 1 {
		t.Fatalf("engagement facts = %d, want exactly 1", facts)
	}
	claimTaskReserved(t, pool, code) // + invariants (reserved 0, one earn row)
}
