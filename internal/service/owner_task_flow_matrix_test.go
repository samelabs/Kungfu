package service

// Task publishing flow alignment regression:
// - UpdateTaskBasics accepts pending and closed, rejects open.
// - Pending tasks can be opened (fundability rules apply).
// JS-side action matrix (pending=Edit+Open, open=Close+AddBudget,
// closed=Edit+Open+Refund-eligibility) is locked in
// internal/server/owner_task_flow_test.go against the real asset
// sources.

import (
	"context"
	"testing"

	"kungfu.md/internal/errors"
)

// TestUpdateTaskBasicsPendingAllowedClosedAllowedOpenRejected locks
// the backend edit matrix: pending/closed editable, open rejected.
func TestUpdateTaskBasicsPendingAllowedClosedAllowedOpenRejected(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	created, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput("https://example.com/hook"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	task := created["task"].(map[string]interface{})
	code := task["code"].(string)

	// pending: edit allowed
	newTitle := "pending edit"
	if _, err := UpdateTaskBasics(ctx, pool, botID, code, &OwnerTaskConfig{}, &UpdateTaskBasicsInput{Title: &newTitle}); err != nil {
		t.Fatalf("pending edit should be allowed: %v", err)
	}

	// pending -> open
	if _, err := SetTaskStatus(ctx, pool, botID, code, "open"); err != nil {
		t.Fatalf("open from pending: %v", err)
	}

	// open: edit rejected
	if _, err := UpdateTaskBasics(ctx, pool, botID, code, &OwnerTaskConfig{}, &UpdateTaskBasicsInput{Title: &newTitle}); err == nil {
		t.Fatal("open edit must be rejected")
	}

	// open -> closed; closed: edit allowed again
	if _, err := SetTaskStatus(ctx, pool, botID, code, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := UpdateTaskBasics(ctx, pool, botID, code, &OwnerTaskConfig{}, &UpdateTaskBasicsInput{Title: &newTitle}); err != nil {
		t.Fatalf("closed edit should be allowed: %v", err)
	}
}

// A task the platform closed (review_note set by the admin close) cannot
// be reopened by its owner; an owner-closed task still can.
func TestPlatformClosedTaskCannotBeReopenedByOwner(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	created, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput("https://example.com/hook"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := created["task"].(map[string]interface{})["code"].(string)
	if _, err := SetTaskStatus(ctx, pool, botID, code, "open"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := SetTaskStatus(ctx, pool, botID, code, "closed"); err != nil {
		t.Fatalf("owner close: %v", err)
	}
	if _, err := SetTaskStatus(ctx, pool, botID, code, "open"); err != nil {
		t.Fatalf("owner-closed task must reopen: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tb_tasks SET status='closed', closed_at=NOW(), review_note='spam', reviewed_at=NOW() WHERE code=$1`, code); err != nil {
		t.Fatalf("platform close: %v", err)
	}
	_, err = SetTaskStatus(ctx, pool, botID, code, "open")
	if ae, ok := errors.IsAppError(err); !ok || ae.Code != "TASK_CLOSED_BY_PLATFORM" {
		t.Fatalf("reopen after platform close: %v", err)
	}
}
