package service

// Task 1.1 §8 turn fusion: an active work claim is a Deliver
// obligation of the account, so todo_list carries it beside the
// thread kinds — same ordering, same keyset cursor, same "settles
// with the fact" rule. The projection must never reveal the task's
// publisher (task-spec §10 item 7).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/task"
)

// taskTodoRows returns the todo rows of kind deliver that are Task
// engagements (task field set).
func taskTodoRows(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	rows := res["todos"].([]map[string]any)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if code, ok := r["task"].(string); ok && code != "" {
			out = append(out, r)
		}
	}
	return out
}

func TestTodoListCarriesTaskClaims(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	res, err := TodoList(ctx, pool, agent, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	rows := taskTodoRows(t, res)
	if len(rows) != 1 {
		t.Fatalf("want 1 task deliver item, got %d in %v", len(rows), res["todos"])
	}
	row := rows[0]
	if row["kind"] != "deliver" || row["task"] != code {
		t.Fatalf("row = %v, want kind deliver / task %s", row, code)
	}
	if got := row["claim"].(int64); got != claim.ClaimID.Int64() {
		t.Fatalf("claim handle = %d, want %d", got, claim.ClaimID.Int64())
	}
	if row["next_action"] != "submit" {
		t.Fatalf("next_action = %v, want submit", row["next_action"])
	}
	wantDue := claim.ExpiresAt.UTC().Format("2006-01-02 15:04:05")
	if row["due_at"] != wantDue {
		t.Fatalf("due_at = %v, want %v (claim expires_at)", row["due_at"], wantDue)
	}
	// no thread facts, no author, no publisher identity on the row
	for _, banned := range []string{"thread", "author", "entry", "assign", "seq"} {
		if _, ok := row[banned]; ok {
			t.Fatalf("task row must not carry %q: %v", banned, row)
		}
	}
	blob := fmt.Sprint(res["todos"])
	publisherName := repositoryBotName(t, pool, publisher)
	if containsSubstr(blob, publisherName) {
		t.Fatalf("todo leaked the publisher identity %q", publisherName)
	}
}

func repositoryBotName(t *testing.T, pool *pg.Pool, botID int64) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT bot_name FROM tb_bots WHERE id = $1`, botID).Scan(&name); err != nil {
		t.Fatalf("bot name: %v", err)
	}
	return name
}

func containsSubstr(hay, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestTodoListTaskItemSettlesWithClaim: the item tracks the claim's
// STATUS, not the clock — it is there until the fact that ends the
// claim lands (use, release, or the expiry sweep materializing
// expired), exactly like a thread assignment past its due date.
func TestTodoListTaskItemSettlesWithClaim(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// -- used by a submission --
	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "use-1", Payload: []byte(submitPayloadOK),
		ClaimID: &claim.ClaimID,
	}, testAgentRefKey, time.Now()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rows := taskTodoRows(t, todoOf(t, pool, agent)); len(rows) != 0 {
		t.Fatalf("claim used: want 0 task items, got %v", rows)
	}
	claimTaskReserved(t, pool, code) // invariants

	// -- released --
	if _, err := ReleaseClaim(ctx, pool, agent, claim.ClaimID.Int64(), time.Now()); err == nil {
		t.Fatal("releasing a used claim must fail")
	}
	code2 := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim2, err := ClaimTask(ctx, pool, agent, code2, time.Now())
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if _, err := ReleaseClaim(ctx, pool, agent, claim2.ClaimID.Int64(), time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if rows := taskTodoRows(t, todoOf(t, pool, agent)); len(rows) != 0 {
		t.Fatalf("claim released: want 0 task items, got %v", rows)
	}
	claimTaskReserved(t, pool, code2)

	// -- expiry sweep (an expired-but-active claim stays listed until
	//    the sweeper materializes the fact, mirroring thread assigns) --
	code3 := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		ttl := int64(300)
		c.Claim.TTL = &ttl
	})
	if _, err := ClaimTask(ctx, pool, agent, code3, time.Now()); err != nil {
		t.Fatalf("claim3: %v", err)
	}
	if rows := taskTodoRows(t, todoOf(t, pool, agent)); len(rows) != 1 {
		t.Fatalf("active claim: want 1 task item, got %v", rows)
	}
	past := time.Now().Add(2 * time.Hour)
	if _, err := ExpireClaims(ctx, pool, past, 100); err != nil {
		t.Fatalf("expire sweep: %v", err)
	}
	if rows := taskTodoRows(t, todoOf(t, pool, agent)); len(rows) != 0 {
		t.Fatalf("claim expired: want 0 task items, got %v", rows)
	}
	claimTaskReserved(t, pool, code3)
}

func todoOf(t *testing.T, pool *pg.Pool, bot int64) map[string]any {
	t.Helper()
	res, err := TodoList(context.Background(), pool, bot, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	return res
}

// TestTodoListTaskItemsPageWithThreadItems: task engagements join the
// SAME produced ASC ordering and (produced, branch, row_id) keyset
// cursor as the three thread kinds — a full traversal over pages
// returns each obligation exactly once, none silently omitted.
func TestTodoListTaskItemsPageWithThreadItems(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	agent, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// a thread reply obligation, older than the claims
	code, raw := threadStart(t, pool, owner, "todo-room", true, "")
	defer threadCleanup(t, pool, []int64{owner, agent}, code)
	if _, err := ThreadJoin(ctx, pool, agent, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "agent, your take?", "", "", nil, []int64{agent}, nil, "tq-todo"); err != nil {
		t.Fatalf("post: %v", err)
	}

	publisher := pubSeedBot(t, pool, 10_000)
	codes := []string{
		claimOpenedTask(t, pool, publisher, 1000, nil),
		claimOpenedTask(t, pool, publisher, 1000, nil),
	}
	for _, c := range codes {
		if _, err := ClaimTask(ctx, pool, agent, c, time.Now()); err != nil {
			t.Fatalf("claim %s: %v", c, err)
		}
	}

	// traverse every page; 3 obligations total
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := TodoList(ctx, pool, agent, 0, cursor)
		if err != nil {
			t.Fatalf("todo page: %v", err)
		}
		rows := res["todos"].([]map[string]any)
		if len(rows) == 0 {
			break
		}
		pages++
		for _, r := range rows {
			key := fmt.Sprint(r["kind"], "@", r["thread"], "@", r["task"])
			if seen[key] {
				t.Fatalf("duplicate obligation %q across pages", key)
			}
			seen[key] = true
		}
		nc, _ := res["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
		if pages > 10 {
			t.Fatal("cursor traversal did not terminate")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("traversal saw %d obligations, want 3 (1 reply + 2 task claims): %v", len(seen), seen)
	}
	// cursor grammar accepts branch 4
	res, err := TodoList(ctx, pool, agent, 0, "2000-01-01 00:00:00|4|1")
	if err != nil {
		t.Fatalf("branch-4 cursor: %v", err)
	}
	_ = res
	if _, err := TodoList(ctx, pool, agent, 0, "2000-01-01 00:00:00|5|1"); err == nil {
		t.Fatal("branch 5 must be an invalid cursor")
	}
}

// TestTodoListRoomScopeExcludesTaskClaims: threadID > 0 is the room
// working-set slice; Task engagements are not room facts.
func TestTodoListRoomScopeExcludesTaskClaims(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := ClaimTask(ctx, pool, agent, code, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	res, err := TodoList(ctx, pool, agent, 12345, "")
	if err != nil {
		t.Fatalf("scoped todo: %v", err)
	}
	if rows := taskTodoRows(t, res); len(rows) != 0 {
		t.Fatalf("room-scoped todo must not carry task claims: %v", rows)
	}
}
