package repository

// Task 1.0 (WO-1) repository integration tests against a real
// PostgreSQL (KF_TEST_DATABASE_URL, migrations applied). The five
// scenarios required by the work order:
//
//	a) lock → reserve → settle, then CheckInvariants passes
//	b) reserve → release: reserved falls back, invariants pass
//	c) a second (agent, task, request_key) insert violates the unique key
//	d) UPDATE / DELETE on tb_task_submission_event fail (append-only)
//	e) a write violating budget_closed is rejected by the database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/task"
)

func taskV1Pool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func taskV1SeedBot(t *testing.T, pool *pg.Pool, balance int64) int64 {
	t.Helper()
	name := fmt.Sprintf("wo1_%d_%d", time.Now().UnixNano(), balance)
	digest := sha256.Sum256([]byte(name))
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'w1w1', 'x', $3) RETURNING id`,
		name, digest[:], balance).Scan(&id)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return id
}

func taskV1SeededTask(t *testing.T, pool *pg.Pool, publisherID int64, lock int64) *TaskRow {
	t.Helper()
	ctx := context.Background()
	code := fmt.Sprintf("wo1-%d", time.Now().UnixNano())
	taskID, err := InsertTask(ctx, pool, NewTaskRow{Code: code, PublisherID: publisherID, Contract: []byte(`{}`)})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if lock > 0 {
		tx, err := pool.TxBegin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = pg.Rollback(tx) }()
		if err := LockTaskBudget(ctx, pool, tx, taskID, publisherID, lock); err != nil {
			t.Fatalf("lock budget: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	tr, err := FindTaskByID(ctx, pool, taskID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	return tr
}

func taskV1Hash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// a) lock → reserve → settle; §10 invariants hold afterwards.
func TestTaskV1LockReserveSettleInvariants(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()
	publisher := taskV1SeedBot(t, pool, 10_000)
	agent := taskV1SeedBot(t, pool, 0)
	tr := taskV1SeededTask(t, pool, publisher, 1000)

	if err := ApplyTaskStatus(ctx, pool, tr.ID, task.TaskPaused, task.EventOpen, nil); err != nil {
		t.Fatalf("open task: %v", err)
	}

	// accept: create the delivering submission + reserve, one transaction
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	payload := []byte(`{"result":"ok"}`)
	subID, err := InsertSubmission(ctx, tx, NewSubmissionRow{
		TaskID: tr.ID, AgentID: agent,
		RequestKey: "a-key", Payload: payload,
		PayloadHash: taskV1Hash(payload), Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	if err := ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// settle: 2xx outcome — state write and money move in one transaction
	tx, err = pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if err := SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliver2XX, nil); err != nil {
		t.Fatalf("settle state: %v", err)
	}
	if err := SettleTaskSubmission(ctx, pool, tx, tr.ID, subID, agent, 5); err != nil {
		t.Fatalf("settle money: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := FindTaskByID(ctx, pool, tr.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.BudgetLocked != 1000 || after.Settled != 5 || after.Reserved != 0 || after.Refunded != 0 {
		t.Fatalf("counters = locked %d settled %d reserved %d refunded %d, want 1000/5/0/0",
			after.BudgetLocked, after.Settled, after.Reserved, after.Refunded)
	}
	var agentBalance int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, agent).Scan(&agentBalance); err != nil {
		t.Fatalf("agent balance: %v", err)
	}
	if agentBalance != 5 {
		t.Fatalf("agent balance = %d, want 5", agentBalance)
	}
	var settles int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type = 'earn_task' AND ref_type = 'task_submission' AND ref_id = $1`,
		fmt.Sprint(subID)).Scan(&settles); err != nil {
		t.Fatalf("count settlements: %v", err)
	}
	if settles != 1 {
		t.Fatalf("settlement records = %d, want 1", settles)
	}

	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// b) claim reserve → release: reserved falls back and invariants hold.
func TestTaskV1ReserveReleaseInvariants(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()
	publisher := taskV1SeedBot(t, pool, 10_000)
	agent := taskV1SeedBot(t, pool, 0)
	tr := taskV1SeededTask(t, pool, publisher, 1000)

	now := time.Now()
	// work_claim: active claim + reservation, one transaction
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	claimID, err := InsertClaim(ctx, tx, NewClaimRow{
		TaskID: tr.ID, AgentID: agent,
		ExpiresAt: now.Add(30 * time.Minute), Deadline: now.Add(2 * time.Hour), Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	if err := ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	mid, err := FindTaskByID(ctx, pool, tr.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if mid.Reserved != 5 {
		t.Fatalf("reserved after claim = %d, want 5", mid.Reserved)
	}

	// work_release: claim → released + reservation released
	tx, err = pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if err := ApplyClaimStatus(ctx, tx, claimID, task.ClaimActive, task.EventClaimRelease); err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if err := ReleaseTaskReservation(ctx, tx, tr.ID, 5); err != nil {
		t.Fatalf("release reservation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := FindTaskByID(ctx, pool, tr.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Reserved != 0 {
		t.Fatalf("reserved after release = %d, want 0", after.Reserved)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// c) the same (agent, task, request_key) insert violates the unique key.
func TestTaskV1SubmissionUniqueIdentity(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()
	publisher := taskV1SeedBot(t, pool, 10_000)
	agent := taskV1SeedBot(t, pool, 0)
	tr := taskV1SeededTask(t, pool, publisher, 1000)

	in := NewSubmissionRow{
		TaskID: tr.ID, AgentID: agent, RequestKey: "dup-key",
		Payload: []byte(`{"n":1}`), Amount: 5,
	}
	in.PayloadHash = taskV1Hash(in.Payload)

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := InsertSubmission(ctx, tx, in); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// different payload, same identity → IDEMPOTENCY_CONFLICT territory;
	// the database must reject the second row either way.
	in.Payload = []byte(`{"n":2}`)
	in.PayloadHash = taskV1Hash(in.Payload)
	tx, err = pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	_, err = InsertSubmission(ctx, tx, in)
	if err == nil {
		t.Fatal("second insert with the same (agent, task, request_key) unexpectedly succeeded")
	}
	if !IsUniqueViolation(err) {
		t.Fatalf("second insert: want unique violation, got %v", err)
	}
}

// d) tb_task_submission_event is append-only: UPDATE and DELETE fail.
func TestTaskV1SubmissionEventAppendOnly(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()
	publisher := taskV1SeedBot(t, pool, 10_000)
	agent := taskV1SeedBot(t, pool, 0)
	tr := taskV1SeededTask(t, pool, publisher, 1000)

	payload := []byte(`{"x":1}`)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	subID, err := InsertSubmission(ctx, tx, NewSubmissionRow{
		TaskID: tr.ID, AgentID: agent, RequestKey: "ev-key",
		Payload: payload, PayloadHash: taskV1Hash(payload), Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE tb_task_submission_event SET to_state = 'settled'
		WHERE submission_id = $1`, subID); err == nil {
		t.Fatal("UPDATE on tb_task_submission_event unexpectedly succeeded")
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM tb_task_submission_event WHERE submission_id = $1`, subID); err == nil {
		t.Fatal("DELETE on tb_task_submission_event unexpectedly succeeded")
	}

	// The sanctioned path appends: two transitions, three events, the
	// last to_state equals the current state.
	tx, err = pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if err := SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventTimeout, nil); err != nil {
		t.Fatalf("delivering → uncertain: %v", err)
	}
	if err := SetSubmissionState(ctx, tx, subID, task.SubUncertain, task.EventUnresolved, &SetSubmissionStateOpts{
		Failure: strPtr("DELIVERY_UNRESOLVED"),
	}); err != nil {
		t.Fatalf("uncertain → failed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, err := ListSubmissionEvents(ctx, pool, subID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	last := events[len(events)-1]
	if last.ToState != task.SubFailed || last.FromState == nil || *last.FromState != task.SubUncertain {
		t.Fatalf("last event = %+v, want uncertain → failed", last)
	}
	sub, err := FindSubmissionByID(ctx, pool, subID)
	if err != nil {
		t.Fatalf("reload submission: %v", err)
	}
	if sub.State != task.SubFailed || sub.Failure == nil || *sub.Failure != "DELIVERY_UNRESOLVED" {
		t.Fatalf("submission = state %s failure %v, want failed/DELIVERY_UNRESOLVED", sub.State, sub.Failure)
	}
}

// e) a write breaking budget_locked ≥ settled + reserved + refunded
// is rejected by the database CHECK, whatever the write path.
func TestTaskV1BudgetClosedConstraintRejected(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()
	publisher := taskV1SeedBot(t, pool, 10_000)
	tr := taskV1SeededTask(t, pool, publisher, 10) // budget_locked = 10

	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET settled = 11 WHERE id = $1`, tr.ID); err == nil {
		t.Fatal("settled > budget_locked unexpectedly succeeded")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET reserved = 11 WHERE id = $1`, tr.ID); err == nil {
		t.Fatal("reserved > budget_locked unexpectedly succeeded")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET refunded = 1, settled = 10 WHERE id = $1`, tr.ID); err == nil {
		t.Fatal("settled + refunded > budget_locked unexpectedly succeeded")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET reserved = -1 WHERE id = $1`, tr.ID); err == nil {
		t.Fatal("negative reserved unexpectedly succeeded")
	}

	// The counter primitives enforce the same bound transactionally:
	// reserving beyond available fails without touching the row.
	if err := ReserveTaskAmount(ctx, pool, tr.ID, 11); !errors.Is(err, ErrNoAvailableBudget) {
		t.Fatalf("reserve beyond available: want ErrNoAvailableBudget, got %v", err)
	}
	after, err := FindTaskByID(ctx, pool, tr.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Reserved != 0 {
		t.Fatalf("reserved = %d after failed reserve, want 0", after.Reserved)
	}
}

func strPtr(s string) *string { return &s }

// TestGetTaskStatsBatchMatchesSingular: the batch computation returns
// exactly the same numbers as calling GetTaskStats per task, for
// tasks with terminals, tasks with none, and absent task ids.
func TestGetTaskStatsBatchMatchesSingular(t *testing.T) {
	pool := taskV1Pool(t)
	ctx := context.Background()

	// seed two tasks with different terminal mixes
	seedTask := func(code string, n int) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO tb_task (code, publisher_id, status, budget_locked, contract)
			 VALUES ($1, 1, 'open', 1, 100000, '{}') RETURNING id`, code).Scan(&id); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO tb_task_version (task_id, contract, harness)
			 VALUES ($1, 1, '{}', '[]')`, id); err != nil {
			t.Fatalf("seed version: %v", err)
		}
		for i := 0; i < n; i++ {
			var sid int64
			state := "settled"
			if i%2 == 1 {
				state = "rejected"
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO tb_task_submission (task_id, agent_id, request_key, payload_hash, amount, state)
				 VALUES ($1, 1, 2, $2, 'h', 5, 'delivering') RETURNING submission_id`, id,
				fmt.Sprintf("%s-%d", code, i)).Scan(&sid); err != nil {
				t.Fatalf("seed submission: %v", err)
			}
			if _, err := pool.Exec(ctx,
				`INSERT INTO tb_task_submission_event (submission_id, seq, to_state, cause)
				 VALUES ($1, 1, 'delivering', 'submit')`, sid); err != nil {
				t.Fatalf("seed event1: %v", err)
			}
			cause := "deliver_2xx"
			if state == "rejected" {
				cause = "deliver_4xx"
			}
			if _, err := pool.Exec(ctx,
				`INSERT INTO tb_task_submission_event (submission_id, seq, to_state, cause)
				 VALUES ($1, 2, $2, $3)`, sid, state, cause); err != nil {
				t.Fatalf("seed event2: %v", err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE tb_task_submission SET state = $2, payload = NULL WHERE submission_id = $1`, sid, state); err != nil {
				t.Fatalf("settle: %v", err)
			}
		}
		return id
	}

	idA := seedTask(fmt.Sprintf("statbatch%06d", time.Now().UnixNano()%1000000), 4)
	idB := seedTask(fmt.Sprintf("statbatch2%06d", time.Now().UnixNano()%1000000), 0) // no terminals

	since := time.Now().Add(-30 * 24 * time.Hour)
	ids := []int64{idA, idB}
	batch, err := GetTaskStatsBatch(ctx, pool, ids, since)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(batch) == 0 {
		t.Fatal("batch returned nothing")
	}
	for _, id := range ids {
		single, err := GetTaskStats(ctx, pool, id, since)
		if err != nil {
			t.Fatalf("singular %d: %v", id, err)
		}
		got, ok := batch[id]
		if !ok {
			if single.TerminalTotal == 0 {
				continue // no terminals → absent from both
			}
			t.Fatalf("batch missing task %d (has %d terminals)", id, single.TerminalTotal)
		}
		if got.Settled != single.Settled || got.Rejected != single.Rejected ||
			got.Failed != single.Failed || got.TerminalTotal != single.TerminalTotal {
			t.Fatalf("task %d: batch %+v != singular %+v", id, got, single)
		}
		if (got.MedianReplySeconds == nil) != (single.MedianReplySeconds == nil) {
			t.Fatalf("task %d: median nil mismatch %v vs %v", id, got.MedianReplySeconds, single.MedianReplySeconds)
		}
		if got.MedianReplySeconds != nil && single.MedianReplySeconds != nil &&
			*got.MedianReplySeconds != *single.MedianReplySeconds {
			t.Fatalf("task %d: median %v != %v", id, *got.MedianReplySeconds, *single.MedianReplySeconds)
		}
	}
}
