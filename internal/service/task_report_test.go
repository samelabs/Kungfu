package service

// Report + retention tests (WO-6b) — real PostgreSQL, injectable
// clocks. Every test ends with task.CheckInvariants.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"kungfu.md/internal/pg"
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

// seedTerminal seeds one TERMINAL submission carrying payload, hash,
// verdict and events.
func seedTerminal(t *testing.T, pool *pg.Pool, agent int64, code string, state string, verdict []byte) int64 {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, Version: 1, AgentID: agent,
		RequestKey:  fmt.Sprintf("pu-%s-%d", code, time.Now().UnixNano()),
		Payload:     []byte(submitPayloadOK),
		PayloadHash: task.PayloadHash([]byte(submitPayloadOK)), Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	var event string
	var opts *repository.SetSubmissionStateOpts
	switch state {
	case task.SubSettled:
		event = task.EventDeliver2XX
		opts = &repository.SetSubmissionStateOpts{Verdict: verdict}
	case task.SubRejected:
		event = task.EventDeliver4XX
		opts = &repository.SetSubmissionStateOpts{Verdict: verdict}
	default: // failed
		event = task.EventDeliveryFailed
		reason := "RECEIVER_FAULT"
		opts = &repository.SetSubmissionStateOpts{Failure: &reason}
	}
	if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, event, opts); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return subID
}

func TestPurgeExpiredPayloads(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := workOpenTask(t, pool, publisher, 1000, nil).Code
	old := seedTerminal(t, pool, agent, code, task.SubRejected,
		[]byte(`{"accepted":false,"criteria":["C1"],"reason":"r","retryable":true,"source":"receiver"}`))

	// a live (delivering) submission must never be purged
	tx, _ := pool.TxBegin(ctx)
	live, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: mustTaskID(t, pool, code), Version: 1, AgentID: agent,
		RequestKey: fmt.Sprintf("live-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)), Amount: 5,
	})
	if err != nil {
		t.Fatalf("live insert: %v", err)
	}
	if err := repository.ReserveTaskAmount(ctx, tx, mustTaskID(t, pool, code), 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_ = tx.Commit(ctx)

	subBefore, _ := repository.FindSubmissionByID(ctx, pool, old)
	eventsBefore, _ := repository.ListSubmissionEvents(ctx, pool, old)

	// 29 days: nothing purged
	if p, s, err := PurgeExpired(ctx, pool, time.Now().Add(29*24*time.Hour), 500); err != nil || p != 0 || s != 0 {
		t.Fatalf("29d purge (payloads=%d snapshots=%d err=%v)", p, s, err)
	}

	// 31 days: the terminal payload goes; hash, verdict, events stay
	payloads, _, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500)
	if err != nil || payloads < 1 {
		t.Fatalf("31d purge (payloads=%d err=%v)", payloads, err)
	}
	subAfter, _ := repository.FindSubmissionByID(ctx, pool, old)
	if subAfter.Payload != nil {
		t.Fatalf("payload not purged: %s", subAfter.Payload)
	}
	if subAfter.PayloadHash != subBefore.PayloadHash {
		t.Fatal("payload_hash changed")
	}
	if string(subAfter.Verdict) != string(subBefore.Verdict) {
		t.Fatalf("verdict changed: %s", subAfter.Verdict)
	}
	eventsAfter, _ := repository.ListSubmissionEvents(ctx, pool, old)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("events = %d, want unchanged %d", len(eventsAfter), len(eventsBefore))
	}

	liveSub, _ := repository.FindSubmissionByID(ctx, pool, live)
	if liveSub.Payload == nil {
		t.Fatal("non-terminal payload purged")
	}
	// idempotent second pass
	if p, _, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500); err != nil || p != 0 {
		t.Fatalf("second pass (payloads=%d err=%v)", p, err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

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

	snapshot := func(taskID int64) (harness string, examples int, criteria []string, schemaPresent bool) {
		v, err := repository.FindTaskVersion(ctx, pool, taskID, 1)
		if err != nil || v == nil {
			t.Fatalf("version: %v", err)
		}
		var contract map[string]any
		if err := json.Unmarshal(v.Contract, &contract); err != nil {
			t.Fatalf("contract: %v", err)
		}
		ex, _ := contract["examples"].([]any)
		crit := contract["acceptance"].(map[string]any)["criteria"].([]any)
		ids := make([]string, 0, len(crit))
		for _, cc := range crit {
			ids = append(ids, cc.(map[string]any)["id"].(string))
		}
		_, schemaPresent = contract["output"].(map[string]any)["schema"]
		return string(v.Harness), len(ex), ids, schemaPresent
	}

	// 29 days: nothing
	if _, s, err := PurgeExpired(ctx, pool, time.Now().Add(29*24*time.Hour), 500); err != nil || s != 0 {
		t.Fatalf("29d snapshots=%d err=%v", s, err)
	}
	// 31 days: the closed task's snapshot empties; the open one stays
	_, snapshots, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500)
	if err != nil || snapshots != 1 {
		t.Fatalf("31d purge (snapshots=%d err=%v)", snapshots, err)
	}
	harness, examples, criteria, schemaPresent := snapshot(closed.ID)
	if harness != "[]" {
		t.Fatalf("closed harness = %s, want []", harness)
	}
	if examples != 0 {
		t.Fatalf("closed examples len = %d, want 0", examples)
	}
	if len(criteria) != 2 { // C1, C2 stay for audit
		t.Fatalf("criteria = %v, want kept", criteria)
	}
	if !schemaPresent {
		t.Fatal("schema dropped")
	}
	openHarness, openExamples, _, _ := snapshot(open.ID)
	if openHarness == "[]" || openExamples == 0 {
		t.Fatal("open task snapshot was purged")
	}
	// idempotent second pass
	if _, s, err := PurgeExpired(ctx, pool, time.Now().Add(31*24*time.Hour), 500); err != nil || s != 0 {
		t.Fatalf("second pass (snapshots=%d err=%v)", s, err)
	}
	for _, id := range []int64{closed.ID, open.ID} {
		if err := task.CheckInvariants(ctx, pool, id); err != nil {
			t.Fatalf("CheckInvariants(%d): %v", id, err)
		}
	}
}
