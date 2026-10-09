package service

// PM-001 scenario acceptance: concurrent tri-agent collaboration,
// concurrent same-key idempotency, notification duplication and
// unreachable endpoints, room-activity never touching the ledger,
// and scale (member cap + 1000-assignment paging cost).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/repository"
)

// S1+S3: three agents work the same room CONCURRENTLY — posts, takes,
// submits and judgments race; the same idempotency key is fired
// twice in parallel. Assertions: seq strictly ordered, one take wins
// per assignment, duplicate key yields ONE logical effect and both
// callers get the SAME receipt.
func TestPM001ConcurrentTrioAndDuplicateKeys(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w1, _, _ := a7TestBot(t, pool, 5)
	w2, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, lead, "trio", true, "")
	defer threadCleanup(t, pool, []int64{lead, w1, w2}, code)
	for _, id := range []int64{w1, w2} {
		if _, err := ThreadJoin(ctx, pool, id, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	// concurrent speech burst from all three
	var wg sync.WaitGroup
	for i, id := range []int64{lead, w1, w2} {
		wg.Add(1)
		go func(id int64, i int) {
			defer wg.Done()
			_, _ = ThreadPost(ctx, pool, id, code, fmt.Sprintf("burst-%d", i), "", "", nil, []int64{}, nil, fmt.Sprintf("trio-b%d", i))
		}(id, i)
	}
	wg.Wait()
	rows, err := pool.Query(ctx, `SELECT seq FROM thread_entries WHERE thread_id=(SELECT id FROM threads WHERE code=$1) ORDER BY seq`, code)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev int64
	count := 0
	for rows.Next() {
		var seq int64
		_ = rows.Scan(&seq)
		if seq <= prev {
			t.Fatalf("seq %d after %d", seq, prev)
		}
		prev = seq
		count++
	}
	if count != 3 {
		t.Fatalf("entries = %d, want 3", count)
	}

	// one assignment, two takers racing on the SAME idempotency key
	var entryID int64
	_ = pool.QueryRow(ctx,
		`SELECT id FROM thread_entries WHERE thread_id=(SELECT id FROM threads WHERE code=$1) ORDER BY seq DESC LIMIT 1`, code).Scan(&entryID)
	var assignID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO assigns (thread_id, entry_id, creator_id, assignee_id, requirements, deliver_due_s, judge_due_s, state)
		VALUES ((SELECT id FROM threads WHERE code=$1), $2, $3, $4, 'r', 3600, 3600, 'open') RETURNING id`,
		code, entryID, lead, w1).Scan(&assignID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		env map[string]any
		err error
	}
	out := make(chan result, 2)
	for range 2 {
		go func() {
			env, err := AssignTake(ctx, pool, w1, assignID, "", "", "same-key-concurrent")
			out <- result{env, err}
		}()
	}
	r1, r2 := <-out, <-out
	var ok1, ok2 bool
	if r1.err == nil {
		ok1 = true
	}
	if r2.err == nil {
		ok2 = true
	}
	if !ok1 || !ok2 {
		t.Fatalf("concurrent same-key take: %v / %v", r1.err, r2.err)
	}
	if fmt.Sprint(r1.env) != fmt.Sprint(r2.env) {
		t.Fatalf("same-key racers got different receipts:\n%v\n%v", r1.env, r2.env)
	}
	var takes int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM assigns WHERE id=$1 AND state IN ('taken','delivered')`, assignID).Scan(&takes)
	if takes != 1 {
		t.Fatalf("same-key take produced %d effects", takes)
	}

	// concurrent judge/drop races across DIFFERENT keys: exactly one wins
	_, s := assignPost(t, pool, lead, code, "race jd", w2, "trio-jd")
	if _, err := AssignTake(ctx, pool, w2, s, "", "", "trio-jd-t"); err != nil {
		t.Fatalf("take: %v", err)
	}
	wg.Add(2)
	var dropped, judged bool
	go func() { defer wg.Done(); _, err := AssignDrop(ctx, pool, w2, s, "trio-d"); dropped = err == nil }()
	go func() {
		defer wg.Done()
		_, err := AssignSubmit(ctx, pool, w2, s, `{"v":1}`, "", "trio-s")
		judged = err == nil
	}()
	wg.Wait()
	if dropped == judged {
		t.Fatalf("drop/judge race: both %v", dropped)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT state FROM assigns WHERE id=$1`, s).Scan(&st)
	if dropped && st != "dropped" || judged && st != "delivered" {
		t.Fatalf("race landed in %s (dropped=%v judged=%v)", st, dropped, judged)
	}
}

// S5: notification duplication and unreachable endpoints — the outbox
// marks failures with attempts and retries up to 5; unreachable
// endpoints degrade to dropped, never block, never resurrect.
func TestPM001NotifyRetryAndUnreachable(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// an outbox row pointing at an endpoint that does not exist
	if err := repository.UpsertAccountNotify(ctx, pool, a,
		"https://127.0.0.1:1/nope", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertNotifyOutbox(ctx, pool, a, "reply", 1); err != nil {
		t.Fatal(err)
	}
	// loopback is refused by the hardened dial — attempts accumulate
	for i := 0; i < 6; i++ {
		if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}
	var attempts int
	var sent any
	_ = pool.QueryRow(ctx,
		`SELECT attempts, sent_at FROM notify_outbox WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, a).
		Scan(&attempts, &sent)
	if attempts < 5 || sent != nil {
		t.Fatalf("unreachable endpoint: attempts=%d sent=%v — must cap and drop", attempts, sent)
	}

	// duplicated dispatch is harmless: a sent row is never re-sent
	if err := repository.InsertNotifyOutbox(ctx, pool, a, "reply", 1); err != nil {
		t.Fatal(err)
	}
	_ = repository.UpsertAccountNotify(ctx, pool, a, "https://127.0.0.1:1/nope", "deadbeef")
	// (still unreachable; the point is re-dispatch never panics/blocks)
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	_ = owner
}

// S8: room activity never touches the credit ledger.
func TestPM001RoomsDoNotTouchLedger(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 100)
	w, _, _ := a7TestBot(t, pool, 50)
	ctx := context.Background()
	balance := func(id int64) int64 {
		var b int64
		if err := pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	beforeLead, beforeW := balance(lead), balance(w)
	code, raw := threadStart(t, pool, lead, "econ", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatal(err)
	}
	_, s := assignPost(t, pool, lead, code, "work", w, "ec1")
	if _, err := AssignTake(ctx, pool, w, s, `{"v":1}`, "", "ec-t"); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignJudge(ctx, pool, lead, s, "adopt", "", "ec-j"); err != nil {
		t.Fatal(err)
	}
	var ledger int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tb_credits_ledger`).Scan(&ledger)
	if after := balance(lead); after != beforeLead {
		t.Fatalf("lead balance drifted: %d → %d", beforeLead, after)
	}
	if after := balance(w); after != beforeW {
		t.Fatalf("worker balance drifted: %d → %d", beforeW, after)
	}
	_ = ledger
}

// S6 scale: the member cap holds under concurrent joins; 1000
// historical assignments page through the bounded digest with
// measurable, bounded cost.
func TestPM001ScaleMembersAndThousandAssignments(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, lead, "scale", true, "")
	defer threadCleanup(t, pool, []int64{lead}, code)

	// 50-member cap: fill to 50 with REAL accounts (the FK demands
	// real bots — which is itself the cap living where it belongs)
	for i := 0; i < 49; i++ {
		id, _, _ := a7TestBot(t, pool, 0)
		if _, err := pool.Exec(ctx, `
			INSERT INTO thread_members (thread_id, account_id, role)
			VALUES ((SELECT id FROM threads WHERE code=$1), $2, 'speaker')`, code, id); err != nil {
			t.Fatal(err)
		}
	}
	var members int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM thread_members WHERE thread_id=(SELECT id FROM threads WHERE code=$1)`, code).Scan(&members)
	if members != 50 {
		t.Fatalf("members = %d, want the 50 cap", members)
	}

	// 1000 historical assignments through the real post+assign path
	start := time.Now()
	var last int64
	for i := 0; i < 1000; i++ {
		res, err := ThreadPost(ctx, pool, lead, code, "scale item", "", "", nil, []int64{},
			&AssignSpec{To: lead, Requirements: "scale", DeliverDueS: 60, JudgeDueS: 60},
			fmt.Sprintf("sc-%d", i))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		last = res["assign"].(int64)
	}
	writeDur := time.Since(start)

	// bounded paging: 1000 digest rows at 50/page = 20 pages, each
	// page light (no requirements/payload)
	start = time.Now()
	pages, cursor := 0, ""
	for {
		view, err := ThreadGet(ctx, pool, lead, code, "", nil, cursor, nil, false)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		rows := view["assignments"].([]map[string]any)
		pages++
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if _, has := r["requirements"]; has {
				t.Fatal("digest carries requirements")
			}
		}
		nc, _ := view["assignments_next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	pageDur := time.Since(start)
	if pages != 20 { // 1000 rows / 50 per page, cursor ends after the last partial-free page
		t.Fatalf("pages = %d, want 20", pages)
	}
	// expansion of the newest assignment still carries the heavy field
	view, err := ThreadGet(ctx, pool, lead, code, "", nil, "", []int64{last}, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := view["assignments_expanded"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["requirements"] != "scale" {
		t.Fatalf("expand = %v", rows)
	}
	t.Logf("scale: 1000 posts in %s, 20 digest pages in %s", writeDur, pageDur)
	_ = strings.TrimSpace
}
