package service

// Platform governance tests (WO-8b) — real PostgreSQL. PlatformCloseTask
// and ResolveReport reuse the publisher close's claim/reservation core
// (§4: active claims keep their reservation until expiry, in-flight
// submissions complete). Every test ends with task.CheckInvariants.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// govSeedAdmin seeds an admin row (the audit trail's actor); ids are
// unique per run and the gate database is fresh, so no cleanup.
func govSeedAdmin(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	var id int64
	name := fmt.Sprintf("govadmin_%d", time.Now().UnixNano())
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1, 'Governance', 'x') RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return id
}

// auditCount counts audit rows for one action + target id.
func auditCount(t *testing.T, pool *pg.Pool, action, targetID string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action = $1 AND target_id = $2`,
		action, targetID).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func TestPlatformCloseTaskClosesDraftOpenPaused(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	admin := govSeedAdmin(t, pool)
	ctx := context.Background()

	draft := pubCreateForTest(t, pool, publisher, submitContract(), 1000)
	opened := workOpenTask(t, pool, publisher, 1000, nil)
	paused := workOpenTask(t, pool, publisher, 1000, nil)
	if _, err := PauseTask(ctx, pool, publisher, paused.Code); err != nil {
		t.Fatalf("pause: %v", err)
	}

	for _, code := range []string{draft, opened.Code, paused.Code} {
		view, err := PlatformCloseTask(ctx, pool, admin, code, "off-spec task")
		if err != nil {
			t.Fatalf("platform close %s: %v", code, err)
		}
		if view["status"] != task.TaskClosed || view["closed_reason"] != "off-spec task" {
			t.Fatalf("%s: view = %v", code, view)
		}
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task
		WHERE publisher_id = $1 AND status = 'closed' AND closed_reason = 'off-spec task'`,
		publisher).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("closed with reason = %d, want 3", n)
	}
}

func TestPlatformCloseTaskKeepsClaimUntilExpiry(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	admin := govSeedAdmin(t, pool)
	ctx := context.Background()

	opened := workOpenTask(t, pool, publisher, 1000, nil)
	if _, err := ClaimTask(ctx, pool, agent, opened.Code, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before, _ := repository.FindTaskByCode(ctx, pool, opened.Code)
	if before.Reserved != 5 {
		t.Fatalf("reserved before close = %d, want the claimed price 5", before.Reserved)
	}

	if _, err := PlatformCloseTask(ctx, pool, admin, opened.Code, "governance"); err != nil {
		t.Fatalf("platform close: %v", err)
	}

	// §4 close (and therefore 平台关闭): the active claim keeps its
	// reservation and may still submit until expiry — nothing is
	// released by the close itself (same handling as CloseTask).
	after, _ := repository.FindTaskByCode(ctx, pool, opened.Code)
	if after.Status != task.TaskClosed {
		t.Fatalf("status = %s, want closed", after.Status)
	}
	if after.Reserved != before.Reserved {
		t.Fatalf("reserved after close = %d, want unchanged %d", after.Reserved, before.Reserved)
	}
	var claimStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM tb_task_claim WHERE task_id = $1`,
		after.ID).Scan(&claimStatus); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimStatus != task.ClaimActive {
		t.Fatalf("claim status = %s, want active", claimStatus)
	}

	// the reservation drains through the claim's own expiry path
	// (§5.2): the same task → claim lock order and event as the
	// reclaimer, applied to THIS claim only so the test stays
	// hermetic under the shared test database
	var claimID int64
	if err := pool.QueryRow(ctx, `SELECT claim_id FROM tb_task_claim
		WHERE task_id = $1 AND status = 'active'`, after.ID).Scan(&claimID); err != nil {
		t.Fatalf("claim id: %v", err)
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := repository.FindTaskByIDForUpdate(ctx, tx, after.ID); err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("task lock: %v", err)
	}
	claim, err := repository.FindClaimByIDForUpdate(ctx, tx, claimID)
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("claim lock: %v", err)
	}
	if err := repository.ApplyClaimStatus(ctx, tx, claimID, task.ClaimActive, task.EventClaimExpire); err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("expire: %v", err)
	}
	if err := repository.ReleaseTaskReservation(ctx, tx, after.ID, claim.Amount); err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("release: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	drained, _ := repository.FindTaskByCode(ctx, pool, opened.Code)
	if drained.Reserved != 0 {
		t.Fatalf("reserved after claim expiry = %d, want 0", drained.Reserved)
	}
	if err := task.CheckInvariants(ctx, pool, after.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestPlatformCloseTaskGuards(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	admin := govSeedAdmin(t, pool)
	ctx := context.Background()

	opened := workOpenTask(t, pool, publisher, 1000, nil)

	if _, err := PlatformCloseTask(ctx, pool, admin, opened.Code, "   "); appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("blank reason: %v, want VALIDATION_FAILED", err)
	}
	if _, err := PlatformCloseTask(ctx, pool, admin, opened.Code, strings.Repeat("r", 501)); appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("501-rune reason: %v, want VALIDATION_FAILED", err)
	}
	// 500 runes pass and trim-surrounded reasons are stored trimmed
	if _, err := PlatformCloseTask(ctx, pool, admin, opened.Code, "  "+strings.Repeat("r", 500)+"  "); err != nil {
		t.Fatalf("500-rune reason rejected: %v", err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT closed_reason FROM tb_task WHERE code = $1`, opened.Code).Scan(&reason); err != nil {
		t.Fatalf("reason: %v", err)
	}
	if len(reason) != 500 {
		t.Fatalf("stored reason length = %d, want 500 (trimmed)", len(reason))
	}

	// the terminal status rejects every further close
	if _, err := PlatformCloseTask(ctx, pool, admin, opened.Code, "again"); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("closed task: %v, want INVALID_STATE", err)
	}
	if _, err := PlatformCloseTask(ctx, pool, admin, "nope00000000", "x"); appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("missing task: %v, want TASK_NOT_FOUND", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, opened.Code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestResolveReportDismiss(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	reporter := pubSeedBot(t, pool, 0)
	admin := govSeedAdmin(t, pool)
	ctx := context.Background()

	code := workOpenTask(t, pool, publisher, 1000, nil).Code
	rep, err := ReportTask(ctx, pool, reporter, code, "violates boundaries")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	reportID := rep["report_id"].(WireID).Int64()

	out, err := ResolveReport(ctx, pool, admin, reportID, "dismiss")
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if out["status"] != "dismissed" {
		t.Fatalf("out = %v", out)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tb_task_report WHERE id = $1`, reportID).Scan(&status); err != nil {
		t.Fatalf("report: %v", err)
	}
	if status != "dismissed" {
		t.Fatalf("report status = %s, want dismissed", status)
	}
	if n := auditCount(t, pool, "report.dismiss", fmt.Sprint(reportID)); n != 1 {
		t.Fatalf("dismiss audit rows = %d, want 1", n)
	}

	// a second resolution is rejected — the report is no longer open
	if _, err := ResolveReport(ctx, pool, admin, reportID, "dismiss"); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("re-dismiss: %v, want INVALID_STATE", err)
	}
	// unknown action and missing report
	if _, err := ResolveReport(ctx, pool, admin, reportID, "burn"); appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("unknown action: %v, want VALIDATION_FAILED", err)
	}
	if _, err := ResolveReport(ctx, pool, admin, 999999999, "dismiss"); appErrOf(t, err).Code != "NOT_FOUND" {
		t.Fatalf("missing report: %v, want NOT_FOUND", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestResolveReportCloseActionsAllOpenReports(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	reporterA := pubSeedBot(t, pool, 0)
	reporterB := pubSeedBot(t, pool, 0)
	admin := govSeedAdmin(t, pool)
	ctx := context.Background()

	opened := workOpenTask(t, pool, publisher, 1000, nil)
	repA, err := ReportTask(ctx, pool, reporterA, opened.Code, "malicious rejection")
	if err != nil {
		t.Fatalf("report A: %v", err)
	}
	reportA := repA["report_id"].(WireID).Int64()
	// a second open report on the same task and one already dismissed
	repB, err := ReportTask(ctx, pool, reporterB, opened.Code, "boundary violation")
	if err != nil {
		t.Fatalf("report B: %v", err)
	}
	reportB := repB["report_id"].(WireID).Int64()
	if _, err := ResolveReport(ctx, pool, admin, reportB, "dismiss"); err != nil {
		t.Fatalf("pre-dismiss B: %v", err)
	}

	out, err := ResolveReport(ctx, pool, admin, reportA, "close")
	if err != nil {
		t.Fatalf("resolve close: %v", err)
	}
	if out["status"] != "actioned" {
		t.Fatalf("out = %v", out)
	}

	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, closed_reason FROM tb_task WHERE code = $1`,
		opened.Code).Scan(&status, &reason); err != nil {
		t.Fatalf("task: %v", err)
	}
	if status != task.TaskClosed {
		t.Fatalf("task status = %s, want closed", status)
	}
	if reason != fmt.Sprintf("Closed by platform governance (report #%d)", reportA) {
		t.Fatalf("closed_reason = %q", reason)
	}
	var nActioned int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task_report WHERE task_id = $1 AND status = 'actioned'`,
		opened.ID).Scan(&nActioned); err != nil {
		t.Fatalf("reports: %v", err)
	}
	if nActioned != 1 { // report A; B was already dismissed and keeps that status
		t.Fatalf("actioned reports = %d, want 1", nActioned)
	}
	if n := auditCount(t, pool, "report.close", fmt.Sprint(reportA)); n != 1 {
		t.Fatalf("report.close audit rows = %d, want 1", n)
	}
	if n := auditCount(t, pool, "task.platform_close", opened.Code); n != 1 {
		t.Fatalf("task.platform_close audit rows = %d, want 1", n)
	}
	if err := task.CheckInvariants(ctx, pool, opened.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}
