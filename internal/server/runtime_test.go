package server

// R2.1: request execution deadline behavior proofs.
//
// The single requestDeadlineMiddleware (25s, below the frozen 30s
// WriteTimeout) is the only request-budget owner. These tests prove
// the deadline reaches handlers, real PostgreSQL queries/locks, and
// context-aware providers, and that a cancelled transactional
// mutation leaves no partial fact.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/config"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/service"
)

// deadlineTestServer builds a server with the production router and a
// short-budget override for deterministic tests. The middleware value
// itself is a production constant; the test injects a shorter budget
// through a package-level variable the middleware reads — see below.
func deadlineTestServer(t *testing.T) *Server {
	t.Helper()
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
}

// -- 1) fast request completes normally under the deadline --

func TestR21FastRequestCompletes(t *testing.T) {
	s := deadlineTestServer(t)
	router := s.buildRouter()
	req := httptest.NewRequest("GET", "/robots.txt", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { router.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
		if rec.Code != 200 {
			t.Fatalf("fast request = %d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast request did not complete")
	}
}

// -- 2) handler observes request-deadline cancellation (bounded) --
// Uses the real middleware with a test-only budget override.

func TestR21HandlerObservesDeadlineCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			// handler CAN observe cancellation — proves the ctx
			// with the budget actually reached the handler
			return
		case <-time.After(10 * time.Second):
			t.Error("handler never observed cancellation")
		}
	})
	// the production middleware with an explicit short budget —
	// same mechanism, injected duration (no mutable package state)
	var router http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestDeadlineMiddleware(300*time.Millisecond)(handler).ServeHTTP(w, r)
	})
	req := httptest.NewRequest("GET", "/anything", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() { router.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed >= time.Second {
			t.Fatalf("request ran %v — deadline not enforced", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not terminate after deadline")
	}
}

// -- 3) real PG: blocked query is released by the caller deadline --

func TestR21BlockedPGQueryReleasedByDeadline(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	// Take an exclusive lock on a scratch table in one connection.
	if _, err := pool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS r21_probe (id int)`); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE r21_probe`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	lockTx, err := pool.TxBegin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(context.Background())
	if _, err := lockTx.Exec(context.Background(),
		`INSERT INTO r21_probe VALUES (1)`); err != nil {
		// uncommitted insert holds the row/table write lock
		t.Fatalf("seed: %v", err)
	}

	// A second TRANSACTION needing that table lock, under a 500ms
	// caller deadline: its TRUNCATE deterministically waits on the
	// open transaction's uncommitted row write.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	blockedTx, berr := pool.TxBegin(ctx)
	if berr != nil {
		t.Fatalf("begin blocked tx: %v", berr)
	}
	defer blockedTx.Rollback(context.Background())
	start := time.Now()
	_, err = blockedTx.Exec(ctx, `TRUNCATE r21_probe`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("TRUNCATE unexpectedly acquired — open tx not holding the lock")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context deadline error, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("blocked query ran %v — deadline did not release it", elapsed)
	}
}

// -- 4) Creem provider path propagates request cancellation --
// The Creem client is context-aware (NewRequestWithContext); prove
// through the production checkout handler that a slow upstream and a
// request deadline terminate the call early (before the 15s client
// timeout). We drive the REAL handler with a stubbed Creem base URL
// pointing at a slow test server, using the server's test-only
// creemBaseOverride field.

// ===========================================================================
// R2.1 repair: REAL service.Submit PostAPI cancellation + REAL Creem
// checkout handler cancellation (both through the production router /
// service mechanisms, real PostgreSQL).
// ===========================================================================

// -- real service.Submit: slow PostAPI + short request deadline --

func TestR21RealServiceSubmitPostAPICancellation(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	ctxBG := context.Background()

	// slow fake PostAPI that confirms the request actually arrived
	arrived := make(chan struct{}, 1)
	slowRelease := make(chan struct{})
	slowPost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-slowRelease:
		}
	}))
	defer slowPost.Close()
	defer close(slowRelease)

	// TWO distinct bots: the task belongs to the OWNER; the agent
	// submits to it (the production subject split).
	suffix := fmt.Sprint(time.Now().UnixNano())
	var ownerID, agentID int64
	if err := pool.QueryRow(ctxBG, `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', 500) RETURNING id`,
		"r21owner_"+suffix, "kf_live_"+suffix+strings.Repeat("a", 64-len(suffix))).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctxBG, `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', 0) RETURNING id`,
		"r21agent_"+suffix, "kf_live_"+suffix+strings.Repeat("b", 64-len(suffix))).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	var taskCode string
	if err := pool.QueryRow(ctxBG, `
		INSERT INTO tb_tasks (bot_id, code, title, requirements, postapi, price, budget, status, created_at, updated_at)
		VALUES ($1, substr(md5(random()::text),1,12), 'R2.1 Task', 'x', $2, 1, 1000, 'open', NOW(), NOW())
		RETURNING code`, ownerID, slowPost.URL).Scan(&taskCode); err != nil {
		t.Fatalf("task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctxBG, `DELETE FROM tb_transactions WHERE bot_id IN ($1,$2)`, ownerID, agentID)
		_, _ = pool.Exec(ctxBG, `DELETE FROM tb_tasks WHERE code=$1`, taskCode)
		_, _ = pool.Exec(ctxBG, `DELETE FROM tb_logs WHERE bot_id IN ($1,$2)`, ownerID, agentID)
		_, _ = pool.Exec(ctxBG, `DELETE FROM tb_bots WHERE id IN ($1,$2)`, ownerID, agentID)
	})

	// baseline economics/state
	var budgetBefore float64
	var ownerBalanceBefore, agentBalanceBefore float64
	var statusBefore string
	_ = pool.QueryRow(ctxBG, `SELECT budget FROM tb_tasks WHERE code=$1`, taskCode).Scan(&budgetBefore)
	_ = pool.QueryRow(ctxBG, `SELECT balance FROM tb_bots WHERE id=$1`, ownerID).Scan(&ownerBalanceBefore)
	_ = pool.QueryRow(ctxBG, `SELECT balance FROM tb_bots WHERE id=$1`, agentID).Scan(&agentBalanceBefore)
	_ = pool.QueryRow(ctxBG, `SELECT status FROM tb_tasks WHERE code=$1`, taskCode).Scan(&statusBefore)

	// the request context with a deadline far below the 10s PostAPI net
	ctx, cancel := context.WithTimeout(ctxBG, 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	submitRes, submitErr := service.Submit(ctx, pool, taskCode, agentID, map[string]interface{}{"a": 1})
	elapsed := time.Since(start)

	// upstream observed the request (proves the chain reached PostAPI)
	select {
	case <-arrived:
	default:
		t.Fatal("PostAPI request never arrived — chain broken")
	}
	// bounded return well before the 10s client timeout
	if elapsed >= 5*time.Second {
		t.Fatalf("Submit ran %v — not bounded by the request deadline", elapsed)
	}

	// canonical failure contract: non-nil error, AppError, 424,
	// POSTAPI_NETWORK_ERROR
	if submitErr == nil {
		t.Fatalf("cancelled Submit returned success: %+v", submitRes)
	}
	ae, ok := submitErr.(*apperrors.AppError)
	if !ok {
		t.Fatalf("Submit error is not the canonical AppError: %T %+v", submitErr, submitErr)
	}
	if ae.HTTPCode != 424 {
		t.Fatalf("HTTPCode = %d, want 424", ae.HTTPCode)
	}
	if ae.Code != "POSTAPI_NETWORK_ERROR" {
		t.Fatalf("Code = %q, want POSTAPI_NETWORK_ERROR", ae.Code)
	}

	// transaction invariant: nothing partially settled
	time.Sleep(100 * time.Millisecond) // let deferred cleanup settle
	var budgetAfter, ownerBalanceAfter, agentBalanceAfter float64
	var statusAfter string
	var earn int
	_ = pool.QueryRow(ctxBG, `SELECT budget FROM tb_tasks WHERE code=$1`, taskCode).Scan(&budgetAfter)
	_ = pool.QueryRow(ctxBG, `SELECT status FROM tb_tasks WHERE code=$1`, taskCode).Scan(&statusAfter)
	_ = pool.QueryRow(ctxBG, `SELECT balance FROM tb_bots WHERE id=$1`, ownerID).Scan(&ownerBalanceAfter)
	_ = pool.QueryRow(ctxBG, `SELECT balance FROM tb_bots WHERE id=$1`, agentID).Scan(&agentBalanceAfter)
	_ = pool.QueryRow(ctxBG, `
		SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='earn_task'`, agentID).Scan(&earn)
	if budgetAfter != budgetBefore {
		t.Fatalf("owner task budget mutated on cancelled submit: %v -> %v", budgetBefore, budgetAfter)
	}
	if statusAfter != statusBefore {
		t.Fatalf("task status changed: %s -> %s", statusBefore, statusAfter)
	}
	if earn != 0 {
		t.Fatalf("agent earn_task settlement committed: %d", earn)
	}
	if agentBalanceAfter != agentBalanceBefore {
		t.Fatalf("agent balance changed: %v -> %v", agentBalanceBefore, agentBalanceAfter)
	}
	if ownerBalanceAfter != ownerBalanceBefore {
		t.Fatalf("owner balance changed: %v -> %v", ownerBalanceBefore, ownerBalanceAfter)
	}

	// row lock released: a later independent transaction locks/updates
	// the same task within a bounded context
	lockCtx, lockCancel := context.WithTimeout(ctxBG, 3*time.Second)
	defer lockCancel()
	ltx, err := pool.TxBegin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := ltx.Exec(lockCtx,
		`UPDATE tb_tasks SET budget = budget WHERE code = $1`, taskCode)
	if err != nil {
		_ = ltx.Rollback(lockCtx)
		t.Fatalf("later transaction blocked on the task row: %v", err)
	}
	if err := ltx.Commit(lockCtx); err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("later update affected %d rows", tag.RowsAffected())
	}
}

// -- real Creem checkout handler: slow provider GET product + short
// request deadline; cancellation BEFORE any local payment creation.
// Asserts the existing HTTP contract (502 PAYMENT_PROVIDER_UNAVAILABLE)
// and owner-scoped payment-row invariance. --

func TestR21RealCreemCheckoutCancellation(t *testing.T) {
	s := deadlineTestServer(t) // real PG pool + production config shape
	ctxBG := context.Background()

	// slow fake provider: the GET product phase blocks; the request's
	// arrival is confirmed so we know the handler reached the provider
	arrived := make(chan struct{}, 1)
	slowRelease := make(chan struct{})
	slowCreem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-slowRelease:
		}
	}))
	defer slowCreem.Close()
	defer close(slowRelease)

	// configure a valid Creem runtime (all-or-none contract) pointed at
	// the slow fake via the existing test-only base override
	s.Config.CreemAPIKey = "test-key"
	s.Config.CreemWebhookSecret = "test-secret"
	s.Config.CreemMode = "test"
	s.Config.CreemSuccessURL = "https://example.com/return"
	s.Config.CreemPackages = map[string]config.CreemPackage{
		"starter": {Code: "starter", ProductID: "prod_r21", Credits: 100},
	}
	s.creemBaseOverride = slowCreem.URL

	// seed the owner bot + session cookie
	_, ownerID := seededTestServerOn(t, s)
	w := httptest.NewRecorder()
	setOwnerCookie(w, ownerID, s.Config.SessionSecret, false)
	ownerCookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	// baseline: OWNER-SCOPED payment rows (never whole-table counts —
	// parallel packages share the CI database)
	var paymentsBefore int
	_ = s.Pool.QueryRow(ctxBG,
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id = $1`, ownerID).Scan(&paymentsBefore)

	// short deterministic request deadline through the SAME router
	// mechanism (buildRouterWithDeadline — no mutable global state)
	router := s.buildRouterWithDeadline(800 * time.Millisecond)

	req := httptest.NewRequest("POST", "/api/owner/payments/checkout",
		strings.NewReader(`{"package":"starter"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ownerCookie)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() { router.ServeHTTP(rec, req); close(done) }()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("checkout handler did not return bounded")
	}
	elapsed := time.Since(start)

	// upstream observed the request (chain reached the provider)
	select {
	case <-arrived:
	default:
		t.Fatal("provider request never arrived")
	}
	// bounded BEFORE the 15s Creem client timeout
	if elapsed >= 5*time.Second {
		t.Fatalf("checkout ran %v — request deadline not effective", elapsed)
	}

	// existing HTTP contract: 502 + PAYMENT_PROVIDER_UNAVAILABLE
	if rec.Code != 502 {
		t.Fatalf("checkout status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PAYMENT_PROVIDER_UNAVAILABLE") {
		t.Fatalf("error code missing PAYMENT_PROVIDER_UNAVAILABLE: %s", rec.Body.String())
	}

	// owner-scoped payment rows unchanged (cancellation hit the
	// GET-product phase, before pending payment creation)
	var paymentsAfter int
	_ = s.Pool.QueryRow(ctxBG,
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id = $1`, ownerID).Scan(&paymentsAfter)
	if paymentsAfter != paymentsBefore {
		t.Fatalf("owner payment rows changed during cancelled checkout: %d -> %d",
			paymentsBefore, paymentsAfter)
	}
}

// seededTestServerOn seeds an active bot on the GIVEN server (variant
// of the shared helper for an externally-constructed Server).
func seededTestServerOn(t *testing.T, s *Server) (t2 *Server, botID int64) {
	t.Helper()
	var id int64
	err := s.Pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', 10) RETURNING id`,
		"r21owner"+fmt.Sprint(time.Now().UnixNano()),
		"kf_live_r21o"+fmt.Sprint(time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(),
			`DELETE FROM tb_bots WHERE id=$1`, id)
	})
	return s, id
}
