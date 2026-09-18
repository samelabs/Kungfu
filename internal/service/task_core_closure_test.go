package service

// Task Core Closure integration tests. Real PostgreSQL via
// KF_TEST_DATABASE_URL; owner POST endpoints via httptest servers;
// failure injection via real DB CHECK constraints and a gating server.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// -- helpers --

func tcTestPool(t *testing.T) *pg.Pool {
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

func tcSeedBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000") + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"tcc_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func tcSeedTask(t *testing.T, pool *pg.Pool, botID int64, status, postapi string, price, budget float64) string {
	t.Helper()
	code := "tc" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "") +
		fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
	// 12-hex: build from a hash of the time
	code = fmt.Sprintf("%x", time.Now().UnixNano())[:12]
	var pa *string
	if postapi != "" {
		pa = &postapi
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tb_tasks (code, bot_id, title, requirements, postapi, budget, price, status)
		 VALUES ($1, $2, 'TCC task', 'req', $3, $4, $5, $6)`,
		code, botID, pa, budget, price, status)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_task_logs WHERE task_code = $1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE target_type = 'task' AND target_id = $1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_tasks WHERE code = $1`, code)
	})
	return code
}

func tcBudget(t *testing.T, pool *pg.Pool, code string) (float64, string) {
	t.Helper()
	var budget float64
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT budget, status FROM tb_tasks WHERE code = $1`, code).Scan(&budget, &status); err != nil {
		t.Fatalf("read task: %v", err)
	}
	return budget, status
}

func tcEarnCount(t *testing.T, pool *pg.Pool, botID int64) (int, float64) {
	t.Helper()
	var n int
	var sum float64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE bot_id = $1 AND type = 'earn_task'`, botID).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

func tcBalance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newGateServer returns a server whose every response (status + body) is
// controlled atomically at request time, plus functions to flip it.
func newGateServer() (srv *httptest.Server, setOK func(bool), hits *int32) {
	var ok atomic.Bool
	ok.Store(true)
	h := int32(0)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&h, 1)
		if ok.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"accepted":true}`))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"no"}`))
		}
	}))
	return srv, func(v bool) { ok.Store(v) }, &h
}

// -- 3. illegal status is not silently treated as close --

func TestSetTaskStatusRejectsUnknownStatus(t *testing.T) {
	pool := tcTestPool(t)
	botID := tcSeedBot(t, pool, 5000)
	code := tcSeedTask(t, pool, botID, "pending", "", 1, 1000)

	for _, bad := range []string{"paused", "shipped", "CLOSED", ""} {
		_, err := SetTaskStatus(context.Background(), pool, botID, code, bad)
		ae, ok := errors.IsAppError(err)
		if !ok || ae.HTTPCode != 400 {
			t.Fatalf("status %q: want 400, got %v", bad, err)
		}
	}
	// unchanged
	_, status := tcBudget(t, pool, code)
	if status != "pending" {
		t.Fatalf("status mutated by illegal request: %s", status)
	}

	// legal close still works
	if _, err := SetTaskStatus(context.Background(), pool, botID, code, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, status := tcBudget(t, pool, code); status != "closed" {
		t.Fatalf("status = %s, want closed", status)
	}
}

// -- 5. pending task cannot be agent-submitted --

func TestSubmitPendingTaskRejectedBeforePost(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 1, 1000)

	_, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 409 || ae.Code != "TASK_NOT_OPEN" {
		t.Fatalf("want 409 TASK_NOT_OPEN, got %v", err)
	}
	if *hits != 0 {
		t.Fatalf("pending task POSTed: %d", *hits)
	}
}

// -- 6/7. pending owner test works, stays pending, no earn --

func TestOwnerTestPendingTask(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1000)

	res, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("pending owner test must succeed: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("POST hits = %d, want 1", *hits)
	}

	budget, status := tcBudget(t, pool, code)
	if budget != 995 {
		t.Fatalf("budget = %v, want 995 (1000 - 5)", budget)
	}
	if status != "pending" {
		t.Fatalf("status = %s, want pending (test keeps pending)", status)
	}
	if n, _ := tcEarnCount(t, pool, owner); n != 0 {
		t.Fatalf("owner test produced earn_task: %d", n)
	}
	if res.Billing["status"] != "pending" {
		t.Fatalf("billing status = %v, want pending", res.Billing["status"])
	}
}

// -- 8. owner test POST failure does not touch budget --

func TestOwnerTestPostFailureNoBudgetChange(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, setOK, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1000)
	setOK(false)

	_, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 424 {
		t.Fatalf("want 424, got %v", err)
	}
	budget, status := tcBudget(t, pool, code)
	if budget != 1000 || status != "pending" {
		t.Fatalf("budget/status changed on failed test: %v/%s", budget, status)
	}
}

// -- 9. settlement failure is an API error (no success-with-error-status) --

func TestOwnerTestSettlementFailureIsError(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1000)

	// Poison the settlement UPDATE with a CHECK constraint that only
	// rejects our magic budget value.
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE tb_tasks ADD CONSTRAINT tc_settle_fail_chk CHECK (budget <> 995)`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_tasks DROP CONSTRAINT IF EXISTS tc_settle_fail_chk`)
	})

	_, err := TestTaskDeliver(ctx, pool, owner, code, map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("settlement failure must be an API error, not a success response")
	}
	// budget unchanged (rolled back)
	if budget, _ := tcBudget(t, pool, code); budget != 1000 {
		t.Fatalf("budget = %v, want 1000 (settlement rolled back)", budget)
	}
}

// -- 10. agent POST non-2xx: budget unchanged, no earn --

func TestSubmitPostFailureNoSettlement(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, setOK, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1000)
	setOK(false)

	_, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 424 {
		t.Fatalf("want 424, got %v", err)
	}
	budget, status := tcBudget(t, pool, code)
	if budget != 1000 || status != "open" {
		t.Fatalf("budget/status changed on failed delivery: %v/%s", budget, status)
	}
	if n, _ := tcEarnCount(t, pool, agent); n != 0 {
		t.Fatalf("failed delivery produced earn_task: %d", n)
	}
	if b := tcBalance(t, pool, agent); b != 0 {
		t.Fatalf("agent balance = %v, want 0", b)
	}
}

// -- 11. agent POST 2xx: budget decrement + earn_task in one tx --

func TestSubmitSuccessAtomicSettlement(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1000)

	res, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if budget, _ := tcBudget(t, pool, code); budget != 995 {
		t.Fatalf("budget = %v, want 995", budget)
	}
	if n, sum := tcEarnCount(t, pool, agent); n != 1 || sum != 5 {
		t.Fatalf("earn_task = %d/%v, want 1/5", n, sum)
	}
	if b := tcBalance(t, pool, agent); b != 5 {
		t.Fatalf("agent balance = %v, want 5", b)
	}
	if res.Billing["reward"] != 5.0 {
		t.Fatalf("reward = %v", res.Billing["reward"])
	}
}

// -- 12. budget update failure: zero earn --

func TestSubmitBudgetWriteFailureZeroEarn(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1000)

	// Poison the budget write: reject budget=995 (the post-settlement value).
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE tb_tasks ADD CONSTRAINT tc_budget_fail_chk CHECK (budget <> 995)`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_tasks DROP CONSTRAINT IF EXISTS tc_budget_fail_chk`)
	})

	_, err := Submit(ctx, pool, code, agent, map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("submit must fail when the budget write fails")
	}
	if n, _ := tcEarnCount(t, pool, agent); n != 0 {
		t.Fatalf("earn_task despite budget write failure: %d", n)
	}
	if b := tcBalance(t, pool, agent); b != 0 {
		t.Fatalf("agent balance = %v, want 0", b)
	}
	if budget, _ := tcBudget(t, pool, code); budget != 1000 {
		t.Fatalf("budget = %v, want 1000 (rolled back)", budget)
	}
}

// -- 13. concurrent close vs submit: no delivery after close --

func TestConcurrentCloseVsSubmit(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1000)

	var wg sync.WaitGroup
	submitErr := make([]error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, submitErr[0] = Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	}()
	go func() {
		defer wg.Done()
		_, _ = SetTaskStatus(context.Background(), pool, owner, code, "closed")
	}()
	wg.Wait()

	_, status := tcBudget(t, pool, code)
	if status != "closed" {
		t.Fatalf("status = %s, want closed (close must win or race cleanly)", status)
	}
	// If the submit lost the race: rejected and never POSTed. If it won
	// before the close, its settlement is complete. Either way no
	// delivered-but-unsettled state.
	if submitErr[0] != nil {
		// submit rejected: no earn, no budget change from submit
		if n, _ := tcEarnCount(t, pool, agent); n != 0 {
			t.Fatalf("rejected submit still earned: %d", n)
		}
	} else {
		if n, _ := tcEarnCount(t, pool, agent); n != 1 {
			t.Fatalf("successful submit earned %d times", n)
		}
	}
	_ = hits
}

// -- 14. concurrent submits: budget cannot be over-delivered --

func TestConcurrentSubmitsNoOverDelivery(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agents := []int64{tcSeedBot(t, pool, 0), tcSeedBot(t, pool, 0), tcSeedBot(t, pool, 0), tcSeedBot(t, pool, 0)}
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// budget 1400, price 400: exactly TWO deliveries are fundable
	// (1400->1000 still fundable, 1000->600 unfundable auto-closes).
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 400, 1400)

	var wg sync.WaitGroup
	errs := make([]error, len(agents))
	for i, ag := range agents {
		wg.Add(1)
		go func(i int, ag int64) {
			defer wg.Done()
			_, errs[i] = Submit(context.Background(), pool, code, ag, map[string]interface{}{"a": 1})
		}(i, ag)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else {
			ae, ok := errors.IsAppError(err)
			if !ok || ae.HTTPCode != 409 {
				t.Fatalf("loser got non-409 error: %v", err)
			}
		}
	}
	if succeeded != 2 {
		t.Fatalf("succeeded = %d, want exactly 2 (fundable deliveries)", succeeded)
	}
	if *hits != 2 {
		t.Fatalf("POST hits = %d, want 2 (no unfundable POSTs)", *hits)
	}
	budget, status := tcBudget(t, pool, code)
	if budget != 600 || status != "closed" {
		t.Fatalf("final = %v/%s, want 600/closed", budget, status)
	}
	totalEarn := 0.0
	for _, ag := range agents {
		n, sum := tcEarnCount(t, pool, ag)
		if n > 1 {
			t.Fatalf("agent earned %d times", n)
		}
		totalEarn += sum
	}
	if totalEarn != 800 {
		t.Fatalf("total earn = %v, want 800 (2 x 400)", totalEarn)
	}
}

// -- 15. budget >= 1000 but < price is not acceptable --

func TestBudgetBelowPriceNotAcceptable(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// budget 1200 >= 1000 but < price 1500: not acceptable, invisible on board.
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 1500, 1200)

	board, err := ListOpenTasks(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range board["tasks"].([]map[string]interface{}) {
		if tm["code"] == code {
			t.Fatal("unfundable task visible on board")
		}
	}
	if _, err := GetOpenTask(context.Background(), pool, code); err == nil {
		t.Fatal("unfundable task retrievable via GetOpenTask")
	}
	_, err = Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 409 {
		t.Fatalf("want 409, got %v", err)
	}
	if *hits != 0 {
		t.Fatalf("unfundable task POSTed: %d", *hits)
	}
}

// -- 16. auto-close after settlement when next delivery unfundable --

func TestAutoCloseAfterSettlement(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	// 1000 - 5 = 995 >= 1000? No: 995 < 1000 -> auto-close.
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1000)

	if _, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	budget, status := tcBudget(t, pool, code)
	if budget != 995 || status != "closed" {
		t.Fatalf("final = %v/%s, want 995/closed", budget, status)
	}
	// Closed task is invisible on board and not submittable.
	board, _ := ListOpenTasks(context.Background(), pool)
	for _, tm := range board["tasks"].([]map[string]interface{}) {
		if tm["code"] == code {
			t.Fatal("closed task visible on board")
		}
	}
}

// -- 17. board / GetOpenTask / submit agree on acceptability --

func TestBoardGetSubmitConsistency(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	// fundable: on board, gettable, submittable
	okCode := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1500)
	board, _ := ListOpenTasks(context.Background(), pool)
	found := false
	for _, tm := range board["tasks"].([]map[string]interface{}) {
		if tm["code"] == okCode {
			found = true
		}
	}
	if !found {
		t.Fatal("fundable task missing from board")
	}
	if _, err := GetOpenTask(context.Background(), pool, okCode); err != nil {
		t.Fatalf("fundable task not gettable: %v", err)
	}
	if _, err := Submit(context.Background(), pool, okCode, agent, map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("fundable task not submittable: %v", err)
	}

	// pending: not on board, not gettable, not submittable
	pend := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	board2, _ := ListOpenTasks(context.Background(), pool)
	for _, tm := range board2["tasks"].([]map[string]interface{}) {
		if tm["code"] == pend {
			t.Fatal("pending task on board")
		}
	}
	if _, err := GetOpenTask(context.Background(), pool, pend); err == nil {
		t.Fatal("pending task gettable")
	}
	if _, err := Submit(context.Background(), pool, pend, agent, map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("pending task submittable")
	}
}

// -- 18. refund atomicity (7-day cooldown bypassed by backdating closed_at) --

func TestRefundBudgetAtomic(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 2000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1200)

	// settle once -> 1195, still fundable; then close and backdate.
	if _, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := SetTaskStatus(context.Background(), pool, owner, code, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tb_tasks SET closed_at = NOW() - INTERVAL '8 days' WHERE code = $1`, code); err != nil {
		t.Fatal(err)
	}

	before := tcBalance(t, pool, owner) // 2000 - 1200 locked + ... = 800
	if _, err := RefundTaskBudget(context.Background(), pool, owner, code); err != nil {
		t.Fatalf("refund: %v", err)
	}
	budget, _ := tcBudget(t, pool, code)
	if budget != 0 {
		t.Fatalf("budget = %v, want 0", budget)
	}
	after := tcBalance(t, pool, owner)
	if after != before+1195 {
		t.Fatalf("balance = %v, want %v (+1195 refund)", after, before+1195)
	}
	var n int
	var sum float64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE bot_id = $1 AND type = 'refund_task'`, owner).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 1 || sum != 1195 {
		t.Fatalf("refund_task = %d/%v, want 1/1195", n, sum)
	}
}

// -- 1/2. create/addbudget atomicity (lock_task <-> task write) --

func TestCreateTaskLockAtomic(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 2000)

	// Insufficient credits: no lock row, no task row.
	_, err := CreateTask(context.Background(), pool, owner, &OwnerTaskConfig{}, &CreateTaskInput{
		Title: "TCC atomic", Requirements: "req", PostAPI: "https://example.com/h",
		Budget: 5000, Price: 1,
	})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 402 {
		t.Fatalf("want 402, got %v", err)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1 AND type = 'lock_task'`, owner).Scan(&n)
	if n != 0 {
		t.Fatalf("lock_task leaked: %d", n)
	}

	// Sufficient: exactly one lock_task == budget, task exists.
	res, err := CreateTask(context.Background(), pool, owner, &OwnerTaskConfig{}, &CreateTaskInput{
		Title: "TCC atomic", Requirements: "req", PostAPI: "https://example.com/h",
		Budget: 1200, Price: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := res["task"].(map[string]interface{})["code"].(string)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_task_logs WHERE task_code = $1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE target_type = 'task' AND target_id = $1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_tasks WHERE code = $1`, code)
	})
	var lockN int
	var lockSum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE bot_id = $1 AND type = 'lock_task'`, owner).Scan(&lockN, &lockSum)
	if lockN != 1 || lockSum != -1200 {
		t.Fatalf("lock_task = %d/%v, want 1/-1200", lockN, lockSum)
	}

	// AddBudget atomic: another lock_task with the same ref.
	if _, err := AddTaskBudget(context.Background(), pool, owner, code, 300); err != nil {
		t.Fatalf("add budget: %v", err)
	}
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE bot_id = $1 AND type = 'lock_task'`, owner).Scan(&lockN, &lockSum)
	if lockN != 2 || lockSum != -1500 {
		t.Fatalf("lock_task after add = %d/%v, want 2/-1500", lockN, lockSum)
	}
	if b, _ := tcBudget(t, pool, code); b != 1500 {
		t.Fatalf("budget = %v, want 1500", b)
	}
}

// -- 4. DB error is not disguised as 404 (owner task paths) --

func TestDBErrorNotDisguisedAsNotFound(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	code := tcSeedTask(t, pool, owner, "pending", "", 1, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := SetTaskStatus(ctx, pool, owner, code, "closed")
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("SetTaskStatus DB error: want 500 INTERNAL_ERROR, got %v", err)
	}

	_, err = AddTaskBudget(ctx, pool, owner, code, 100)
	ae, ok = errors.IsAppError(err)
	if !ok || ae.HTTPCode != 500 {
		t.Fatalf("AddTaskBudget DB error: want 500, got %v", err)
	}
}
