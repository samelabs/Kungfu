package service

// Task 1.2 (WO-32) — §8 opportunity discovery: an opportunity is a
// restricted task naming the agent that is open, still offerable
// (slots, eligibility) and not held under an active claim — plus the
// untaken thread assignments addressed to the agent. The counts live
// in todo_list's opportunities projection; work_list
// offered_to_me=true enumerates the task side with the same paging
// as the default listing. Opportunities bind no one: they never
// enter todos.

import (
	"context"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// oppCounts reads todo_list's opportunities block for one agent.
func oppCounts(t *testing.T, pool *pg.Pool, agent int64) (tasks, assignments int64, next []map[string]any) {
	t.Helper()
	res, err := TodoList(context.Background(), pool, agent, 0, "")
	if err != nil {
		t.Fatalf("todo_list: %v", err)
	}
	opp, _ := res["opportunities"].(map[string]any)
	if opp == nil {
		t.Fatalf("opportunities missing from todo_list result: %#v", res)
	}
	tasks, _ = opp["tasks"].(int64)
	assignments, _ = opp["assignments"].(int64)
	next, _ = res["next"].([]map[string]any)
	return tasks, assignments, next
}

// TestOpportunityCountsFollowLifecycle: the tasks count moves with
// claim, release, expiry, slot exhaustion, close and deactivation —
// and only restricted work named to the agent ever counts.
func TestOpportunityCountsFollowLifecycle(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 1_000_000)
	named := pubSeedBot(t, pool, 0)
	other := pubSeedBot(t, pool, 0) // named elsewhere, never for `named`
	ctx := context.Background()

	r1 := audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, named)), 100)
	// an open task is not an opportunity — only restricted work is
	audOpen(t, pool, publisher, pubContract(""), 100)
	// restricted to someone else: invisible to `named`
	audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, other)), 100)

	tasks, assignments, next := oppCounts(t, pool, named)
	if tasks != 1 || assignments != 0 {
		t.Fatalf("opportunities = %d/%d, want 1/0 (only the task naming the agent)", tasks, assignments)
	}
	if len(next) != 1 || next[0]["tool"] != "work_list" {
		t.Fatalf("next hint = %#v, want one work_list offered_to_me hint", next)
	}

	// claim: the held task is no longer offered
	claim, err := ClaimTask(ctx, pool, named, r1, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 0 {
		t.Fatalf("after claim tasks = %d, want 0", tasks)
	}
	// release: the opportunity returns
	if _, err := ReleaseClaim(ctx, pool, named, claim.ClaimID.Int64(), time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("after release tasks = %d, want 1", tasks)
	}

	// expiry: the re-claimed task returns once the claim expires
	if _, err := ClaimTask(ctx, pool, named, r1, time.Now()); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tb_task_claim SET expires_at = NOW() - interval '1 second'
		WHERE task_id = (SELECT id FROM tb_task WHERE code = $1)
		  AND agent_id = $2 AND status = 'active'`, r1, named); err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("after expiry tasks = %d, want 1 (an expired claim no longer holds the offer)", tasks)
	}
	// and the sweeper makes the expiry real
	if n, err := ExpireClaims(ctx, pool, time.Now(), 100); err != nil || n != 1 {
		t.Fatalf("ExpireClaims = %d err=%v, want 1", n, err)
	}

	// slot exhaustion: one price in the budget, held by the agent's
	// own claim, is not offerable
	r2c := restrictedContract(audBotName(t, pool, named))
	r2c.Price = 1000
	r2 := audOpen(t, pool, publisher, r2c, 1000)
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 2 {
		t.Fatalf("with r2 tasks = %d, want 2", tasks)
	}
	if _, err := ClaimTask(ctx, pool, named, r2, time.Now()); err != nil {
		t.Fatalf("claim r2: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("after r2 claim tasks = %d, want 1 (slots held)", tasks)
	}

	// close: the remaining task stops being offered
	r3 := audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, named)), 100)
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 2 {
		t.Fatalf("with r3 tasks = %d, want 2", tasks)
	}
	if _, err := CloseTask(ctx, pool, publisher, r3); err != nil {
		t.Fatalf("close: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("after close tasks = %d, want 1", tasks)
	}

	// deactivation: a disabled agent is offered nothing; re-enabling
	// restores the offer (the audience itself never changed)
	if _, err := pool.Exec(ctx, `UPDATE tb_bots SET status = 'disabled' WHERE id = $1`, named); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 0 {
		t.Fatalf("disabled agent tasks = %d, want 0", tasks)
	}
	if _, err := pool.Exec(ctx, `UPDATE tb_bots SET status = 'active' WHERE id = $1`, named); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("re-enabled agent tasks = %d, want 1", tasks)
	}

	// the eligibility rule carries over: at the rejection cap the
	// task stops being an opportunity
	r4 := audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, named)), 100)
	for i := 0; i < 5; i++ {
		seedSubmission(t, pool, r4, named, task.SubRejected)
	}
	if tasks, _, _ = oppCounts(t, pool, named); tasks != 1 {
		t.Fatalf("after cap tasks = %d, want 1 (r4 capped away, r1 remains)", tasks)
	}

	for _, code := range []string{r1, r2, r3, r4} {
		tr, _ := repository.FindTaskByCode(ctx, pool, code)
		if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
			t.Fatalf("CheckInvariants(%s): %v", code, err)
		}
	}
}

// TestTodoOpportunitiesAssignmentsSide: the assignments count reuses
// the thread_list.open_invites query — an open assignment addressed
// to the agent counts once, taking it ends the opportunity, and with
// only assignments left the hint points at thread_list.
func TestTodoOpportunitiesAssignmentsSide(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	code, raw := threadStart(t, pool, lead, "wo32-invites", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// zero is reported like any other number, and no hint fires
	if tasks, assignments, next := oppCounts(t, pool, w); tasks != 0 || assignments != 0 || len(next) != 0 {
		t.Fatalf("fresh agent opportunities = %d/%d next=%v, want 0/0 and no hint", tasks, assignments, next)
	}

	_, assignID := assignPostAsk(t, pool, lead, code, "w, take this", w, true, "wo32a1")
	if tasks, assignments, next := oppCounts(t, pool, w); tasks != 0 || assignments != 1 {
		t.Fatalf("after invite opportunities = %d/%d, want 0/1", tasks, assignments)
	} else if len(next) != 1 || next[0]["tool"] != "thread_list" {
		t.Fatalf("next hint = %#v, want one thread_list hint", next)
	}
	// the invite is a pointer, never an obligation: todos holds no
	// item for it
	res, err := TodoList(ctx, pool, w, 0, "")
	if err != nil {
		t.Fatalf("todo_list: %v", err)
	}
	for _, it := range res["todos"].([]map[string]any) {
		if it["kind"] == "deliver" {
			t.Fatalf("untaken assignment became an obligation: %#v", it)
		}
	}

	if _, err := AssignTake(ctx, pool, w, assignID, "", "", "wo32tk"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if tasks, assignments, next := oppCounts(t, pool, w); tasks != 0 || assignments != 0 || len(next) != 0 {
		t.Fatalf("after take opportunities = %d/%d next=%v, want 0/0 (the obligation side takes over)", tasks, assignments, next)
	}
}

// TestOfferedWorkPaging: offered_to_me enumerates exactly the
// agent's opportunities with the standard paging contract.
func TestOfferedWorkPaging(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 1_000_000)
	named := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// 25 offered tasks plus one open task and one restricted task
	// for someone else — neither may appear
	for i := 0; i < 25; i++ {
		audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, named)), 100)
	}
	audOpen(t, pool, publisher, pubContract(""), 100)
	audOpen(t, pool, publisher, restrictedContract(audBotName(t, pool, stranger)), 100)

	var seen []string
	for page := 1; page <= 3; page++ {
		items, total, err := ListWork(ctx, pool, named, time.Now(), WorkListFilter{OfferedToMe: true, Page: page, PageSize: 10})
		if err != nil {
			t.Fatalf("offered page %d: %v", page, err)
		}
		if total != 25 {
			t.Fatalf("page %d total = %d, want 25", page, total)
		}
		want := 10
		if page == 3 {
			want = 5
		}
		if len(items) != want {
			t.Fatalf("page %d items = %d, want %d", page, len(items), want)
		}
		for _, it := range items {
			if it["audience"] != task.AudienceRestricted {
				t.Fatalf("offered row audience = %#v, want restricted", it["audience"])
			}
			seen = append(seen, it["code"].(string))
		}
	}
	if len(seen) != 25 || len(uniqueStrings(seen)) != 25 {
		t.Fatalf("offered pages returned %d rows (%d unique), want 25 unique", len(seen), len(uniqueStrings(seen)))
	}
	// a held claim removes its task from the enumeration, and the
	// todo_list count equals the enumeration
	if _, err := ClaimTask(ctx, pool, named, seen[0], time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	items, total, err := ListWork(ctx, pool, named, time.Now(), WorkListFilter{OfferedToMe: true, PageSize: 100})
	if err != nil || total != 24 || len(items) != 24 {
		t.Fatalf("after claim offered = %d/%d err=%v, want 24/24", len(items), total, err)
	}
	if tasks, _, _ := oppCounts(t, pool, named); tasks != 24 {
		t.Fatalf("todo tasks = %d, want 24 (equal to the offered enumeration)", tasks)
	}
}

func uniqueStrings(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
