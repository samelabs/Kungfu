package service

// PM-001 B: the assignment-invite discovery experiment. Records the
// ACTUAL path an agent takes to find an open assignment addressed to
// it, with and without the notification.

import (
	"context"
	"testing"
)

func TestPM001InviteDiscoveryExperiment(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, lead, "invite-exp", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// Path 1 — with the notification: the invite lands in the outbox
	// (kind 'assign') in the same transaction as the assignment.
	// ask:[] keeps the carrying entry receipt-free — in a two-member
	// room the default pair rule would otherwise ask w implicitly
	res, err := ThreadPost(ctx, pool, lead, code, "task for w", "", "", nil, []int64{},
		&AssignSpec{To: w, Requirements: "do it"}, "iv1")
	if err != nil {
		t.Fatalf("post+assign: %v", err)
	}
	e, a := res["entry"].(int64), res["assign"].(int64)
	var queued int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notify_outbox WHERE account_id=$1 AND kind='assign' AND sent_at IS NULL`,
		w).Scan(&queued)
	if queued != 1 {
		t.Fatalf("invite notification = %d, want 1", queued)
	}

	// Path 2 — the notification is LOST (never dispatched). What does
	// the worker's watch loop see?
	todo, err := TodoList(ctx, pool, w, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(todo["todos"].([]map[string]any)); n != 0 {
		t.Fatalf("unaccepted assignment leaked into the turn list: %d items (A11 violation)", n)
	}
	// thread_list: open_items counts OBLIGATIONS only — the invite is
	// invisible here too
	list, err := ThreadList(ctx, pool, w, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list["threads"].([]map[string]any) {
		if row["open_items"].(int64) != 0 {
			t.Fatalf("open_items counts invites: %v", row)
		}
	}
	// the ONLY compensation path today: thread_get on the room, where
	// the assignments digest shows the open invite with its id
	view, err := ThreadGet(ctx, pool, w, code, "", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range view["assignments"].([]map[string]any) {
		if row["assign"].(int64) == a && row["state"] == "open" && row["to"].(int64) == w {
			found = true
		}
	}
	if !found {
		t.Fatalf("open invite not visible in the room digest: entry=%d assign=%d", e, a)
	}
	// and taking it works with the digest handle
	if _, err := AssignTake(ctx, pool, w, a, `{"ok":1}`, "", "iv-t"); err != nil {
		t.Fatalf("take from digest handle: %v", err)
	}

	// GAP RECORDED (report item): with the notification lost and no
	// obligations in the room, nothing in todo_list or thread_list
	// points the worker at THIS room — the compensation path requires
	// knowing where to look. Minimal proposals go to the PM in the
	// report (thread_list invite counts / an advisory slice).
	_ = e
}
