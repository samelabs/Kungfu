package service

// PM-002: invite counts across the full lifecycle, room discovery at
// scale, assignment-location cost inside a 1000-assignment room,
// discoverability beyond next[], and MCP-entry parameter contracts.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

func invitesFor(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (any, error)
}) {
	t.Helper()
}

func inviteCount(t *testing.T, pool pgPool, room string, acct int64) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(c.n, 0) FROM threads t
		LEFT JOIN (SELECT a.thread_id, COUNT(*) n FROM assigns a
		           JOIN thread_members m ON m.thread_id=a.thread_id AND m.account_id=$2
		           WHERE a.assignee_id=$2 AND a.state='open' GROUP BY a.thread_id) c
		ON c.thread_id = t.id WHERE t.code=$1`, room, acct).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func listInvites(t *testing.T, ctx context.Context, pool pgPool, acct int64) map[string]int64 {
	t.Helper()
	res, err := ThreadList(ctx, pool, acct, "", "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, row := range res["threads"].([]map[string]any) {
		code := row["thread"].(map[string]any)["code"].(string)
		out[code] = row["open_invites"].(int64)
	}
	return out
}

// B-1 lifecycle: count appears on creation, drops on take/void/leave
// and close, never becomes an obligation.
func TestPM002InviteCountLifecycle(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, lead, "b1", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatal(err)
	}
	if got := listInvites(t, ctx, pool, w)[code]; got != 0 {
		t.Fatalf("initial invites = %d", got)
	}
	invite := func(idem string) int64 {
		res, err := ThreadPost(ctx, pool, lead, code, "invite", "", "", nil, []int64{},
			&AssignSpec{To: w, Requirements: "r"}, idem)
		if err != nil {
			t.Fatalf("invite post: %v", err)
		}
		return res["assign"].(int64)
	}
	a1, a2 := invite("b1-a1"), invite("b1-a2")
	if got := listInvites(t, ctx, pool, w)[code]; got != 2 {
		t.Fatalf("invites = %d, want 2", got)
	}
	// not an obligation
	todo, _ := TodoList(ctx, pool, w, 0, "")
	if n := len(todo["todos"].([]map[string]any)); n != 0 {
		t.Fatalf("invite leaked into obligations: %d", n)
	}
	// take drops one
	if _, err := AssignTake(ctx, pool, w, a1, "", "", "b1-t"); err != nil {
		t.Fatal(err)
	}
	if got := listInvites(t, ctx, pool, w)[code]; got != 1 {
		t.Fatalf("after take = %d, want 1", got)
	}
	// void drops the other
	if _, err := AssignVoid(ctx, pool, lead, a2, "b1-v"); err != nil {
		t.Fatal(err)
	}
	if got := listInvites(t, ctx, pool, w)[code]; got != 0 {
		t.Fatalf("after void = %d", got)
	}
	// recreate + leave: membership gate closes the count
	invite("b1-a3")
	if got := listInvites(t, ctx, pool, w)[code]; got != 1 {
		t.Fatalf("recreated = %d, want 1", got)
	}
	if _, err := ThreadLeave(ctx, pool, w, code, "b1-l"); err != nil {
		t.Fatal(err)
	}
	inv := listInvites(t, ctx, pool, w)
	if c, ok := inv[code]; ok && c != 0 {
		t.Fatalf("left room still counts invites: %d", c)
	}
	// departure VOIDED the invite (§6.2/R-18: the departing member's
	// undelivered assignments void) — rejoining shows 0, not 1: the
	// count tracks live invites, and this one is dead
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatal(err)
	}
	if got := listInvites(t, ctx, pool, w)[code]; got != 0 {
		t.Fatalf("rejoined shows %d, want 0 (invite voided by departure)", got)
	}
	// a fresh invite counts again; close kills it
	invite("b1-a4")
	if got := listInvites(t, ctx, pool, w)[code]; got != 1 {
		t.Fatalf("fresh invite after rejoin = %d, want 1", got)
	}
	if _, err := ThreadClose(ctx, pool, lead, code, "b1-c"); err != nil {
		t.Fatal(err)
	}
	if got := listInvites(t, ctx, pool, w)[code]; got != 0 {
		t.Fatalf("closed room counts invites: %d", got)
	}
}

// B-1 scale (scope note, PM-003 D): the 99 background rooms are
// owned by LEAD; the worker is a member of the TARGET room only.
// This proves single-membership discovery, NOT "worker sifting 99 of
// its own rooms" — that claim is not made here. The measured costs
// (thread_list discovery, digest paging) are per-room-list-call and
// hold regardless of membership breadth.
func TestPM002InviteDiscoveryAtScale(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// PRD §2 caps an account at 100 open rooms: 99 background + the
	// keyed target = exactly the cap
	var codes []string
	for i := 0; i < 99; i++ {
		code, _ := threadStart(t, pool, lead, fmt.Sprintf("room-%03d", i), false, "")
		codes = append(codes, code)
	}
	defer func() {
		for _, c := range codes {
			_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code=$1)`, c)
			_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code=$1`, c)
		}
	}()
	target := codes[57]
	// worker joins only the target room
	tk, rawTk := threadStart(t, pool, lead, target+"-key", true, "")
	_ = tk
	if _, err := ThreadJoin(ctx, pool, w, rawTk, ""); err != nil {
		t.Fatal(err)
	}
	// the OLD invite for w FIRST — the 1000 newer history rows will
	// bury it (the deep-paging worst case)
	oldRes, err := ThreadPost(ctx, pool, lead, tk, "old invite", "", "", nil, []int64{},
		&AssignSpec{To: w, Requirements: "find me"}, "b2-old")
	if err != nil {
		t.Fatal(err)
	}
	oldAssign := oldRes["assign"].(int64)
	// 1000 newer history rows (addressed to lead, not w)
	for i := 0; i < 1000; i++ {
		if _, err := ThreadPost(ctx, pool, lead, tk, "hist", "", "", nil, []int64{},
			&AssignSpec{To: lead, Requirements: "h"}, fmt.Sprintf("b2-h%d", i)); err != nil {
			t.Fatalf("hist %d: %v", i, err)
		}
	}

	// discovery: one thread_list call names the room
	inv := listInvites(t, ctx, pool, w)
	found := ""
	for c, n := range inv {
		if n == 1 {
			found = c
		}
	}
	if found != tk {
		t.Fatalf("discovery found %q, want %q (map %v)", found, tk, inv)
	}
	// common case: a FRESH invite lands on digest page 1 (DESC by id)
	res, err := ThreadPost(ctx, pool, lead, tk, "fresh invite", "", "", nil, []int64{},
		&AssignSpec{To: w, Requirements: "fresh"}, "b2-inv")
	if err != nil {
		t.Fatal(err)
	}
	view, err := ThreadGet(ctx, pool, w, tk, "", nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	gotID := int64(-1)
	for _, r := range view["assignments"].([]map[string]any) {
		if r["state"] == "open" && r["to"].(int64) == w {
			gotID = r["assign"].(int64)
		}
	}
	if gotID != res["assign"].(int64) {
		t.Fatalf("page-1 location: got %d want %d", gotID, res["assign"].(int64))
	}
	// worst case: the OLD invite sits under 1000 newer rows — walk
	// the digest and count the pages (the cost the PM asked measured)
	pages, cursor := 0, ""
	for {
		v, err := ThreadGet(ctx, pool, w, tk, "", nil, cursor, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		hit := false
		for _, r := range v["assignments"].([]map[string]any) {
			if r["assign"].(int64) == oldAssign {
				hit = true
			}
		}
		if hit {
			break
		}
		nc, _ := v["assignments_next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	t.Logf("worst-case old-invite location: %d digest pages (50 light rows each)", pages)
	if pages < 20 {
		t.Fatalf("expected the deep-paging worst case, found at page %d", pages)
	}
	// and the take works from the located handle
	if _, err := AssignTake(ctx, pool, w, oldAssign, `{"found":true}`, "", "b2-old-t"); err != nil {
		t.Fatal(err)
	}
}

// B-2: obligations beyond next[] stay discoverable via todo_list.
func TestPM002BeyondNextHints(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, lead, "b2x", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatal(err)
	}
	// five obligations for w: four receipts + one taken assignment
	for i := 0; i < 4; i++ {
		if _, err := ThreadPost(ctx, pool, lead, code, fmt.Sprintf("ask %d", i), "", "", nil, []int64{w}, nil, fmt.Sprintf("b2x-a%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, s := assignPost(t, pool, lead, code, "work", w, "b2x-w")
	if _, err := AssignTake(ctx, pool, w, s, "", "", "b2x-t"); err != nil {
		t.Fatal(err)
	}
	view, err := ThreadGet(ctx, pool, w, code, "", nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(view["next"].([]map[string]any)); n > 3 {
		t.Fatalf("next hints = %d, cap 3", n)
	}
	todo, err := TodoList(ctx, pool, w, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(todo["todos"].([]map[string]any)); n != 5 {
		t.Fatalf("todo = %d, want all 5 obligations enumerable", n)
	}
}

// PM-002 test repair: the REAL admin disable path racing REAL room
// writes (join, post) through the service layer — both call RESULTS
// checked, final DB state asserted.
func TestPM002RealWritesRaceRealDisable(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	principal := &admin.Principal{Admin: &model.Admin{ID: 1}, Permissions: []string{"*"}}
	for round := 0; round < 6; round++ {
		victim, _, _ := a7TestBot(t, pool, 5)
		code, raw := threadStart(t, pool, lead, fmt.Sprintf("rr-%d", round), true, "")
		type outcome struct {
			joinErr, postErr, disErr error
		}
		o := outcome{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			o.disErr = admin.DisablePlatformAccount(ctx, pool, principal, victim)
		}()
		// real business writes racing the disable
		o.joinErr = ThreadJoinErr(ctx, pool, victim, raw, "")
		o.postErr = ThreadPostErr(ctx, pool, victim, code, "race post", "", "", nil, []int64{}, nil, "")
		<-done

		// whatever the interleaving: a disabled account ends clean
		var members, pending, dangling int
		_ = pool.QueryRow(ctx, `
			SELECT count(*) FROM thread_members m JOIN tb_bots b ON b.id=m.account_id
			WHERE m.account_id=$1 AND b.status='disabled'`, victim).Scan(&members)
		_ = pool.QueryRow(ctx, `
			SELECT count(*) FROM thread_receipts WHERE account_id=$1 AND state='pending'`, victim).Scan(&pending)
		_ = pool.QueryRow(ctx, `
			SELECT count(*) FROM assigns WHERE state IN ('open','taken') AND (assignee_id=$1 OR creator_id=$1)`, victim).Scan(&dangling)
		if members != 0 || pending != 0 || dangling != 0 {
			t.Fatalf("round %d: disabled residue members=%d pending=%d dangling=%d (join=%v post=%v disable=%v)",
				round, members, pending, dangling, o.joinErr, o.postErr, o.disErr)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code=$1)`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code=$1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, victim)
	}
	_ = invitesFor
}

// economics isolation with REAL ledger assertions (PM-002 §4).
func TestPM002LedgerUntouched(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 100)
	w, _, _ := a7TestBot(t, pool, 50)
	ctx := context.Background()
	var txBefore, txAfter int64
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tb_transactions`).Scan(&txBefore)
	bal := func(id int64) int64 {
		var b int64
		_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, id).Scan(&b)
		return b
	}
	lb, wb := bal(lead), bal(w)
	code, raw := threadStart(t, pool, lead, "eco", true, "")
	defer threadCleanup(t, pool, []int64{lead, w}, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatal(err)
	}
	_, s := assignPost(t, pool, lead, code, "work", w, "eco-a")
	if _, err := AssignTake(ctx, pool, w, s, `{"v":1}`, "", "eco-t"); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignJudge(ctx, pool, lead, s, "adopt", "", "eco-j"); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tb_transactions`).Scan(&txAfter)
	if txAfter != txBefore {
		t.Fatalf("ledger rows drifted: %d → %d", txBefore, txAfter)
	}
	if bal(lead) != lb || bal(w) != wb {
		t.Fatalf("balances drifted: lead %d→%d worker %d→%d", lb, bal(lead), wb, bal(w))
	}
}

type pgPool = *pg.Pool

func ThreadJoinErr(ctx context.Context, pool pgPool, botID int64, key, idem string) error {
	_, err := ThreadJoin(ctx, pool, botID, key, idem)
	return err
}
func ThreadPostErr(ctx context.Context, pool pgPool, botID int64, thread, content, mem, summary string, replyTo *int64, ask []int64, assign *AssignSpec, idem string) error {
	_, err := ThreadPost(ctx, pool, botID, thread, content, mem, summary, replyTo, ask, assign, idem)
	return err
}

// ── PM-003 B: the old-invite location friction, before and after.
// The worker's open invite is buried under 1000 OTHER agents' open
// assignments (worse than PM-002's self-assigned history). ──
func TestPM003MineOpenLocator(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	other, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, lead, "pm3", true, "")
	defer threadCleanup(t, pool, []int64{lead, w, other}, code)
	for _, id := range []int64{w, other} {
		if _, err := ThreadJoin(ctx, pool, id, raw, ""); err != nil {
			t.Fatal(err)
		}
	}
	// the worker's OLD open invite first
	res, err := ThreadPost(ctx, pool, lead, code, "your invite", "", "", nil, []int64{},
		&AssignSpec{To: w, Requirements: "find me"}, "p3-inv")
	if err != nil {
		t.Fatal(err)
	}
	want := res["assign"].(int64)
	// 1000 NEWER open assignments addressed to ANOTHER member
	for i := 0; i < 1000; i++ {
		if _, err := ThreadPost(ctx, pool, lead, code, "noise", "", "", nil, []int64{},
			&AssignSpec{To: other, Requirements: "n"}, fmt.Sprintf("p3-n%d", i)); err != nil {
			t.Fatalf("noise %d: %v", i, err)
		}
	}

	// BEFORE (filter off): worst-case page-walk to locate the invite
	pages := 0
	cursor := ""
	for {
		v, err := ThreadGet(ctx, pool, w, code, "", nil, cursor, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		hit := false
		for _, r := range v["assignments"].([]map[string]any) {
			if r["assign"].(int64) == want {
				hit = true
			}
		}
		if hit {
			break
		}
		nc, _ := v["assignments_next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}

	// AFTER (filter on): page 1 carries the invite and ONLY mine-open rows
	v, err := ThreadGet(ctx, pool, w, code, "", nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rows := v["assignments"].([]map[string]any)
	if len(rows) != 1 || rows[0]["assign"].(int64) != want || rows[0]["state"] != "open" {
		t.Fatalf("filtered page1 = %v, want exactly the invite %d", rows, want)
	}
	t.Logf("location cost: unfiltered worst case = %d digest pages; assignments_mine_open=true = 1 page", pages)
	if pages < 20 {
		t.Fatalf("expected deep paging before the fix, got %d", pages)
	}

	// expansion by id is untouched by the filter
	ex, err := ThreadGet(ctx, pool, w, code, "", nil, "", []int64{want}, true)
	if err != nil {
		t.Fatal(err)
	}
	er := ex["assignments_expanded"].([]any)
	if len(er) != 1 || er[0].(map[string]any)["requirements"] != "find me" {
		t.Fatalf("expand under filter: %v", er)
	}

	// the other member sees ONLY its own 1000, not the worker's invite
	vo, err := ThreadGet(ctx, pool, other, code, "", nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range vo["assignments"].([]map[string]any) {
		if r["assign"].(int64) == want {
			t.Fatal("filter leaked another member's invite")
		}
	}
	// take works from the filtered handle
	if _, err := AssignTake(ctx, pool, w, want, `{"found":true}`, "", "p3-t"); err != nil {
		t.Fatal(err)
	}
	// after take the filtered view is empty for the worker
	v2, err := ThreadGet(ctx, pool, w, code, "", nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(v2["assignments"].([]map[string]any)); n != 0 {
		t.Fatalf("post-take filtered view = %d, want 0", n)
	}
	// authorization unchanged: a non-member cannot use the filter
	outsider, _, _ := a7TestBot(t, pool, 5)
	if _, err := ThreadGet(ctx, pool, outsider, code, "", nil, "", nil, true); err == nil {
		t.Fatal("non-member must be rejected regardless of the filter")
	} else {
		threadErrIs(t, err, 403, "NOT_MEMBER")
	}
}

// ── PM-003 C: deactivation race — proof-of-effect hardening. ──
func TestPM003RealWritesRaceRealDisableHardened(t *testing.T) {
	pool := revisionTestPool(t)
	lead, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	// the audit trail requires a REAL admin row (FK on actor) — seed
	// one; its credential surface is not under test
	var adminID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO tb_admins (username, display_name, password_hash)
		VALUES ('pm3-race', 'PM-003', 'x') RETURNING id`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM tb_admins WHERE id=$1`, adminID) }()
	principal := &admin.Principal{Admin: &model.Admin{ID: adminID, Username: "pm3-race"}, Permissions: []string{"*"}}
	var sawJoin, sawPost, races int
	for round := 0; round < 8; round++ {
		victim, _, _ := a7TestBot(t, pool, 5)
		code, raw := threadStart(t, pool, lead, fmt.Sprintf("p3r-%d", round), true, "")
		// the three run CONCURRENTLY — the race is the test
		var joinErr, postErr, disErr error
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); joinErr = ThreadJoinErr(ctx, pool, victim, raw, "") }()
		go func() {
			defer wg.Done()
			postErr = ThreadPostErr(ctx, pool, victim, code, "race post", "", "", nil, []int64{}, nil, "")
		}()
		go func() { defer wg.Done(); disErr = admin.DisablePlatformAccount(ctx, pool, principal, victim) }()
		wg.Wait()
		// the disable itself MUST succeed — a silent failure would
		// make the zero-residue assertions vacuous
		if disErr != nil {
			t.Fatalf("round %d: DisablePlatformAccount failed: %v", round, disErr)
		}
		races++
		if joinErr == nil {
			sawJoin++
		}
		if postErr == nil {
			sawPost++
		}
		// join/post outcomes must be legal: success or a guard rejection
		for _, e := range []error{joinErr, postErr} {
			if e == nil {
				continue
			}
			msg := e.Error()
			legal := strings.Contains(msg, "UNAUTHORIZED") || strings.Contains(msg, "KEY_INVALID") ||
				strings.Contains(msg, "NOT_MEMBER") || strings.Contains(msg, "THREAD_CLOSED") ||
				strings.Contains(msg, "IDEMPOTENCY") ||
				strings.Contains(msg, "INTERNAL_ERROR") // serialization/deadlock loser of a true race
			if !legal {
				t.Fatalf("round %d: unexpected write failure class: %v", round, e)
			}
		}
		// independent read-back: the account IS disabled
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM tb_bots WHERE id=$1`, victim).Scan(&status); err != nil {
			t.Fatalf("round %d: status read: %v", round, err)
		}
		if status != "disabled" {
			t.Fatalf("round %d: final status = %q", round, status)
		}
		// zero residue — every query error checked
		var members, pending, dangling int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM thread_members m JOIN tb_bots b ON b.id=m.account_id
			WHERE m.account_id=$1 AND b.status='disabled'`, victim).Scan(&members); err != nil {
			t.Fatalf("members read: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM thread_receipts WHERE account_id=$1 AND state='pending'`, victim).Scan(&pending); err != nil {
			t.Fatalf("receipts read: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM assigns WHERE state IN ('open','taken') AND (assignee_id=$1 OR creator_id=$1)`, victim).Scan(&dangling); err != nil {
			t.Fatalf("assigns read: %v", err)
		}
		if members != 0 || pending != 0 || dangling != 0 {
			t.Fatalf("round %d: residue members=%d pending=%d dangling=%d", round, members, pending, dangling)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code=$1)`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code=$1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, victim)
	}
	t.Logf("races=%d join-ok=%d post-ok=%d (the rest legally rejected by the active-account guard)", races, sawJoin, sawPost)
}
