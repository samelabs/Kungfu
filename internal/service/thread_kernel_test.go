package service

// Thread kernel (D2) service tests against the local PostgreSQL
// (KF_TEST_DATABASE_URL): the §6.1 key lifecycle, L2 duplicate join,
// §6.2 LAST_MANAGER on all three paths, the A16 room authorization
// matrix, §6.5 close semantics, the §4 deactivation cascade, the L4
// caps, the L3 idempotency receipts, and L5 concurrency.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

var threadKeyShape = regexp.MustCompile(`^kf_[0-9a-f]{32}$`)

// threadCleanup removes everything a test created: memberships of the
// given accounts, idempotency receipts, and the rooms by code
// (members first — thread_members references threads and tb_bots).
func threadCleanup(t *testing.T, pool *pg.Pool, bots []int64, codes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range bots {
		_, _ = pool.Exec(ctx, `DELETE FROM thread_idempotency WHERE account_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE account_id = $1`, id)
	}
	for _, code := range codes {
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code = $1)`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code = $1`, code)
	}
}

// threadStart opens a room and returns (code, rawKey) when issued.
func threadStart(t *testing.T, pool *pg.Pool, bot int64, subject string, key bool, idem string) (string, string) {
	t.Helper()
	res, err := ThreadStart(context.Background(), pool, bot, subject, key, idem)
	if err != nil {
		t.Fatalf("thread_start: %v", err)
	}
	code, _ := res["thread"].(string)
	if code == "" {
		t.Fatalf("thread_start result has no thread: %v", res)
	}
	raw, _ := res["key"].(string)
	return code, raw
}

func threadErrIs(t *testing.T, err error, wantHTTP int, wantCode string) {
	t.Helper()
	ae, ok := apperrIs(err)
	if !ok {
		t.Fatalf("want AppError %s, got %v", wantCode, err)
	}
	if ae.Code != wantCode || ae.HTTPCode != wantHTTP {
		t.Fatalf("want %d %s, got %d %s", wantHTTP, wantCode, ae.HTTPCode, ae.Code)
	}
}

func threadMemberCount(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM thread_members m JOIN threads t ON t.id = m.thread_id WHERE t.code = $1`,
		code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestThreadStartIssuesFirstKeyAtomically (§6.1, L2): key=true signs
// a well-formed first key (speaker) in the same transaction; a
// non-member joins with it and lands in the key's role.
func TestThreadStartIssuesFirstKeyAtomically(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	code, raw := threadStart(t, pool, owner, "war room", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest}, code)

	if !threadKeyShape.MatchString(raw) {
		t.Fatalf("first key %q is not kf_+32hex", raw)
	}
	joined, err := ThreadJoin(context.Background(), pool, guest, raw, "")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if joined["role"] != repository.ThreadRoleSpeaker || joined["thread"] != code {
		t.Fatalf("join result = %v", joined)
	}
	if n := threadMemberCount(t, pool, code); n != 2 {
		t.Fatalf("members = %d, want 2", n)
	}
}

// TestThreadKeyLifecycle (A5): a superseded key, a revoked key and a
// closed room's key all join with KEY_INVALID; only the current key
// of an open room works.
func TestThreadKeyLifecycle(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	j1, _, _ := a7TestBot(t, pool, 5)
	j2, _, _ := a7TestBot(t, pool, 5)
	j3, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw1 := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, j1, j2, j3}, code)

	// the first key works
	if _, err := ThreadJoin(ctx, pool, j1, raw1, ""); err != nil {
		t.Fatalf("join with first key: %v", err)
	}

	// signing a new key invalidates the old one in the same action
	issued, err := ThreadIssueKey(ctx, pool, owner, code, "", "")
	if err != nil {
		t.Fatalf("thread_key: %v", err)
	}
	raw2, _ := issued["key"].(string)
	if !threadKeyShape.MatchString(raw2) || raw2 == raw1 {
		t.Fatalf("new key = %q", raw2)
	}
	if issued["previous_key_invalidated"] != true {
		t.Fatalf("thread_key result = %v", issued)
	}
	_, err = ThreadJoin(ctx, pool, j2, raw1, "")
	threadErrIs(t, err, 401, "KEY_INVALID") // superseded key (A5)

	// the new key works; revoking it kills it too
	if _, err := ThreadJoin(ctx, pool, j2, raw2, ""); err != nil {
		t.Fatalf("join with second key: %v", err)
	}
	if _, err := ThreadRevokeKey(ctx, pool, owner, code, ""); err != nil {
		t.Fatalf("thread_key_revoke: %v", err)
	}
	_, err = ThreadJoin(ctx, pool, j3, raw2, "")
	threadErrIs(t, err, 401, "KEY_INVALID") // revoked key (A5)

	// a closed room has no key at all (§6.5)
	reissued, err := ThreadIssueKey(ctx, pool, owner, code, repository.ThreadRoleObserver, "")
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	raw3, _ := reissued["key"].(string)
	if _, err := ThreadClose(ctx, pool, owner, code, ""); err != nil {
		t.Fatalf("thread_close: %v", err)
	}
	_, err = ThreadJoin(ctx, pool, j3, raw3, "")
	threadErrIs(t, err, 401, "KEY_INVALID") // closed room (A5)
}

// TestThreadJoinDuplicateReturnsExisting (A7, L2): joining again with
// a valid key returns the existing membership unchanged — same role,
// same joined_at, no new member row — even when the new key carries a
// different role.
func TestThreadJoinDuplicateReturnsExisting(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest}, code)

	first, err := ThreadJoin(ctx, pool, guest, raw, "")
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	again, err := ThreadJoin(ctx, pool, guest, raw, "")
	if err != nil {
		t.Fatalf("duplicate join: %v", err)
	}
	if again["role"] != first["role"] || again["joined_at"] != first["joined_at"] {
		t.Fatalf("duplicate join = %v, want the existing membership %v", again, first)
	}
	if n := threadMemberCount(t, pool, code); n != 2 {
		t.Fatalf("member rows after duplicate join = %d, want 2", n)
	}

	// a governor-key re-join does not promote an existing speaker
	govKey, err := ThreadIssueKey(ctx, pool, owner, code, repository.ThreadRoleGovernor, "")
	if err != nil {
		t.Fatalf("issue governor key: %v", err)
	}
	still, err := ThreadJoin(ctx, pool, guest, govKey["key"].(string), "")
	if err != nil {
		t.Fatalf("re-join with governor key: %v", err)
	}
	if still["role"] != repository.ThreadRoleSpeaker {
		t.Fatalf("re-join role = %v, want the unchanged speaker role", still["role"])
	}
}

// TestThreadLastManagerThreePaths (A8, §6.2): the last governor of an
// open room cannot leave, be removed, or be downgraded — all three
// rejected with LAST_MANAGER and no effect.
func TestThreadLastManagerThreePaths(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest}, code)
	if _, err := ThreadJoin(ctx, pool, guest, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	_, err := ThreadLeave(ctx, pool, owner, code, "")
	threadErrIs(t, err, 409, "LAST_MANAGER")

	_, err = ThreadRemoveMember(ctx, pool, owner, code, owner, "")
	threadErrIs(t, err, 409, "LAST_MANAGER")

	_, err = ThreadSetRole(ctx, pool, owner, code, owner, repository.ThreadRoleSpeaker, "")
	threadErrIs(t, err, 409, "LAST_MANAGER")

	// no path had any effect: the owner is still the sole governor
	if n := threadMemberCount(t, pool, code); n != 2 {
		t.Fatalf("members = %d, want 2 (no effect)", n)
	}
	view, err := ThreadGet(ctx, pool, owner, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view["role"] != repository.ThreadRoleGovernor {
		t.Fatalf("owner role = %v", view["role"])
	}

	// with a second governor all three paths succeed
	if _, err := ThreadSetRole(ctx, pool, owner, code, guest, repository.ThreadRoleGovernor, ""); err != nil {
		t.Fatalf("promote second governor: %v", err)
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, guest, repository.ThreadRoleSpeaker, ""); err != nil {
		t.Fatalf("demote second governor: %v", err)
	}
	if _, err := ThreadRemoveMember(ctx, pool, owner, code, guest, ""); err != nil {
		t.Fatalf("remove second governor: %v", err)
	}
}

// TestThreadGovernanceAuthorization (A16 room part): only a governor
// signs keys, removes members, changes roles and closes; a speaker,
// an observer and a non-member are all refused without effect; the
// observer still reads (thread_get works, read-only).
func TestThreadGovernanceAuthorization(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	speaker, _, _ := a7TestBot(t, pool, 5)
	observer, _, _ := a7TestBot(t, pool, 5)
	outsider, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, speaker, observer, outsider}, code)
	if _, err := ThreadJoin(ctx, pool, speaker, raw, ""); err != nil {
		t.Fatalf("speaker join: %v", err)
	}
	observerKey, err := ThreadIssueKey(ctx, pool, owner, code, repository.ThreadRoleObserver, "")
	if err != nil {
		t.Fatalf("issue observer key: %v", err)
	}
	if _, err := ThreadJoin(ctx, pool, observer, observerKey["key"].(string), ""); err != nil {
		t.Fatalf("observer join: %v", err)
	}

	for _, who := range []int64{speaker, observer} {
		if _, err := ThreadIssueKey(ctx, pool, who, code, "", ""); err == nil {
			t.Fatal("non-governor thread_key must be rejected")
		} else {
			threadErrIs(t, err, 403, "NOT_GOVERNOR")
		}
		if _, err := ThreadRevokeKey(ctx, pool, who, code, ""); err == nil {
			t.Fatal("non-governor thread_key_revoke must be rejected")
		} else {
			threadErrIs(t, err, 403, "NOT_GOVERNOR")
		}
		if _, err := ThreadRemoveMember(ctx, pool, who, code, observer, ""); err == nil {
			t.Fatal("non-governor thread_remove must be rejected")
		} else {
			threadErrIs(t, err, 403, "NOT_GOVERNOR")
		}
		if _, err := ThreadSetRole(ctx, pool, who, code, observer, repository.ThreadRoleSpeaker, ""); err == nil {
			t.Fatal("non-governor thread_set_role must be rejected")
		} else {
			threadErrIs(t, err, 403, "NOT_GOVERNOR")
		}
		if _, err := ThreadClose(ctx, pool, who, code, ""); err == nil {
			t.Fatal("non-governor thread_close must be rejected")
		} else {
			threadErrIs(t, err, 403, "NOT_GOVERNOR")
		}
	}

	// the observer reads the room (read-only member view)
	obsView, err := ThreadGet(ctx, pool, observer, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("observer thread_get: %v", err)
	}
	obsNext := obsView["next"].([]map[string]any)
	if obsView["role"] != repository.ThreadRoleObserver || len(obsNext) == 0 {
		t.Fatalf("observer view = %v", obsView)
	}
	for _, n := range obsNext { // observer floor: nothing but leaving
		if n["tool"] != "thread_leave" {
			t.Fatalf("observer next = %v", obsNext)
		}
	}

	// a non-member neither reads nor leaves
	_, err = ThreadGet(ctx, pool, outsider, code, "", nil, "", nil)
	threadErrIs(t, err, 403, "NOT_MEMBER")
	_, err = ThreadLeave(ctx, pool, outsider, code, "")
	threadErrIs(t, err, 403, "NOT_MEMBER")
	// unknown room
	_, err = ThreadGet(ctx, pool, owner, "0000000000aa", "", nil, "", nil)
	threadErrIs(t, err, 404, "THREAD_NOT_FOUND")
}

// TestThreadCloseSemantics (§6.5): close is terminal — key cleared,
// memberships kept read-only, members may still leave, no reopen, no
// governance actions.
func TestThreadCloseSemantics(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest}, code)
	if _, err := ThreadJoin(ctx, pool, guest, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, err := ThreadClose(ctx, pool, owner, code, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ThreadClose(ctx, pool, owner, code, ""); err == nil {
		t.Fatal("double close must be rejected")
	} else {
		threadErrIs(t, err, 409, "THREAD_CLOSED")
	}
	if _, err := ThreadIssueKey(ctx, pool, owner, code, "", ""); err == nil {
		t.Fatal("key issue on a closed room must be rejected")
	} else {
		threadErrIs(t, err, 409, "THREAD_CLOSED")
	}
	// members are kept and can still read and leave — even the last governor
	view, err := ThreadGet(ctx, pool, guest, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("member read on closed room: %v", err)
	}
	if view["thread"].(map[string]any)["status"] != repository.ThreadStatusClosed {
		t.Fatalf("closed view = %v", view["thread"])
	}
	next := view["next"].([]map[string]any)
	if len(next) == 0 || next[0]["tool"] != "thread_leave" {
		t.Fatalf("closed-room next = %v", next)
	}
	if _, err := ThreadLeave(ctx, pool, owner, code, ""); err != nil {
		t.Fatalf("governor leave on closed room: %v", err)
	}
	if _, err := ThreadLeave(ctx, pool, guest, code, ""); err != nil {
		t.Fatalf("member leave on closed room: %v", err)
	}
}

// TestThreadDeactivationCascade (A15/§4): deactivating the account
// ends its memberships in the same transaction, closes the open room
// it kept alive as sole governor, keeps the room's other members
// read-only — and a deactivated account cannot act (join, start).
func TestThreadDeactivationCascade(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	newcomer, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest, newcomer}, code)
	if _, err := ThreadJoin(ctx, pool, guest, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if err := repository.AdminSetBotStatus(ctx, tx, owner, repository.AdminAccountStatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// the room closed in the same fact; the guest membership survived
	view, err := ThreadGet(ctx, pool, guest, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("guest read after cascade: %v", err)
	}
	if view["thread"].(map[string]any)["status"] != repository.ThreadStatusClosed {
		t.Fatalf("room not closed by the cascade: %v", view["thread"])
	}
	if n := threadMemberCount(t, pool, code); n != 1 {
		t.Fatalf("members after cascade = %d, want 1 (guest kept)", n)
	}

	// the deactivated account can no longer act (§4)
	_, err = ThreadStart(ctx, pool, owner, "", false, "")
	threadErrIs(t, err, 401, "UNAUTHORIZED")
	_, err = ThreadJoin(ctx, pool, owner, raw, "")
	threadErrIs(t, err, 401, "UNAUTHORIZED")
	// an active account cannot join with the dead key either
	_, err = ThreadJoin(ctx, pool, newcomer, raw, "")
	threadErrIs(t, err, 401, "KEY_INVALID")
}

// TestThreadMemberAndRoomLimits (A24 part, L4): the 51st member is
// MEMBER_LIMIT; the 101st open room — created or joined — is
// ROOM_LIMIT.
func TestThreadMemberAndRoomLimits(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	late, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, late}, code)

	// creator + 49 seeded members = the 50 cap
	for i := 0; i < 49; i++ {
		bot, _, _ := a7TestBot(t, pool, 0)
		if err := repository.InsertThreadMember(ctx, pool,
			mustThreadID(t, pool, code), bot, repository.ThreadRoleObserver, nil); err != nil {
			t.Fatalf("seed member %d: %v", i, err)
		}
	}
	_, err := ThreadJoin(ctx, pool, late, raw, "")
	threadErrIs(t, err, 409, "MEMBER_LIMIT")

	// a second account reaches its own 100-open-room cap: the 101st
	// creation and a further join are both ROOM_LIMIT
	joiner, _, _ := a7TestBot(t, pool, 5)
	defer threadCleanup(t, pool, []int64{joiner})
	codes := []string{}
	defer func() { threadCleanup(t, pool, nil, codes...) }()
	for i := 0; i < 100; i++ {
		c, err := repository.GenerateUniqueThreadCode(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		id, err := repository.InsertThread(ctx, pool, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.InsertThreadMember(ctx, pool, id, joiner, repository.ThreadRoleSpeaker, nil); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	_, err = ThreadStart(ctx, pool, joiner, "", false, "")
	threadErrIs(t, err, 409, "ROOM_LIMIT")
	other, raw2 := threadStart(t, pool, owner, "", true, "")
	codes = append(codes, other)
	_, err = ThreadJoin(ctx, pool, joiner, raw2, "")
	threadErrIs(t, err, 409, "ROOM_LIMIT")
}

func mustThreadID(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM threads WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestThreadIdempotencyReceipts (L3, A5/A6 part): same key + same
// request replays the first success's stored facts — original
// subject, no raw-key re-disclosure; same key + different request is
// IDEMPOTENCY_CONFLICT with zero side effects; join and close keep
// receipts too; a failure leaves no receipt.
func TestThreadIdempotencyReceipts(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	guest, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	first, err := ThreadStart(ctx, pool, owner, "original subject", true, "idem-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code := first["thread"].(string)
	rawKey := first["key"].(string)
	fingerprint, _ := first["key_fingerprint"].(string)
	defer threadCleanup(t, pool, []int64{owner, guest}, code)

	// the subject later changes on the row (no update tool yet — the
	// A6 shape with a direct write); the replay still returns the
	// FIRST facts
	if _, err := pool.Exec(ctx,
		`UPDATE threads SET subject = 'changed' WHERE code = $1`, code); err != nil {
		t.Fatal(err)
	}
	replay, err := ThreadStart(ctx, pool, owner, "original subject", true, "idem-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay["subject"] != "original subject" {
		t.Fatalf("replay subject = %v, want the first snapshot's", replay["subject"])
	}
	if replay["key"] != "" {
		t.Fatalf("replay re-disclosed the raw key: %v", replay["key"])
	}
	if replay["key_fingerprint"] != fingerprint {
		t.Fatalf("replay fingerprint = %v, want %v", replay["key_fingerprint"], fingerprint)
	}

	// same key, different request: conflict, no side effects
	var before int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM threads`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = ThreadStart(ctx, pool, owner, "another subject", false, "idem-1")
	threadErrIs(t, err, 409, "IDEMPOTENCY_CONFLICT")
	var after int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM threads`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("threads %d → %d despite IDEMPOTENCY_CONFLICT", before, after)
	}

	// join and close carry receipts of their own
	join1, err := ThreadJoin(ctx, pool, guest, rawKey, "idem-j")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	join2, err := ThreadJoin(ctx, pool, guest, rawKey, "idem-j")
	if err != nil {
		t.Fatalf("join replay: %v", err)
	}
	if join1["joined_at"] != join2["joined_at"] || join1["role"] != join2["role"] {
		t.Fatalf("join replay = %v, want the first %v", join2, join1)
	}

	// a failed action stores no receipt: the same key retries cleanly
	// (the room is still open here; the L2 duplicate join returns the
	// existing membership)
	_, err = ThreadJoin(ctx, pool, guest, "kf_"+strings.Repeat("0", 32), "idem-bad")
	threadErrIs(t, err, 401, "KEY_INVALID")
	ok, err := ThreadJoin(ctx, pool, guest, rawKey, "idem-bad")
	if err != nil {
		t.Fatalf("retry after failure with the same key: %v", err)
	}
	if ok["role"] != join1["role"] {
		t.Fatalf("retry after failure = %v", ok)
	}

	// key issuance carries a receipt too: the raw key appears once,
	// the replay keeps the fingerprint only
	key1, err := ThreadIssueKey(ctx, pool, owner, code, "", "idem-k")
	if err != nil {
		t.Fatalf("thread_key: %v", err)
	}
	if raw := key1["key"].(string); !threadKeyShape.MatchString(raw) || raw == rawKey {
		t.Fatalf("issued key = %q", raw)
	}
	key2, err := ThreadIssueKey(ctx, pool, owner, code, "", "idem-k")
	if err != nil {
		t.Fatalf("thread_key replay: %v", err)
	}
	if key2["key"] != "" {
		t.Fatalf("key replay re-disclosed the raw key: %v", key2["key"])
	}
	if key2["key_fingerprint"] != key1["key_fingerprint"] {
		t.Fatalf("key replay fingerprint = %v, want %v", key2["key_fingerprint"], key1["key_fingerprint"])
	}

	close1, err := ThreadClose(ctx, pool, owner, code, "idem-c")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	close2, err := ThreadClose(ctx, pool, owner, code, "idem-c")
	if err != nil {
		t.Fatalf("close replay: %v", err)
	}
	if fmt.Sprint(close1["status"]) != fmt.Sprint(close2["status"]) {
		t.Fatalf("close replay = %v, want the first %v", close2, close1)
	}
}

// TestThreadConcurrencyJoinAndClose (L5): concurrent joins of two
// accounts produce exactly one membership row each; a close racing a
// join serializes into one of the two legal orders.
func TestThreadConcurrencyJoinAndClose(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	c, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b, c}, code)

	// two accounts join at the same moment: both land, exactly one
	// row each
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, who := range []int64{a, b} {
		wg.Add(1)
		go func(who int64) {
			defer wg.Done()
			<-start
			_, err := ThreadJoin(ctx, pool, who, raw, "")
			errs <- err
		}(who)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent join: %v", err)
		}
	}
	if n := threadMemberCount(t, pool, code); n != 3 {
		t.Fatalf("members after concurrent joins = %d, want 3 (one row each)", n)
	}

	// close racing a join: either the join commits first (member kept,
	// room then closed) or the close wins (join KEY_INVALID) — no
	// third state, no partial effect
	start2 := make(chan struct{})
	joinErr := make(chan error, 1)
	closeErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start2
		_, err := ThreadJoin(ctx, pool, c, raw, "")
		joinErr <- err
	}()
	go func() {
		defer wg.Done()
		<-start2
		_, err := ThreadClose(ctx, pool, owner, code, "")
		closeErr <- err
	}()
	close(start2)
	wg.Wait()
	if err := <-closeErr; err != nil {
		t.Fatalf("concurrent close failed: %v", err)
	}
	jErr := <-joinErr
	view, err := ThreadGet(ctx, pool, owner, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view["thread"].(map[string]any)["status"] != repository.ThreadStatusClosed {
		t.Fatalf("room not closed after the race: %v", view["thread"])
	}
	if jErr == nil {
		// join committed first: the member row exists
		if n := threadMemberCount(t, pool, code); n != 4 {
			t.Fatalf("winning join left %d members, want 4", n)
		}
	} else {
		threadErrIs(t, jErr, 401, "KEY_INVALID") // close won: the key died
		if n := threadMemberCount(t, pool, code); n != 3 {
			t.Fatalf("losing join left a member row: %d", n)
		}
	}
}

// TestThreadIdempotencyConcurrentSameKey (L3+L5): two genuinely
// concurrent calls with the SAME idempotency key and request produce
// exactly one thread; both calls answer the winner's receipt.
func TestThreadIdempotencyConcurrentSameKey(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	start := make(chan struct{})
	out := make(chan map[string]any, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := ThreadStart(ctx, pool, owner, "race subject", false, "idem-race")
			errs <- err
			out <- res
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(out)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent same-key start: %v", err)
		}
	}
	results := <-out
	other := <-out
	if results["thread"] != other["thread"] {
		t.Fatalf("same-key concurrent starts returned %v and %v", results["thread"], other["thread"])
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM threads WHERE subject = 'race subject'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("threads created by the same-key race = %d, want 1", n)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE subject = 'race subject')`)
	_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE subject = 'race subject'`)
	_, _ = pool.Exec(ctx, `DELETE FROM thread_idempotency WHERE account_id = $1 AND key = 'idem-race'`, owner)
}

// TestThreadListAndGetProjections: thread_get returns the room, the
// member table and the key facts; thread_list pages with the status
// filter and the cursor.
func TestThreadListAndGetProjections(t *testing.T) {
	pool := revisionTestPool(t)
	owner, ownerName, _ := a7TestBot(t, pool, 5)
	guest, guestName, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "subject line", true, "")
	defer threadCleanup(t, pool, []int64{owner, guest}, code)
	if _, err := ThreadJoin(ctx, pool, guest, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	view, err := ThreadGet(ctx, pool, guest, code, "", nil, "", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	room := view["thread"].(map[string]any)
	if room["code"] != code || room["status"] != repository.ThreadStatusOpen || room["subject"] != "subject line" {
		t.Fatalf("room = %v", room)
	}
	if view["role"] != repository.ThreadRoleSpeaker {
		t.Fatalf("role = %v", view["role"])
	}
	members := view["members"].([]map[string]any)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	byName := map[string]map[string]any{}
	for _, m := range members {
		byName[m["account"].(string)] = m
	}
	if byName[ownerName]["role"] != repository.ThreadRoleGovernor {
		t.Fatalf("owner row = %v", byName[ownerName])
	}
	if byName[guestName]["role"] != repository.ThreadRoleSpeaker {
		t.Fatalf("guest row = %v", byName[guestName])
	}
	keyFacts := view["key"].(map[string]any)
	if keyFacts["active"] != true || keyFacts["role"] != repository.ThreadRoleSpeaker {
		t.Fatalf("key facts = %v", keyFacts)
	}
	// empty room: timeline and todos are live typed slices now (D3)
	if tl := view["timeline"].([]map[string]any); len(tl) != 0 {
		t.Fatalf("timeline = %v", tl)
	}
	if td := view["todos"].([]map[string]any); len(td) != 0 {
		t.Fatalf("todos = %v", td)
	}

	// list: newest first, status filter, cursor pagination. 52 open
	// rooms forces a real multi-page keyset walk (audit H: the cursor
	// must actually be consumed, not re-read page one).
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE subject = 'page-walk')`)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE subject = 'page-walk'`)
	}()
	for i := 0; i < 51; i++ {
		threadStart(t, pool, guest, "page-walk", false, "")
	}
	list, err := ThreadList(ctx, pool, guest, repository.ThreadStatusOpen, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := len(list["threads"].([]map[string]any)); n != 50 {
		t.Fatalf("page 1 = %d rooms, want a full 50-row page", n)
	}
	seen := map[string]bool{}
	var collect = func(page map[string]any) int {
		items := page["threads"].([]map[string]any)
		for _, it := range items {
			c := it["thread"].(map[string]any)["code"].(string)
			if seen[c] {
				t.Fatalf("room %s repeated across pages", c)
			}
			seen[c] = true
		}
		return len(items)
	}
	total := collect(list)
	nc, _ := list["next_cursor"].(string)
	for nc != "" {
		page, err := ThreadList(ctx, pool, guest, repository.ThreadStatusOpen, nc)
		if err != nil {
			t.Fatalf("list cursor walk: %v", err)
		}
		total += collect(page)
		nc, _ = page["next_cursor"].(string)
	}
	if total != 52 {
		t.Fatalf("cursor walk covered %d rooms, want 52 (1 joined + 51 created)", total)
	}
	if !seen[code] {
		t.Fatal("joined room missing from thread_list walk")
	}
	closed, err := ThreadList(ctx, pool, guest, repository.ThreadStatusClosed, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(closed["threads"].([]map[string]any)); n != 0 {
		t.Fatalf("closed rooms = %d, want 0", n)
	}
	if _, err := ThreadList(ctx, pool, guest, "bogus", ""); err == nil {
		t.Fatal("invalid status must be rejected")
	} else {
		threadErrIs(t, err, 422, "VALIDATION_FAILED")
	}
	if _, err := ThreadList(ctx, pool, guest, "", "not-a-cursor"); err == nil {
		t.Fatal("invalid cursor must be rejected")
	} else {
		threadErrIs(t, err, 422, "VALIDATION_FAILED")
	}
}
