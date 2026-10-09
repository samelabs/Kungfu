package service

// Thread close-out regression tests: the six defects fixed on top of
// feat/room-face @ 1bd7e278 — judgment forfeiture across rejoin (R-18),
// delivered-memory reads (§9), handle-note isolation (§6.3), reply_to
// identifier consistency, digest paging under expansion, and the
// unambiguous thread_post replay identity.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func todoHasJudge(t *testing.T, pool *pg.Pool, bot, assignID int64) bool {
	t.Helper()
	res, err := TodoList(context.Background(), pool, bot, 0, "")
	if err != nil {
		t.Fatalf("todo_list: %v", err)
	}
	for _, it := range res["todos"].([]map[string]any) {
		if it["kind"] == "judge" && it["assign"] == assignID {
			return true
		}
	}
	return false
}

func openItemsFor(t *testing.T, pool *pg.Pool, bot int64, code string) int64 {
	t.Helper()
	res, err := ThreadList(context.Background(), pool, bot, "", "")
	if err != nil {
		t.Fatalf("thread_list: %v", err)
	}
	for _, it := range res["threads"].([]map[string]any) {
		if it["thread"].(map[string]any)["code"] == code {
			return it["open_items"].(int64)
		}
	}
	return -1
}

// TestJudgeForfeitSurvivesRejoin: a creator whose membership ends loses
// judgment on its delivered assignments for good — leave, removal and
// deactivation alike; rejoining (a new membership) does not revive it,
// todo_list / thread_list stop showing it, and the deadline still
// settles it as undecided.
func TestJudgeForfeitSurvivesRejoin(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "forfeit", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleGovernor, "fr1"); err != nil {
		t.Fatalf("handover: %v", err)
	}

	// --- leave → rejoin
	_, s1 := assignPost(t, pool, owner, code, "deliver then I leave", a, "fp1")
	if _, err := AssignTake(ctx, pool, a, s1, `{"v":1}`, "", "ft1"); err != nil {
		t.Fatalf("take+submit: %v", err)
	}
	if !todoHasJudge(t, pool, owner, s1) || openItemsFor(t, pool, owner, code) < 1 {
		t.Fatal("creator must see the judge item before leaving")
	}
	if _, err := ThreadLeave(ctx, pool, owner, code, "fl1"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := ThreadJoin(ctx, pool, owner, raw, ""); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if _, err := AssignJudge(ctx, pool, owner, s1, "adopt", "", "fj1"); err == nil {
		t.Fatal("rejoined creator judged an assignment forfeited by its earlier departure")
	} else {
		threadErrIs(t, err, 403, "NOT_YOURS")
	}
	if todoHasJudge(t, pool, owner, s1) {
		t.Fatal("todo_list still offers judgment of a forfeited assignment")
	}
	if n := openItemsFor(t, pool, owner, code); n != 0 {
		t.Fatalf("thread_list.open_items = %d after forfeiture, want 0", n)
	}
	// the database CAS refuses on its own, independent of the service guard
	if ok, err := repository.JudgeAssignmentCAS(ctx, pool, s1, owner, "adopt", ""); err != nil || ok {
		t.Fatalf("CAS judged a forfeited assignment: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE assign_deliveries SET judge_due_at = NOW() - INTERVAL '1 second' WHERE assign_id = $1`, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := assignState(t, pool, s1); got != "undecided" {
		t.Fatalf("forfeited delivered after deadline = %s, want undecided", got)
	}

	// --- a creator that stayed keeps judging (no over-reach)
	_, s2 := assignPost(t, pool, owner, code, "I stay", a, "fp2")
	if _, err := AssignTake(ctx, pool, a, s2, `{"v":2}`, "", "ft2"); err != nil {
		t.Fatalf("take2: %v", err)
	}
	if _, err := AssignJudge(ctx, pool, owner, s2, "adopt", "", "fj2"); err != nil {
		t.Fatalf("a continuous member must judge: %v", err)
	}

	// --- removal → rejoin
	_, s3 := assignPost(t, pool, owner, code, "removed after delivery", a, "fp3")
	if _, err := AssignTake(ctx, pool, a, s3, `{"v":3}`, "", "ft3"); err != nil {
		t.Fatalf("take3: %v", err)
	}
	if _, err := ThreadRemoveMember(ctx, pool, a, code, owner, "frm"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := ThreadJoin(ctx, pool, owner, raw, ""); err != nil {
		t.Fatalf("rejoin after removal: %v", err)
	}
	if _, err := AssignJudge(ctx, pool, owner, s3, "adopt", "", "fj3"); err == nil {
		t.Fatal("creator judged after removal and rejoin")
	}

	// --- deactivation cascade → re-enable → rejoin
	_, s4 := assignPost(t, pool, owner, code, "deactivated after delivery", a, "fp4")
	if _, err := AssignTake(ctx, pool, a, s4, `{"v":4}`, "", "ft4"); err != nil {
		t.Fatalf("take4: %v", err)
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TerminateAccountThreadMemberships(ctx, tx, owner); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("cascade: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadJoin(ctx, pool, owner, raw, ""); err != nil {
		t.Fatalf("rejoin after deactivation: %v", err)
	}
	if _, err := AssignJudge(ctx, pool, owner, s4, "adopt", "", "fj4"); err == nil {
		t.Fatal("creator judged after deactivation and rejoin")
	}
	var forfeited bool
	_ = pool.QueryRow(ctx, `SELECT judge_forfeited_at IS NOT NULL FROM assigns WHERE id = $1`, s4).Scan(&forfeited)
	if !forfeited {
		t.Fatal("deactivation did not record the forfeiture")
	}
}

// TestDeliveredMemoryReadable: memory_get(code, assign) serves exactly
// the version the delivery fixed to current room members — private,
// updated and withdrawn memories included — and nothing else.
func TestDeliveredMemoryReadable(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	c, _, _ := a7TestBot(t, pool, 5)
	outsider, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "delivered-mem", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, c, outsider}, code)
	for _, m := range []int64{a, c} {
		if _, err := ThreadJoin(ctx, pool, m, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	mem, err := Push(ctx, pool, a, map[string]interface{}{
		"title": "report", "tags": []interface{}{"r"}, "content": strings.Repeat("v1-", 30),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	other, err := Push(ctx, pool, a, map[string]interface{}{
		"title": "not delivered", "tags": []interface{}{"r"}, "content": strings.Repeat("xx-", 30),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push other: %v", err)
	}
	_, s := assignPost(t, pool, owner, code, "write a report", a, "dm1")
	if _, err := AssignTake(ctx, pool, a, s, "", `[{"name":"report","code":"`+mem.Code+`"}]`, "dt1"); err != nil {
		t.Fatalf("take+submit memory: %v", err)
	}

	// the plain path stays closed for non-authors (unchanged permission)
	if _, err := GetKungfuForBot(ctx, pool, owner, mem.Code); err == nil {
		t.Fatal("plain memory_get must still refuse a private memory to non-authors")
	}
	for _, reader := range []int64{owner, c} {
		got, err := GetDeliveredMemoryForBot(ctx, pool, reader, mem.Code, s)
		if err != nil {
			t.Fatalf("member %d reads delivered memory: %v", reader, err)
		}
		if got["revision"].(int64) != 1 || !strings.Contains(got["content"].(string), "v1-") {
			t.Fatalf("delivered read wrong version: %v", got["revision"])
		}
	}

	// the author updates, then withdraws: the delivery still serves v1
	if _, err := Push(ctx, pool, a, map[string]interface{}{
		"title": "report", "tags": []interface{}{"r"}, "content": strings.Repeat("v2-", 30), "code": mem.Code,
	}, 128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := Delete(ctx, pool, a, mem.Code); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err := GetDeliveredMemoryForBot(ctx, pool, owner, mem.Code, s)
	if err != nil {
		t.Fatalf("read after update+withdraw: %v", err)
	}
	if got["revision"].(int64) != 1 || !strings.Contains(got["content"].(string), "v1-") {
		t.Fatalf("delivery drifted after update: rev=%v", got["revision"])
	}

	// counter-examples
	if _, err := GetDeliveredMemoryForBot(ctx, pool, outsider, mem.Code, s); err == nil {
		t.Fatal("a non-member read a delivered memory")
	} else {
		threadErrIs(t, err, 403, "NOT_MEMBER")
	}
	if _, err := GetDeliveredMemoryForBot(ctx, pool, owner, other.Code, s); err == nil {
		t.Fatal("a memory the delivery never bound was served through it")
	} else {
		threadErrIs(t, err, 404, "NOT_FOUND")
	}
	if _, err := ThreadLeave(ctx, pool, c, code, "dl1"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := GetDeliveredMemoryForBot(ctx, pool, c, mem.Code, s); err == nil {
		t.Fatal("read survived the end of membership")
	}
}

// TestHandleNoteParties: a handle note is readable by the entry author
// and the handler only; other members see the state, not the note.
func TestHandleNoteParties(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	c, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "notes", true, "")
	defer threadCleanup(t, pool, []int64{owner, b, c}, code)
	for _, m := range []int64{b, c} {
		if _, err := ThreadJoin(ctx, pool, m, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	e := statePost(t, pool, owner, code, "b please look", "", nil, []int64{b}, "hn1")
	entry := e["entry"].(int64)
	if _, err := ThreadHandle(ctx, pool, b, code, entry, "private reason", "hn2"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	noteSeenBy := func(viewer int64) (bool, bool) {
		view, err := ThreadGet(ctx, pool, viewer, code, "", nil, "", nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		for _, row := range view["timeline"].([]map[string]any) {
			if row["entry"] != entry {
				continue
			}
			for _, r := range row["receipts"].([]map[string]any) {
				if r["account"] == b {
					_, has := r["note"]
					return true, has
				}
			}
		}
		return false, false
	}
	for viewer, want := range map[int64]bool{owner: true, b: true, c: false} {
		stateSeen, noteSeen := noteSeenBy(viewer)
		if !stateSeen {
			t.Fatalf("viewer %d must see the receipt state", viewer)
		}
		if noteSeen != want {
			t.Fatalf("viewer %d note visible=%v, want %v", viewer, noteSeen, want)
		}
	}
}

// TestReplyToIdentifierConsistent: timeline and expansion both carry
// reply_to as the target's entry id, reply_to_seq as its seq.
func TestReplyToIdentifierConsistent(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "replyid", true, "")
	defer threadCleanup(t, pool, []int64{owner, b}, code)
	if _, err := ThreadJoin(ctx, pool, b, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	first := statePost(t, pool, owner, code, "first", "", nil, nil, "ri1")
	target := first["entry"].(int64)
	reply := statePost(t, pool, b, code, "reply", "", ptrOf(target), nil, "ri2")
	view, err := ThreadGet(ctx, pool, owner, code, "", []int64{reply["entry"].(int64)}, "", nil, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var tl map[string]any
	for _, row := range view["timeline"].([]map[string]any) {
		if row["entry"] == reply["entry"] {
			tl = row
		}
	}
	ex := view["entries"].([]any)[0].(map[string]any)
	if tl["reply_to"] != target || ex["reply_to"] != target {
		t.Fatalf("reply_to mismatch: timeline=%v expanded=%v want entry id %d", tl["reply_to"], ex["reply_to"], target)
	}
	if tl["reply_to_seq"] != ex["reply_to_seq"] || ex["reply_to_seq"] == nil {
		t.Fatalf("reply_to_seq mismatch: timeline=%v expanded=%v", tl["reply_to_seq"], ex["reply_to_seq"])
	}
}

// TestDigestPagingIndependentOfExpansion: the assignment digest pages
// at 50 with a next cursor whether or not assignments are expanded;
// the timeline cursor appears only when another page exists.
func TestDigestPagingIndependentOfExpansion(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "paging", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	// thread_start writes no entry; 50 assignment posts = 50 entries
	var first int64
	for i := 0; i < 50; i++ {
		_, id := assignPost(t, pool, owner, code, "item", a, "pg"+string(rune('A'+i/26))+string(rune('a'+i%26)))
		if i == 0 {
			first = id
		}
	}
	view, err := ThreadGet(ctx, pool, owner, code, "", nil, "", nil, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n := len(view["timeline"].([]map[string]any)); n != 50 {
		t.Fatalf("timeline page = %d, want 50", n)
	}
	if view["next_cursor"] != nil {
		t.Fatalf("exactly 50 entries must not advertise a next page: %v", view["next_cursor"])
	}
	_, last := assignPost(t, pool, owner, code, "item 51", a, "pg-51")
	for _, expand := range [][]int64{nil, {first, last}} {
		view, err = ThreadGet(ctx, pool, owner, code, "", nil, "", expand, false)
		if err != nil {
			t.Fatalf("get (expand=%v): %v", expand, err)
		}
		if n := len(view["assignments"].([]map[string]any)); n != 50 {
			t.Fatalf("assignment digest = %d rows with expand=%v, want 50", n, expand)
		}
		if view["assignments_next_cursor"] == nil {
			t.Fatalf("assignment digest lost its next cursor with expand=%v", expand)
		}
		if view["next_cursor"] == nil {
			t.Fatal("51 entries must advertise a next timeline page")
		}
		if want := len(expand); len(view["assignments_expanded"].([]any)) != want {
			t.Fatalf("expanded %d, want %d", len(view["assignments_expanded"].([]any)), want)
		}
	}
}

// TestPostReplayIdentityUnambiguous: free text cannot shift between
// fields of the replay identity; ask absent vs [] stays distinct; a
// receipt stored under the legacy encoding still replays.
func TestPostReplayIdentityUnambiguous(t *testing.T) {
	// the legacy delimiter encoding collides; the typed identity does not
	curA, legA := threadPostRequestHashes("T", "a\x1f", "", "b", nil, nil, nil)
	curB, legB := threadPostRequestHashes("T", "a", "", "\x1fb", nil, nil, nil)
	if legA != legB {
		t.Fatal("test premise: the legacy encoding should collide here")
	}
	if curA == curB {
		t.Fatal("typed identity collides across content/summary")
	}
	r1, _ := threadPostRequestHashes("T", "x", "", "", nil, nil,
		&AssignSpec{To: 1, Requirements: "r,{}", OutputSchema: "", DeliverDueS: 60, JudgeDueS: 60})
	r2, _ := threadPostRequestHashes("T", "x", "", "", nil, nil,
		&AssignSpec{To: 1, Requirements: "r", OutputSchema: "{}", DeliverDueS: 60, JudgeDueS: 60})
	if r1 == r2 {
		t.Fatal("typed identity collides across assign fields")
	}
	absent, _ := threadPostRequestHashes("T", "x", "", "", nil, nil, nil)
	empty, _ := threadPostRequestHashes("T", "x", "", "", nil, []int64{}, nil)
	if absent == empty {
		t.Fatal("ask absent and ask [] must differ")
	}
	o1, _ := threadPostRequestHashes("T", "x", "", "", nil, []int64{3, 1}, nil)
	o2, _ := threadPostRequestHashes("T", "x", "", "", nil, []int64{1, 3}, nil)
	if o1 != o2 {
		t.Fatal("ask order must not change the identity")
	}

	// behavior: conflict on a different request, replay on the same, and
	// a legacy-encoded receipt still replays after the upgrade
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "idem", false, "")
	defer threadCleanup(t, pool, []int64{owner}, code)
	first := statePost(t, pool, owner, code, "a\x1f", "b", nil, nil, "same-key")
	if _, err := ThreadPost(ctx, pool, owner, code, "a", "", "\x1fb", nil, nil, nil, "same-key"); err == nil {
		t.Fatal("a different request under the same key replayed")
	} else {
		threadErrIs(t, err, 409, "IDEMPOTENCY_CONFLICT")
	}
	again := statePost(t, pool, owner, code, "a\x1f", "b", nil, nil, "same-key")
	if fmt.Sprint(again["entry"]) != fmt.Sprint(first["entry"]) {
		t.Fatalf("replay produced a new entry: %v vs %v", again["entry"], first["entry"])
	}
	_, legacy := threadPostRequestHashes(code, "legacy", "", "", nil, nil, nil)
	if _, err := pool.Exec(ctx, `
		INSERT INTO thread_idempotency (account_id, tool, key, request_hash, result_snapshot)
		VALUES ($1, 'thread_post', 'old-key', $2, '{"entry": 42, "legacy": true}')`, owner, legacy); err != nil {
		t.Fatal(err)
	}
	res, err := ThreadPost(ctx, pool, owner, code, "legacy", "", "", nil, nil, nil, "old-key")
	if err != nil {
		t.Fatalf("legacy receipt must replay: %v", err)
	}
	if res["legacy"] != true {
		t.Fatalf("legacy replay returned a fresh result: %v", res)
	}
}
