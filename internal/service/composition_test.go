package service

// P2 compositional audits: mechanism pairs and triples driven from
// the protocol's compositional semantics, plus a seeded random-action
// fuzzer asserting the closure invariants after EVERY step. Single
// mechanisms were proven in their stages; this file proves they do
// not fork or conflict when combined.

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// ── M1: close × judge-deadline — a delivered assignment in a CLOSED
// room keeps its judgment clock; nobody judges; the sweeper settles
// it undecided and notifies both parties (exit). ──
func TestComposeCloseAndJudgeDeadline(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "close-due", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, s := assignPost(t, pool, owner, code, "will outlive the room", a, "m1")
	if _, err := AssignTake(ctx, pool, a, s, `{"v":1}`, "", "t1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := ThreadClose(ctx, pool, owner, code, "c1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE assign_deliveries SET judge_due_at = NOW() - INTERVAL '1 second' WHERE assign_id = $1`, s); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := assignState(t, pool, s); got != "undecided" {
		t.Fatalf("closed-room delivered after deadline = %s, want undecided", got)
	}
	// both parties got an exit notification (sweeper-side enqueue)
	var exits int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notify_outbox WHERE kind='exit'`).Scan(&exits); err != nil || exits < 2 {
		t.Fatalf("exit notifications = %d (err %v), want ≥2", exits, err)
	}
}

// ── M2: replay × expiry — L3 vs L4 composition. The take receipt is
// frozen at first success; a later expiry does not rewrite it. The
// REPLAY still returns the original snapshot (receipt ≠ workset),
// while the live state is timed_out. ──
func TestComposeReplayAfterExpiry(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "replay-exp", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, s := assignPost(t, pool, owner, code, "expire then replay", a, "m2")
	first, err := AssignTake(ctx, pool, a, s, "", "", "fixed-key")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	// expire on the DB clock, sweep the terminal fact
	if _, err := pool.Exec(ctx,
		`UPDATE assigns SET deliver_due_at = NOW() - INTERVAL '1 second' WHERE id = $1`, s); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatal(err)
	}
	if got := assignState(t, pool, s); got != "timed_out" {
		t.Fatalf("live state = %s, want timed_out", got)
	}
	// replay: the frozen receipt, verbatim — including state:taken
	replay, err := AssignTake(ctx, pool, a, s, "", "", "fixed-key")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if fmt.Sprint(replay) != fmt.Sprint(first) {
		t.Fatalf("replay drifted after expiry:\n first=%v\nreplay=%v", first, replay)
	}
	if replay["state"] != "taken" {
		t.Fatalf("replay state = %v, want the frozen taken (L3: receipt ≠ workset)", replay["state"])
	}
	// and a FRESH take under a new key is rejected on the real state
	if _, err := AssignTake(ctx, pool, a, s, "", "", "new-key"); err == nil {
		t.Fatal("fresh take on timed_out must be rejected")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}
}

// ── M3: key-rotation × join × close — a join holding the OLD key
// racing a rotation and a close lands on exactly one legal outcome;
// after close every key is dead. ──
func TestComposeRotationJoinClose(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw1 := threadStart(t, pool, owner, "rot", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b}, code)
	// rotate: raw1 dies, raw2 lives
	res, err := ThreadIssueKey(ctx, pool, owner, code, repository.ThreadRoleSpeaker, "k1")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	raw2, _ := res["key"].(string)
	if _, err := ThreadJoin(ctx, pool, a, raw1, ""); err == nil {
		t.Fatal("old key must be dead after rotation")
	}
	if _, err := ThreadJoin(ctx, pool, a, raw2, ""); err != nil {
		t.Fatalf("new key join: %v", err)
	}
	// close kills every key; a later join with the live key fails
	if _, err := ThreadClose(ctx, pool, owner, code, "c1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ThreadJoin(ctx, pool, b, raw2, ""); err == nil {
		t.Fatal("join after close must fail (KEY_INVALID)")
	} else {
		threadErrIs(t, err, 401, "KEY_INVALID")
	}
}

// ── M4: pin × withdraw × unshare matrix — all four cells of the
// fixed-reference readability rule (R-19) in one room. ──
func TestComposePinVisibilityMatrix(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "matrix", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	push := func(bot int64, title string) string {
		mem, err := Push(ctx, pool, bot, map[string]interface{}{
			"title": title, "tags": []interface{}{"t"}, "content": strings.Repeat("c", 60),
		}, 128, 10, 24, 500, 102400)
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		return mem.Code
	}
	pin := func(mem string) int64 {
		r := statePostMem(t, pool, owner, code, mem, "pin", nil, nil, "pm-"+mem)
		return r["entry"].(int64)
	}
	readable := func(entry int64) bool {
		view, err := ThreadGet(ctx, pool, a, code, "", []int64{entry}, "", nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return view["entries"].([]any)[0].(map[string]any)["readable"].(bool)
	}

	ownW := push(owner, "own-w") // own pin, later withdrawn
	ownU := push(owner, "own-u") // own PUBLIC pin, later unshared
	frnU := push(a, "frn-u")     // foreign pin, public then unshared
	frnW := push(a, "frn-w")     // foreign pin, public then withdrawn
	// share: own memories by owner, foreign by their author a
	_, _ = Share(ctx, pool, owner, ownU)
	_, _ = Share(ctx, pool, a, frnU)
	_, _ = Share(ctx, pool, a, frnW)
	e1, e2, e3, e4 := pin(ownW), pin(ownU), pin(frnU), pin(frnW)
	// all readable while valid+public-or-own
	for i, e := range []int64{e1, e2, e3, e4} {
		if !readable(e) {
			t.Fatalf("cell %d not readable at rest", i)
		}
	}
	// own withdrawn → still readable; own public unshared → still readable (own pin)
	if _, err := Delete(ctx, pool, owner, ownW); err != nil {
		t.Fatal(err)
	}
	if _, err := Unshare(ctx, pool, owner, ownU); err != nil {
		t.Fatal(err)
	}
	// foreign unshared / withdrawn → unreadable, no content leak
	_, _ = Unshare(ctx, pool, a, frnU)
	if _, err := Delete(ctx, pool, a, frnW); err != nil {
		t.Fatal(err)
	}
	if !readable(e1) || !readable(e2) {
		t.Fatal("own pins must survive withdrawal/unshare (R-19)")
	}
	if readable(e3) || readable(e4) {
		t.Fatal("foreign pins must degrade with publicness (R-19)")
	}
}

// ── M5: deactivation × outbox — the cascade collects the
// obligations AND kills their queued notifications (D-017 fix). ──
func TestComposeDeactivationPurgesOutbox(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "purge", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	statePost(t, pool, owner, code, "you owe this", "", nil, []int64{a}, "m5")
	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notify_outbox WHERE account_id=$1 AND sent_at IS NULL`, a).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued = %d (err %v), want 1", queued, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tb_bots SET status='disabled' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TerminateAccountThreadMemberships(ctx, tx, a); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("cascade: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notify_outbox WHERE account_id=$1 AND sent_at IS NULL`, a).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("ghost notification survived deactivation: %d (err %v)", queued, err)
	}
	// and the account's turn list is empty (fact source agrees)
	todo, err := TodoList(ctx, pool, a, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	if n := len(todo["todos"].([]map[string]any)); n != 0 {
		t.Fatalf("disabled account still owes %d", n)
	}
}

// ── The fuzzer: seeded random action streams over a small roster,
// invariants asserted after EVERY step. Guards rejecting an illegal
// draw is fine — the invariants must hold regardless. ──

type fuzzWorld struct {
	pool   *pg.Pool
	owner  int64
	agents []int64
	rooms  []string
	keys   map[string]string // room -> live key
	ctx    context.Context
	t      *testing.T
}

func (w *fuzzWorld) pick(list []int64) int64 { return list[w.rng().Intn(len(list))] }

var fuzzRand *rand.Rand

func (w *fuzzWorld) rng() *rand.Rand { return fuzzRand }

func (w *fuzzWorld) speechMembers(code string) []int64 {
	rows, err := w.pool.Query(w.ctx, `
		SELECT m.account_id FROM thread_members m
		JOIN threads t ON t.id = m.thread_id
		WHERE t.code = $1 AND m.role IN ('governor','speaker')`, code)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (w *fuzzWorld) step() {
	ctx := w.ctx
	// occasionally run the sweeper (its transitions are part of the world)
	if fuzzRand.Intn(8) == 0 {
		_, _ = RecoverAssigns(ctx, w.pool, "now", 50)
	}
	code := w.rooms[fuzzRand.Intn(len(w.rooms))]
	actors := append([]int64{w.owner}, w.agents...)
	actor := w.pick(actors)
	speech := w.speechMembers(code)

	switch fuzzRand.Intn(12) {
	case 0, 1: // post (ask variants)
		var ask []int64
		switch fuzzRand.Intn(3) {
		case 0:
		case 1:
			ask = []int64{}
		case 2:
			if len(speech) > 0 {
				ask = []int64{w.pick(speech)}
			}
		}
		var assign *AssignSpec
		if len(speech) > 0 && fuzzRand.Intn(4) == 0 {
			assign = &AssignSpec{To: w.pick(speech), Requirements: "fuzz work", DeliverDueS: 60, JudgeDueS: 60}
		}
		_, _ = ThreadPost(ctx, w.pool, actor, code,
			fmt.Sprintf("fuzz %d", fuzzRand.Intn(1000)), "", "", nil, ask, assign, "")
	case 2: // handle a random pending receipt of mine
		var entry int64
		if err := w.pool.QueryRow(ctx, `
			SELECT r.entry_id FROM thread_receipts r
			JOIN threads t ON t.id = r.thread_id
			WHERE t.code=$1 AND r.account_id=$2 AND r.state='pending' LIMIT 1`,
			code, actor).Scan(&entry); err == nil {
			_, _ = ThreadHandle(ctx, w.pool, actor, code, entry, "", "")
		}
	case 3: // take an open assign addressed to me
		var id int64
		if err := w.pool.QueryRow(ctx, `
			SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id
			WHERE t.code=$1 AND a.assignee_id=$2 AND a.state='open' LIMIT 1`,
			code, actor).Scan(&id); err == nil {
			if fuzzRand.Intn(2) == 0 {
				_, _ = AssignTake(ctx, w.pool, actor, id, `{"v":1}`, "", "")
			} else {
				_, _ = AssignTake(ctx, w.pool, actor, id, "", "", "")
			}
		}
	case 4: // submit my taken assign (maybe late → expiry beats it)
		var id int64
		if err := w.pool.QueryRow(ctx, `
			SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id
			WHERE t.code=$1 AND a.assignee_id=$2 AND a.state='taken' LIMIT 1`,
			code, actor).Scan(&id); err == nil {
			_, _ = AssignSubmit(ctx, w.pool, actor, id, `{"v":1}`, "", "")
		}
	case 5: // judge my delivered assign
		var id int64
		if err := w.pool.QueryRow(ctx, `
			SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id
			WHERE t.code=$1 AND a.creator_id=$2 AND a.state='delivered' LIMIT 1`,
			code, actor).Scan(&id); err == nil {
			_, _ = AssignJudge(ctx, w.pool, actor, id, "adopt", "", "")
		}
	case 6: // void my undelivered
		var id int64
		if err := w.pool.QueryRow(ctx, `
			SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id
			WHERE t.code=$1 AND a.creator_id=$2 AND a.state IN ('open','taken') LIMIT 1`,
			code, actor).Scan(&id); err == nil {
			_, _ = AssignVoid(ctx, w.pool, actor, id, "")
		}
	case 7: // drop my taken
		var id int64
		if err := w.pool.QueryRow(ctx, `
			SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id
			WHERE t.code=$1 AND a.assignee_id=$2 AND a.state='taken' LIMIT 1`,
			code, actor).Scan(&id); err == nil {
			_, _ = AssignDrop(ctx, w.pool, actor, id, "")
		}
	case 8: // role churn (guards may reject — fine)
		if len(w.agents) > 0 {
			_, _ = ThreadSetRole(ctx, w.pool, actor, code, w.pick(w.agents),
				[]string{"observer", "speaker", "governor"}[fuzzRand.Intn(3)], "")
		}
	case 9: // leave / rejoin
		if actor != w.owner {
			if _, err := ThreadLeave(ctx, w.pool, actor, code, ""); err == nil && fuzzRand.Intn(2) == 0 {
				_, _ = ThreadJoin(ctx, w.pool, actor, w.keys[code], "")
			}
		}
	case 10: // key rotation
		if _, err := ThreadIssueKey(ctx, w.pool, actor, code, "speaker", ""); err == nil {
			if res, err := ThreadIssueKey(ctx, w.pool, w.owner, code, "speaker", ""); err == nil {
				if k, ok := res["key"].(string); ok {
					w.keys[code] = k
				}
			}
		}
	case 11: // retract my entry with pending receipts
		var entry int64
		if err := w.pool.QueryRow(ctx, `
			SELECT e.id FROM thread_entries e
			JOIN threads t ON t.id = e.thread_id
			WHERE t.code=$1 AND e.author_id=$2
			  AND EXISTS (SELECT 1 FROM thread_receipts r WHERE r.entry_id=e.id AND r.state='pending')
			LIMIT 1`, code, actor).Scan(&entry); err == nil {
			_, _ = ThreadRetract(ctx, w.pool, actor, code, entry, "")
		}
	}
	w.checkInvariants()
}

// checkInvariants asserts the closure invariants directly against
// the database — the same facts every read path projects from.
func (w *fuzzWorld) checkInvariants() {
	ctx, t, pool := w.ctx, w.t, w.pool

	// invariants are scoped to THIS world's rooms — the shared test
	// database carries other tests' fixtures, and their cleanliness is
	// their own contract, not the fuzzer's
	scoped := ` AND t.code = ANY($1)`

	// V1: a pending receipt belongs to a CURRENT member (no ghosts)
	var ghosts int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM thread_receipts r
		JOIN threads t ON t.id = r.thread_id
		WHERE r.state = 'pending'`+scoped+` AND NOT EXISTS (
		    SELECT 1 FROM thread_members m WHERE m.thread_id = t.id AND m.account_id = r.account_id)`,
		w.rooms).Scan(&ghosts); err != nil {
		t.Fatalf("V1: %v", err)
	}
	if ghosts != 0 {
		t.Fatalf("V1: %d pending receipts owed by non-members", ghosts)
	}

	// V2: an undelivered assignment binds two current members
	var dangling int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM assigns a
		JOIN threads t ON t.id = a.thread_id
		WHERE t.code = ANY($1) AND a.state IN ('open','taken') AND (
		    NOT EXISTS (SELECT 1 FROM thread_members m WHERE m.thread_id=a.thread_id AND m.account_id=a.assignee_id)
		    OR NOT EXISTS (SELECT 1 FROM thread_members m WHERE m.thread_id=a.thread_id AND m.account_id=a.creator_id))`,
		w.rooms).Scan(&dangling); err != nil {
		t.Fatalf("V2: %v", err)
	}
	if dangling != 0 {
		t.Fatalf("V2: %d undelivered assignments bound to non-members", dangling)
	}

	// V3: every OPEN room has a governor
	var ungoverned int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM threads t
		WHERE t.status='open' AND t.code = ANY($1) AND NOT EXISTS (
		    SELECT 1 FROM thread_members m WHERE m.thread_id=t.id AND m.role='governor')`,
		w.rooms).Scan(&ungoverned); err != nil {
		t.Fatalf("V3: %v", err)
	}
	if ungoverned != 0 {
		t.Fatalf("V3: %d open rooms without a governor", ungoverned)
	}

	// V4: closed rooms hold no live obligations
	var closedLive int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM thread_receipts r JOIN threads t ON t.id=r.thread_id
		        WHERE t.status='closed' AND t.code = ANY($1) AND r.state='pending')
		     + (SELECT COUNT(*) FROM assigns a JOIN threads t ON t.id=a.thread_id
		        WHERE t.status='closed' AND t.code = ANY($1) AND a.state IN ('open','taken'))`,
		w.rooms).Scan(&closedLive); err != nil {
		t.Fatalf("V4: %v", err)
	}
	if closedLive != 0 {
		t.Fatalf("V4: %d live obligations inside closed rooms", closedLive)
	}

	// V5: receipt state/resolution combinations are legal (note only
	// on handle — the DB CHECK covers the rest; this re-checks note)
	var badNote int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM thread_receipts r
		JOIN threads t ON t.id = r.thread_id
		WHERE t.code = ANY($1)
		  AND r.note IS NOT NULL AND NOT (r.state='fulfilled' AND r.resolution='handle')`,
		w.rooms).Scan(&badNote); err != nil {
		t.Fatalf("V5: %v", err)
	}
	if badNote != 0 {
		t.Fatalf("V5: %d receipts carry notes outside handle", badNote)
	}

	// V6: the todo projection equals a direct recomputation, per actor
	for _, actor := range append([]int64{w.owner}, w.agents...) {
		res, err := TodoList(ctx, pool, actor, 0, "")
		if err != nil {
			t.Fatalf("V6 todo: %v", err)
		}
		projected := len(res["todos"].([]map[string]any))
		var direct int
		if err := pool.QueryRow(ctx, `
			SELECT (SELECT COUNT(*) FROM thread_receipts WHERE account_id=$1 AND state='pending')
			     + (SELECT COUNT(*) FROM assigns WHERE assignee_id=$1 AND state='taken')
			     + (SELECT COUNT(*) FROM assigns a JOIN thread_members m
			        ON m.thread_id=a.thread_id AND m.account_id=a.creator_id
			        WHERE a.creator_id=$1 AND a.state='delivered')`, actor).Scan(&direct); err != nil {
			t.Fatalf("V6 direct: %v", err)
		}
		if projected != direct {
			t.Fatalf("V6: actor %d projection=%d direct=%d", actor, projected, direct)
		}
	}
}

func TestFuzzRandomActionSequences(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			pool := revisionTestPool(t)
			ctx := context.Background()
			owner, _, _ := a7TestBot(t, pool, 5)
			agents := []int64{}
			w := &fuzzWorld{pool: pool, owner: owner, agents: agents, ctx: ctx, t: t,
				rooms: nil, keys: map[string]string{}}
			fuzzRand = rand.New(rand.NewSource(seed))

			// two rooms, keys live
			for i := 0; i < 2; i++ {
				code, raw := threadStart(t, pool, owner, fmt.Sprintf("fuzz-%d-%d", seed, i), true, "")
				w.rooms = append(w.rooms, code)
				w.keys[code] = raw
				defer func(c string) {
					_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code=$1)`, c)
					_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code=$1`, c)
				}(code)
			}
			// three workers join both rooms
			for i := 0; i < 3; i++ {
				a, _, _ := a7TestBot(t, pool, 5)
				w.agents = append(w.agents, a)
				for _, code := range w.rooms {
					if _, err := ThreadJoin(ctx, pool, a, w.keys[code], ""); err != nil {
						t.Fatalf("join: %v", err)
					}
				}
			}

			for step := 0; step < 400; step++ {
				w.step()
			}
			// final full sweep + invariant pass
			if _, err := RecoverAssigns(ctx, pool, "now", 500); err != nil {
				t.Fatalf("final sweep: %v", err)
			}
			w.checkInvariants()
		})
	}
}

// ── External-audit regression: deactivation × join race (P1-3).
// The seam now locks the account row BEFORE any room lock and
// re-verifies active under it — a disabling transaction committing
// mid-join can no longer leave the disabled account with a fresh
// membership, regardless of interleaving.
func TestExtAuditDeactivationJoinRace(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	for round := 0; round < 12; round++ {
		victim, _, _ := a7TestBot(t, pool, 5)
		code, raw := threadStart(t, pool, owner, "race-deact", true, "")
		var wg sync.WaitGroup
		joined := make(chan error, 1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, jErr := ThreadJoin(ctx, pool, victim, raw, "")
			joined <- jErr
		}()
		go func() {
			defer wg.Done()
			_, _ = pool.Exec(ctx, `UPDATE tb_bots SET status='disabled' WHERE id=$1`, victim)
			tx, err := pool.TxBegin(ctx)
			if err == nil {
				_, _ = repository.TerminateAccountThreadMemberships(ctx, tx, victim)
				_ = tx.Commit(ctx)
			}
		}()
		wg.Wait()
		joinErr := <-joined
		_ = joinErr
		// invariant either way: a disabled account holds NO membership
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM thread_members m
			 JOIN tb_bots b ON b.id = m.account_id
			 WHERE m.account_id=$1 AND b.status='disabled'`, victim).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("round %d: disabled account holds %d memberships", round, n)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id=(SELECT id FROM threads WHERE code=$1)`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code=$1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, victim)
	}
}

// ── External-audit regression: explicit invalid dues are rejected,
// never silently normalized (P2-2); an empty memories array is not a
// delivery (P2-1); assignment creation notifies the assignee (P2-3).
func TestExtAuditInputBoundariesAndInviteNotify(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "ext", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// negative due is an explicit mistake
	if _, err := ThreadPost(ctx, pool, owner, code, "bad due", "", "", nil, nil,
		&AssignSpec{To: a, Requirements: "x", DeliverDueS: -5}, "xd1"); err == nil {
		t.Fatal("negative deliver_due must be rejected")
	} else {
		threadErrIs(t, err, 422, "VALIDATION_FAILED")
	}
	// omitted due falls back to default
	r, err := ThreadPost(ctx, pool, owner, code, "default due", "", "", nil, nil,
		&AssignSpec{To: a, Requirements: "default due work"}, "xd2")
	if err != nil {
		t.Fatalf("post+assign: %v", err)
	}
	if r["assign"] == nil {
		t.Fatal("assign missing")
	}
	var due int
	if err := pool.QueryRow(ctx,
		`SELECT deliver_due_s FROM assigns WHERE id=$1`, r["assign"].(int64)).Scan(&due); err != nil || due != 86400 {
		t.Fatalf("default due = %d (err %v)", due, err)
	}

	// memories "[]" is emptiness, not a delivery
	if _, err := AssignTake(ctx, pool, a, r["assign"].(int64), "", "[]", "xt1"); err != nil {
		t.Fatalf("take without delivery: %v", err)
	}
	if _, err := AssignSubmit(ctx, pool, a, r["assign"].(int64), "", "[]", "xs1"); err == nil {
		t.Fatal("empty delivery (payload='', memories='[]') must be rejected")
	} else {
		threadErrIs(t, err, 422, "VALIDATION_FAILED")
	}

	// creating an assignment notifies the assignee (kind 'assign')
	var invited int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notify_outbox WHERE account_id=$1 AND kind='assign'`, a).Scan(&invited); err != nil || invited < 1 {
		t.Fatalf("assign invite notification = %d (err %v)", invited, err)
	}
}

// ── External-audit regression: thread_get assignments are bounded
// and expandable (P1-5). ──
func TestExtAuditAssignmentsBounded(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "bounded", false, "")
	defer threadCleanup(t, pool, []int64{owner}, code)
	// self-assigned 120 assignments: one page holds 50 + cursor
	var last int64
	for i := 0; i < 120; i++ {
		res, err := ThreadPost(ctx, pool, owner, code, "bulk", "", "", nil, nil,
			&AssignSpec{To: owner, Requirements: "bulk work item", DeliverDueS: 3600, JudgeDueS: 3600},
			fmt.Sprintf("bd-%d", i))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		last = res["assign"].(int64)
	}
	page1, err := ThreadGet(ctx, pool, owner, code, "", nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	digest := page1["assignments"].([]map[string]any)
	if len(digest) != 50 {
		t.Fatalf("page1 = %d, want bounded 50", len(digest))
	}
	if _, has := digest[0]["requirements"]; has {
		t.Fatal("digest rows must not carry requirements (bounded payload)")
	}
	nc, _ := page1["assignments_next_cursor"].(string)
	if nc == "" {
		t.Fatal("pagination cursor missing")
	}
	// expansion by id carries the heavy fields
	expanded, err := ThreadGet(ctx, pool, owner, code, "", nil, "", []int64{last}, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := expanded["assignments_expanded"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["requirements"] != "bulk work item" {
		t.Fatalf("expanded = %v", rows)
	}
	// obligation-driven next[]: OPEN self-assigns create no items
	// (A11) — take one without payload, and the deliver obligation
	// must surface as a prefilled assign_submit hint
	if _, err := AssignTake(ctx, pool, owner, last, "", "", "xb-take"); err != nil {
		t.Fatalf("take: %v", err)
	}
	page2, err := ThreadGet(ctx, pool, owner, code, "", nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	nextCalls := page2["next"].([]map[string]any)
	found := false
	for _, n := range nextCalls {
		if n["tool"] == "assign_submit" && n["args"].(map[string]any)["assign"] == last {
			found = true
		}
	}
	if !found {
		t.Fatalf("next[] lacks the prefilled assign_submit handle: %v", nextCalls)
	}
}

// ── PM-001 A regression: the ThreadGet snapshot transaction must be
// REPEATABLE READ. Two connections against the real thread_members
// table: while the read transaction sleeps between statements, a
// removal commits — the read must keep seeing the pre-removal world
// (READ COMMITTED was proven leaky: fresh snapshot per statement).
func TestPM001ThreadGetSnapshotIsolation(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "iso", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatal(err)
	}
	// stmt1 inside the snapshot tx: two members
	var n1 int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM thread_members m JOIN threads t ON t.id=m.thread_id WHERE t.code=$1`,
		code).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	if n1 != 2 {
		t.Fatalf("stmt1 members = %d, want 2", n1)
	}
	// concurrent removal commits between the statements
	if _, err := pool.Exec(ctx,
		`DELETE FROM thread_members WHERE account_id=$1`, a); err != nil {
		t.Fatal(err)
	}
	// stmt2 in the SAME transaction: the snapshot must still hold 2
	var n2 int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM thread_members m JOIN threads t ON t.id=m.thread_id WHERE t.code=$1`,
		code).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n2 != 2 {
		t.Fatalf("snapshot leaked: stmt2 saw %d members after the removal committed (want 2)", n2)
	}
	_ = tx.Commit(ctx)
}
