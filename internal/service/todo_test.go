package service

// D5 turn-projection tests (A18): the account-level todo list
// aggregates receipts and assignments across rooms, orders by
// production time, drops what settles, and recovers after the
// process state is gone (the list is the same query).

import (
	"context"
	"fmt"
	"testing"
)

func todoKinds(t *testing.T, res map[string]any) []string {
	t.Helper()
	items := res["todos"].([]map[string]any)
	kinds := make([]string, 0, len(items))
	for _, it := range items {
		kinds = append(kinds, fmt.Sprint(it["kind"], "@", it["thread"]))
	}
	return kinds
}

func TestTodoListAggregatesAndRecovers(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// two rooms, obligations for `a` in both
	code1, raw1 := threadStart(t, pool, owner, "room1", true, "")
	code2, raw2 := threadStart(t, pool, b, "room2", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b}, code1, code2)
	for _, join := range []struct {
		thread string
		raw    string
	}{{code1, raw1}, {code2, raw2}} {
		if _, err := ThreadJoin(ctx, pool, a, join.raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	// room1: a taken assignment (deliver item); the ask receipt is
	// fulfilled by the take itself
	_, s1 := assignPostAsk(t, pool, owner, code1, "a, take this", a, true, "tq1")
	if _, err := AssignTake(ctx, pool, a, s1, "", "", "tk1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	// room2: b asks a (reply item)
	askRes, err := ThreadPost(ctx, pool, b, code2, "a, your thoughts?", "", "", nil, []int64{a}, nil, "tq2")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	askEntry := askRes["entry"].(int64)

	res, err := TodoList(ctx, pool, a, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	kinds := todoKinds(t, res)
	wantOne := func(kind, thread string) {
		t.Helper()
		hit := false
		for _, k := range kinds {
			if k == kind+"@"+thread {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("todo missing %s@%s in %v", kind, thread, kinds)
		}
	}
	wantOne("deliver", code1)
	wantOne("reply", code2)
	if len(kinds) != 2 {
		t.Fatalf("todo = %v, want exactly deliver+reply (the taken entry's receipt was fulfilled by take)", kinds)
	}

	// judge item appears for the creator once delivered
	if _, err := AssignSubmit(ctx, pool, a, s1, `{"v":1}`, "", "sm1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	ownerTodo, err := TodoList(ctx, pool, owner, "")
	if err != nil {
		t.Fatalf("owner todo: %v", err)
	}
	ok := false
	for _, k := range todoKinds(t, ownerTodo) {
		if k == "judge@"+code1 {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("creator todo missing judge item: %v", todoKinds(t, ownerTodo))
	}

	// acting clears: a replies in room2, drops... no — reply clears the item
	if _, err := ThreadPost(ctx, pool, a, code2, "here", "", "", &askEntry, nil, nil, "rp1"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	after, err := TodoList(ctx, pool, a, "")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	for _, k := range todoKinds(t, after) {
		if k == "reply@"+code2 {
			t.Fatalf("reply item survived the reply: %v", todoKinds(t, after))
		}
	}

	// A18 recovery: a brand-new "session" is the same query — the
	// list is identical because it is a projection of stored facts
	again, err := TodoList(ctx, pool, a, "")
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	if fmt.Sprint(todoKinds(t, again)) != fmt.Sprint(todoKinds(t, after)) {
		t.Fatalf("projection drifted between reads: %v vs %v", todoKinds(t, again), todoKinds(t, after))
	}

	// empty account: wait + retry_after (§8) — a FRESH account owes
	// nothing (b itself owes the reply a's re-ask just created)
	fresh, _, _ := a7TestBot(t, pool, 5)
	empty, err := TodoList(ctx, pool, fresh, "")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if n := len(empty["todos"].([]map[string]any)); n != 0 || empty["next_action"] != "wait" {
		t.Fatalf("empty todo = %d items, next_action=%v", n, empty["next_action"])
	}
}

func TestTodoListCursorPages(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "pages", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	// 52 reply items
	for i := 0; i < 52; i++ {
		if _, err := ThreadPost(ctx, pool, owner, code, fmt.Sprintf("item %d", i), "", "", nil, []int64{a}, nil, fmt.Sprintf("pg-%d", i)); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	seen := 0
	cursor := ""
	for {
		res, err := TodoList(ctx, pool, a, cursor)
		if err != nil {
			t.Fatalf("todo page: %v", err)
		}
		n := len(res["todos"].([]map[string]any))
		seen += n
		nc, _ := res["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	if seen != 52 {
		t.Fatalf("cursor walk saw %d items, want 52", seen)
	}
}
