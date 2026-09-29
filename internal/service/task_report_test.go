package service

// Report + retention tests (WO-6b) — real PostgreSQL, injectable
// clocks. Every test ends with task.CheckInvariants.

import (
	"context"
	"kungfu.md/internal/task"
	"testing"
)

// -- work_report --

func TestReportTaskSuccessAndIdempotent(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := workOpenTask(t, pool, publisher, 1000, nil).Code

	first, err := ReportTask(ctx, pool, agent, code, "  violates boundaries: asks for credentials  ")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if first["status"] != "open" {
		t.Fatalf("status = %v", first["status"])
	}
	var reason string
	_ = pool.QueryRow(ctx, `SELECT reason FROM tb_task_report WHERE id=$1`, first["report_id"]).Scan(&reason)
	if reason != "violates boundaries: asks for credentials" {
		t.Fatalf("stored reason = %q", reason)
	}

	// a second open report on the same task returns the same id
	second, err := ReportTask(ctx, pool, agent, code, "another complaint")
	if err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if second["report_id"] != first["report_id"] {
		t.Fatalf("re-report created a new row: %v vs %v", second["report_id"], first["report_id"])
	}
	// reporting one's own task is allowed
	if _, err := ReportTask(ctx, pool, publisher, code, "self report"); err != nil {
		t.Fatalf("own-task report: %v", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestReportTaskGuards(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	draft := pubCreateForTest(t, pool, publisher, submitContract(), 1000)
	if _, err := ReportTask(ctx, pool, agent, draft, "x"); appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("draft: %v, want TASK_NOT_FOUND", err)
	}
	if _, err := ReportTask(ctx, pool, agent, "nope00000000", "x"); appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("missing: %v, want TASK_NOT_FOUND", err)
	}

	code := workOpenTask(t, pool, publisher, 1000, nil).Code
	if _, err := ReportTask(ctx, pool, agent, code, "   "); appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("blank reason: %v, want VALIDATION_FAILED", err)
	}
	_, err := ReportTask(ctx, pool, agent, code, longReason(2001))
	if appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("long reason: %v, want VALIDATION_FAILED", err)
	}
	if _, err := ReportTask(ctx, pool, agent, code, longReason(2000)); err != nil {
		t.Fatalf("max-length reason rejected: %v", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func longReason(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'r'
	}
	return string(b)
}

// -- §9 retention --
