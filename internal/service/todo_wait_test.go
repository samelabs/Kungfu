package service

// todo_wait tests: the wake channel returns immediately when the
// caller already owes something, answers wait on timeout, wakes
// within the fast recheck window when an obligation (or an addressed
// opportunity) appears mid-wait, and degrades gracefully at the
// global waiter cap.

import (
	"context"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/task"
)

func waitReady(t *testing.T, pool *pg.Pool) (owner, a int64, code, raw string) {
	t.Helper()
	owner, _, _ = a7TestBot(t, pool, 5)
	a, _, _ = a7TestBot(t, pool, 5)
	code, raw = threadStart(t, pool, owner, "wait", true, "")
	return
}

func TestTodoWaitImmediateWhenOwing(t *testing.T) {
	pool := revisionTestPool(t)
	ctx := context.Background()
	owner, a, code, raw := waitReady(t, pool)
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "a, wake up", "", "", nil, []int64{a}, nil, "w1"); err != nil {
		t.Fatalf("post: %v", err)
	}

	start := time.Now()
	res, err := TodoWait(ctx, pool, a, 5)
	if err != nil {
		t.Fatalf("TodoWait: %v", err)
	}
	if res["changed"] != true {
		t.Fatalf("owing caller must return changed=true immediately, got %v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("immediate return took %s", time.Since(start))
	}
	todos := res["todos"].([]map[string]any)
	if len(todos) != 1 || todos[0]["kind"] != "reply" {
		t.Fatalf("expected one reply item, got %v", todos)
	}
}

// TestTodoWaitColdStartOpportunity: a keeper installed onto an
// account with a pending (untaken) assignment must get an immediate
// answer, not sleep through the invite waiting for a change.
func TestTodoWaitColdStartOpportunity(t *testing.T) {
	pool := revisionTestPool(t)
	ctx := context.Background()
	lead, a, code, raw := waitReady(t, pool)
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	// an open assignment addressed to a: an opportunity, NOT an
	// obligation (ask=[] keeps the two-speaker default quiet)
	if _, err := ThreadPost(ctx, pool, lead, code, "cold start", "", "invite a", nil, []int64{},
		&AssignSpec{To: a, Requirements: "r"}, "cw1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	start := time.Now()
	res, err := TodoWait(ctx, pool, a, 5)
	if err != nil {
		t.Fatalf("TodoWait: %v", err)
	}
	if res["changed"] != true {
		t.Fatalf("cold-start opportunity must return changed=true, got %v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cold start must return immediately, took %s", time.Since(start))
	}
	opp := res["opportunities"].(map[string]any)
	if opp["assignments"].(int64) < 1 {
		t.Fatalf("wake must carry the invite count: %v", opp)
	}
}

func TestTodoWaitTimeoutWaits(t *testing.T) {
	pool := revisionTestPool(t)
	a, _, _ := a7TestBot(t, pool, 5)

	start := time.Now()
	res, err := TodoWait(context.Background(), pool, a, 1)
	if err != nil {
		t.Fatalf("TodoWait: %v", err)
	}
	if res["changed"] != false || res["next_action"] != "wait" {
		t.Fatalf("idle timeout must be changed=false + wait, got %v", res)
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("returned too early: %s", elapsed)
	}
}

func TestTodoWaitWakesOnNewObligation(t *testing.T) {
	pool := revisionTestPool(t)
	ctx := context.Background()
	owner, a, code, raw := waitReady(t, pool)
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	var wg sync.WaitGroup
	var woke bool
	var wokeTodos int
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := TodoWait(ctx, pool, a, 8)
		if err != nil {
			return // context canceled by pool teardown racing
		}
		woke = res["changed"] == true
		if todos, ok := res["todos"].([]map[string]any); ok {
			wokeTodos = len(todos)
		}
	}()
	time.Sleep(1500 * time.Millisecond) // let the waiter take its snapshot
	if _, err := ThreadPost(ctx, pool, owner, code, "a, now", "", "", nil, []int64{a}, nil, "w2"); err != nil {
		t.Fatalf("post: %v", err)
	}
	wg.Wait()
	if !woke {
		t.Fatal("waiter did not wake on a new obligation within its budget")
	}
	if wokeTodos != 1 {
		t.Fatalf("woken turn = %d items, want 1", wokeTodos)
	}
}

func TestTodoWaitWakesOnOpportunity(t *testing.T) {
	pool := revisionTestPool(t)
	ctx := context.Background()
	owner, a, code, raw := waitReady(t, pool)
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := TodoWait(ctx, pool, a, 8)
		if err != nil {
			return
		}
		if res["changed"] == true {
			opp := res["opportunities"].(map[string]any)
			if opp["assignments"].(int64) < 1 {
				t.Error("wake on opportunity without the invite count")
			}
		} else {
			t.Error("waiter did not wake on an addressed assignment")
		}
	}()
	time.Sleep(1500 * time.Millisecond)
	// an open assignment addressed to a: an opportunity, NOT an
	// obligation — ask=[] keeps the two-speaker default from minting
	// a reply receipt
	if _, err := ThreadPost(ctx, pool, owner, code, "do this", "", "hand a an open assign", nil, []int64{},
		&AssignSpec{To: a, Requirements: "r"}, "w3"); err != nil {
		t.Fatalf("post+assign: %v", err)
	}
	wg.Wait()
}

// TestTodoWaitWakeTokenDistinguishesSameCountWork: the wake channel
// must tell "a different one of the same many" from "the one already
// shown" (external audit P1-03). T1 offered, T1 replaced by T2 — the
// opportunity COUNTS are identical; only the identity (wake_token)
// moves, and that movement is the wake signal.
func TestTodoWaitWakeTokenDistinguishesSameCountWork(t *testing.T) {
	pool := revisionTestPool(t)
	publisher, _, _ := a7TestBot(t, pool, 100)
	a, aName, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	restricted := func() task.Contract {
		c := pubContract("")
		c.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{aName}}
		return c
	}
	t1 := pubCreateForTest(t, pool, publisher, restricted(), 5)
	if _, err := OpenTask(ctx, pool, publisher, t1); err != nil {
		t.Fatalf("open t1: %v", err)
	}

	// cold start: the offered task answers immediately, with a token
	res, err := TodoWait(ctx, pool, a, 3)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res["changed"] != true || res["wake_token"] == "" || res["wake_token"] == nil {
		t.Fatalf("offered work must answer changed=true with a wake_token, got %v", res)
	}
	token1 := res["wake_token"].(string)
	opp1 := res["opportunities"].(map[string]any)
	if opp1["tasks"].(int64) != 1 {
		t.Fatalf("t1 offered: %v", opp1)
	}

	// T1 ends (closed), T2 arrives: the counts return to the SAME
	// number — the token must NOT
	if _, err := CloseTask(ctx, pool, publisher, t1); err != nil {
		t.Fatalf("close t1: %v", err)
	}
	t2 := pubCreateForTest(t, pool, publisher, restricted(), 5)
	if _, err := OpenTask(ctx, pool, publisher, t2); err != nil {
		t.Fatalf("open t2: %v", err)
	}
	res2, err := TodoWait(ctx, pool, a, 3)
	if err != nil {
		t.Fatalf("wait2: %v", err)
	}
	if res2["changed"] != true {
		t.Fatalf("t2 offered must answer changed=true, got %v", res2)
	}
	token2 := res2["wake_token"].(string)
	opp2 := res2["opportunities"].(map[string]any)
	if opp2["tasks"].(int64) != 1 {
		t.Fatalf("t2 offered: %v", opp2)
	}
	if token1 == token2 {
		t.Fatalf("same-count different work must move wake_token: %q == %q", token1, token2)
	}
}

func TestTodoWaitWaiterCap(t *testing.T) {
	pool := revisionTestPool(t)
	a, _, _ := a7TestBot(t, pool, 5)

	old := todoWaitMaxWaiters
	todoWaitMaxWaiters = 1
	defer func() { todoWaitMaxWaiters = old }()

	// occupy the single waiter slot
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = TodoWait(context.Background(), pool, a, 2)
	}()
	time.Sleep(300 * time.Millisecond)

	if got := todoWaitWaiterCount(); got != 1 {
		t.Fatalf("waiter count = %d, want 1", got)
	}
	// the second caller degrades immediately, not by blocking
	start := time.Now()
	res, err := TodoWait(context.Background(), pool, a, 5)
	if err != nil {
		t.Fatalf("TodoWait: %v", err)
	}
	if res["changed"] != false {
		t.Fatalf("over-cap call must return changed=false, got %v", res)
	}
	if ra, _ := res["retry_after"].(int); ra == 0 {
		t.Fatalf("over-cap call must carry retry_after, got %v", res)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("over-cap degradation must be immediate, took %s", time.Since(start))
	}
	wg.Wait()
}
