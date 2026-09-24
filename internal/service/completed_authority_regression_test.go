package service

import (
	"context"
	"testing"

	"kungfu.md/internal/repository"
)

// Completed-authority regressions (frontend/contract closure ticket):
// The public Completed figure (homepage) and the Owner task success_count
// must count ONLY real agent deliveries. Authority = earn_task
// transactions (legacy era, ref_type='task') + settled agent submissions
// (durable era, ref_type='task_submission'). A successful owner_test
// writes a kind-blind post_succeeded log row but NO earn_task — it must
// never move either counter.
func TestOwnerTestNeverInflatesCompleted(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, setOK, _ := newGateServer()
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)
	setOK(true)

	completedFor := func() int64 {
		tasks, err := repository.QueryHomepageTasks(context.Background(), pool, MinOpenBudget)
		if err != nil {
			t.Fatalf("homepage query: %v", err)
		}
		for _, tk := range tasks {
			if tk.Code == code {
				return tk.SuccessCount
			}
		}
		return -1
	}
	ownerSuccess := func() int64 {
		rows, err := repository.ListOwnerTasksWithStats(context.Background(), pool, owner)
		if err != nil {
			t.Fatalf("owner stats: %v", err)
		}
		for i := range rows {
			if rows[i].Code == code {
				return rows[i].SuccessCount
			}
		}
		return -1
	}

	if c := completedFor(); c != 0 {
		t.Fatalf("baseline homepage Completed = %d, want 0", c)
	}

	// Successful owner test: PostAPI 2xx, kind=owner_test settles with
	// budget mutation but NO earn_task.
	if _, err := TestTaskDeliver(context.Background(), pool, owner, code,
		"otc-key-1", map[string]interface{}{"probe": 1}); err != nil {
		t.Fatalf("owner test deliver: %v", err)
	}

	// 1+2: owner_test must not move either counter.
	if c := completedFor(); c != 0 {
		t.Fatalf("owner_test inflated homepage Completed: %d, want 0", c)
	}
	if c := ownerSuccess(); c != 0 {
		t.Fatalf("owner_test inflated Owner success_count: %d, want 0", c)
	}

	// 3: durable agent settled increases both counters by exactly 1.
	agent := tcSeedBot(t, pool, 0)
	if _, err := Submit(context.Background(), pool, code, agent,
		"otc-key-2", map[string]interface{}{"probe": 2}); err != nil {
		t.Fatalf("agent submit: %v", err)
	}
	if c := completedFor(); c != 1 {
		t.Fatalf("agent settled homepage Completed = %d, want 1", c)
	}
	if c := ownerSuccess(); c != 1 {
		t.Fatalf("agent settled Owner success_count = %d, want 1", c)
	}
}

// 4: legacy era (pre-durable earn_task with ref_type='task') is preserved
// and not double-counted against durable settled submissions. Seeded
// legacy earn_task rows count once; durable rows count once.
func TestLegacyEarnTaskCountsOnce(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, setOK, _ := newGateServer()
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)
	setOK(true)

	// Legacy-style earn_task rows (pre-durable authority shape).
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id)
			 VALUES ($1, 'earn_task', 5, 100, 'task', $2)`, agent, code); err != nil {
			t.Fatalf("seed legacy earn_task: %v", err)
		}
	}
	// One durable settled agent submission.
	if _, err := Submit(context.Background(), pool, code, agent,
		"ltc-key-1", map[string]interface{}{"probe": 1}); err != nil {
		t.Fatalf("agent submit: %v", err)
	}

	tasks, err := repository.QueryHomepageTasks(context.Background(), pool, MinOpenBudget)
	if err != nil {
		t.Fatalf("homepage query: %v", err)
	}
	for _, tk := range tasks {
		if tk.Code == code {
			if tk.SuccessCount != 4 {
				t.Fatalf("Completed = %d, want 4 (3 legacy + 1 durable, no double count)", tk.SuccessCount)
			}
			return
		}
	}
	t.Fatal("seeded task missing from homepage board")
}
