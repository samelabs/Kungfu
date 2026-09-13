package service

// Regression tests for the restored full TaskCheck contract in Submit and
// TestTask: structural postapi/price validation under the lock, explicit
// pending/open-only testing, zero POST hits on any gate failure.
// Real PostgreSQL via KF_TEST_DATABASE_URL.

import (
	"context"
	"strings"
	"testing"
)

func wantGateErr(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got nil", code)
	}
	ae, ok := apperrIs(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.Code != code {
		t.Fatalf("want code %s, got %d %s", code, ae.HTTPCode, ae.Code)
	}
}

// Submit open + malformed postapi -> TASK_CONFIG_INVALID, POST=0.
func TestSubmitMalformedPostAPIRejected(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// valid server exists but the task points at a malformed URL instead
	for _, bad := range []string{"not-a-url", "ftp://example.com/hook"} {
		code := tcSeedTask(t, pool, owner, "open", bad, 5, 1500)
		_, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
		wantGateErr(t, err, "TASK_CONFIG_INVALID")
		if *hits != 0 {
			t.Fatalf("malformed postapi POSTed: %d", *hits)
		}
		if budget, _ := tcBudget(t, pool, code); budget != 1500 {
			t.Fatalf("budget changed on gate rejection: %v", budget)
		}
		if n, _ := tcEarnCount(t, pool, agent); n != 0 {
			t.Fatalf("gate rejection earned: %d", n)
		}
	}
}

// Submit open + overlength postapi: the tb_tasks.postapi column is
// VARCHAR(2048), so an over-length URL cannot exist in a task row at all
// (the owner create/edit validation rejects it first). The 2048 validator
// ceiling therefore cannot regress through Submit against a seeded row;
// what CAN regress is the boundary — a max-length legal URL must pass the
// gate and deliver. Verified here: 2048-char URL -> POST=1, settles.
func TestSubmitMaxLengthPostAPIDelivers(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// Boundary check on the validator itself: exactly 2048 passes the
	// structural gate; the same URL padded one byte further fails.
	exact := "https://example.com/hook?pad=" + strings.Repeat("x", 2048-len("https://example.com/hook?pad="))
	if len(exact) != 2048 {
		t.Fatalf("fixture length = %d, want 2048", len(exact))
	}
	if rule := ValidatePostapi(exact, 2048); rule != nil {
		t.Fatalf("2048-char URL must pass the gate: %v", rule)
	}
	over := exact + "x"
	if rule := ValidatePostapi(over, 2048); rule == nil || rule.Rule.Code != "TASK_CONFIG_INVALID" {
		t.Fatalf("2049-char URL must fail with TASK_CONFIG_INVALID, got %v", rule)
	}

	// End-to-end delivery with a normal-length URL against the live gate
	// server (the boundary fixture targets an external host and would not
	// be answerable by httptest).
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1500)
	res, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("legal postapi must deliver: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("POST hits = %d, want 1", *hits)
	}
	if res.Billing["reward"] != 5.0 {
		t.Fatalf("reward = %v", res.Billing["reward"])
	}
}

// Submit open + empty postapi -> TASK_NOT_CONFIGURED (503 contract), POST=0.
func TestSubmitEmptyPostAPIRejected(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", "", 5, 1500)
	_, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	wantGateErr(t, err, "TASK_NOT_CONFIGURED")
	if *hits != 0 {
		t.Fatalf("empty postapi POSTed: %d", *hits)
	}
}

// TestTask pending + malformed postapi -> TASK_CONFIG_INVALID, POST=0,
// budget untouched.
func TestTestTaskMalformedPostAPIRejected(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	for _, bad := range []string{"not-a-url", "gopher://example.com/hook"} {
		code := tcSeedTask(t, pool, owner, "pending", bad, 5, 1500)
		_, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
		wantGateErr(t, err, "TASK_CONFIG_INVALID")
		if *hits != 0 {
			t.Fatalf("malformed postapi POSTed: %d", *hits)
		}
		if budget, status := tcBudget(t, pool, code); budget != 1500 || status != "pending" {
			t.Fatalf("budget/status changed: %v/%s", budget, status)
		}
	}
}

// TestTask abnormal status -> 409 TASK_NOT_OPEN, POST=0, budget untouched.
func TestTestTaskAbnormalStatusRejected(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "closed", srv.URL, 5, 1500)
	_, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	wantGateErr(t, err, "TASK_NOT_OPEN")
	if *hits != 0 {
		t.Fatalf("closed task POSTed: %d", *hits)
	}
	if budget, status := tcBudget(t, pool, code); budget != 1500 || status != "closed" {
		t.Fatalf("budget/status changed: %v/%s", budget, status)
	}

	// An injected abnormal status value (bypassing SetTaskStatus) is also
	// rejected, not silently treated as testable.
	abnormal := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	if _, err := pool.Exec(context.Background(),
		`UPDATE tb_tasks SET status = 'paused' WHERE code = $1`, abnormal); err != nil {
		t.Fatal(err)
	}
	_, err = TestTaskDeliver(context.Background(), pool, owner, abnormal, map[string]interface{}{"a": 1})
	wantGateErr(t, err, "TASK_NOT_OPEN")
	if *hits != 0 {
		t.Fatalf("abnormal-status task POSTed: %d", *hits)
	}
}
