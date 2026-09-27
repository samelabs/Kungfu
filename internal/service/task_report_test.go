package service

// Report + retention tests (WO-6b) — real PostgreSQL, injectable
// clocks. Every test ends with task.CheckInvariants.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
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

func TestPurgeExpiredSnapshots(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	// a harness-bearing closed task and a harness-bearing open task
	pubSeedKungfu(t, pool, publisher, "harnpurge001", "Purge me")
	withHarness := func(x *task.Contract) { x.HarnessRefs = []string{"harnpurge001"} }
	closed := workOpenTask(t, pool, publisher, 1000, withHarness)
	if _, err := CloseTask(ctx, pool, publisher, closed.Code); err != nil {
		t.Fatalf("close: %v", err)
	}
	open := workOpenTask(t, pool, publisher, 1000, withHarness)

	snapshot := func(taskID int64) (harness string, sample string, requirements string, schemaPresent bool) {
		v, err := repository.FindTaskVersion(ctx, pool, taskID, 1)
		if err != nil || v == nil {
			t.Fatalf("version: %v", err)
		}
		var contract map[string]any
		if err := json.Unmarshal(v.Contract, &contract); err != nil {
			t.Fatalf("contract: %v", err)
		}
		smp, _ := json.Marshal(contract["sample"])
		req, _ := contract["requirements"].(string)
		_, schemaPresent = contract["output"].(map[string]any)["schema"]
		return string(v.Harness), string(smp), req, schemaPresent
	}

	// 29 days: nothing
	if s, err := PurgeExpired(ctx, pool, time.Now().Add(29*24*time.Hour), 500); err != nil || s != 0 {
		t.Fatalf("29d snapshots=%d err=%v", s, err)
	}
	// 31 days: the closed task's snapshot empties; the open one stays.
	// Closed tasks left by earlier tests in THIS package share the
	// cutoff, so assert >= 1 plus this task's exact outcome below.
	snapshots, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500)
	if err != nil || snapshots < 1 {
		t.Fatalf("31d purge (snapshots=%d err=%v)", snapshots, err)
	}
	harness, sample, requirements, schemaPresent := snapshot(closed.ID)
	if harness != "[]" {
		t.Fatalf("closed harness = %s, want []", harness)
	}
	if sample != "{}" {
		t.Fatalf("closed sample = %s, want {}", sample)
	}
	if requirements == "" { // the rest of the contract stays for audit
		t.Fatal("requirements dropped")
	}
	if !schemaPresent {
		t.Fatal("schema dropped")
	}
	openHarness, openSample, _, _ := snapshot(open.ID)
	if openHarness == "[]" || openSample == "{}" {
		t.Fatal("open task snapshot was purged")
	}
	// idempotent second pass
	if s, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500); err != nil || s != 0 {
		t.Fatalf("second pass (snapshots=%d err=%v)", s, err)
	}
	for _, id := range []int64{closed.ID, open.ID} {
		if err := task.CheckInvariants(ctx, pool, id); err != nil {
			t.Fatalf("CheckInvariants(%d): %v", id, err)
		}
	}
}
