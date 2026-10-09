package repository

// Thread core (D2) repository tests against a real PostgreSQL
// (KF_TEST_DATABASE_URL, migrations applied): the 024 schema shape
// (single-active-key group, member uniqueness, idempotency identity),
// the key-group statements, the §6.5 close, and the §4 deactivation
// cascade.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"testing"

	"kungfu.md/internal/pg"
)

func threadTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	return migTestPool(t)
}

// threadKeyHash is the production key digest: sha256 hex of the raw
// "kf_"+32-hex key.
func threadKeyHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

type seededThread struct {
	ID      int64
	Code    string
	Creator int64
	KeyRaw  string // active key raw material, when issued
	KeyHash string
	KeyRole string
}

// threadSeed creates an open room with creator as governor, inside
// the given querier (pool or tx), optionally issuing a first key.
func threadSeed(t *testing.T, q pg.Querier, creator int64, keyRole string) *seededThread {
	t.Helper()
	ctx := context.Background()
	code, err := GenerateUniqueThreadCode(ctx, q)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	id, err := InsertThread(ctx, q, code, nil)
	if err != nil {
		t.Fatalf("insert thread: %v", err)
	}
	if err := InsertThreadMember(ctx, q, id, creator, ThreadRoleGovernor, nil); err != nil {
		t.Fatalf("insert creator: %v", err)
	}
	st := &seededThread{ID: id, Code: code, Creator: creator}
	if keyRole != "" {
		st.KeyRaw = "kf_" + hex.EncodeToString([]byte(code)) // shape irrelevant; uniqueness via code
		st.KeyHash = threadKeyHash(st.KeyRaw)
		st.KeyRole = keyRole
		if err := IssueThreadKey(ctx, q, id, st.KeyHash, keyRole); err != nil {
			t.Fatalf("issue key: %v", err)
		}
	}
	return st
}

func threadTestCleanup(t *testing.T, pool *pg.Pool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, _ = pool.Exec(context.Background(), `DELETE FROM thread_members WHERE thread_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM threads WHERE id = $1`, id)
	}
}

// TestThread024SchemaShape: the fresh chain carries the 024 contract —
// threads' three key columns with the group check, the members pair
// key, and the idempotency triple key.
func TestThread024SchemaShape(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()

	for _, tc := range []struct{ table, column, want string }{
		{"threads", "key_hash", "character"},
		{"threads", "key_role", "text"},
		{"threads", "key_issued_at", "timestamp without time zone"},
		{"threads", "next_seq", "bigint"},
	} {
		var dataType string
		if err := pool.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1 AND column_name = $2`, tc.table, tc.column).
			Scan(&dataType); err != nil {
			t.Fatalf("%s.%s: %v", tc.table, tc.column, err)
		}
		if dataType != tc.want {
			t.Fatalf("%s.%s is %s, want %s", tc.table, tc.column, dataType, tc.want)
		}
	}

	// the single-active-key group invariant exists as a check constraint
	var groupChecks int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_constraint
		WHERE conrelid = 'threads'::regclass AND conname = 'ck_threads_key_group'`).
		Scan(&groupChecks); err != nil {
		t.Fatal(err)
	}
	if groupChecks != 1 {
		t.Fatalf("threads key-group check constraints = %d, want 1", groupChecks)
	}

	// thread_members: UNIQUE(thread_id, account_id) is the primary key
	for _, tc := range []struct {
		table string
		want  []string
	}{
		{"thread_members", []string{"account_id", "thread_id"}},
		{"thread_idempotency", []string{"account_id", "key", "tool"}},
	} {
		rows, err := pool.Query(ctx, `
			SELECT a.attname FROM pg_index i
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
			WHERE i.indrelid = $1::regclass AND i.indisprimary`, tc.table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		sort.Strings(cols)
		if len(cols) != len(tc.want) {
			t.Fatalf("%s primary key = %v, want %v", tc.table, cols, tc.want)
		}
		for i := range cols {
			if cols[i] != tc.want[i] {
				t.Fatalf("%s primary key = %v, want %v", tc.table, cols, tc.want)
			}
		}
	}
}

// TestThreadKeyGroupConstraint: a partial key group (hash set, role
// NULL) is rejected by the database itself — the three columns are
// one group, not three facts.
func TestThreadKeyGroupConstraint(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()
	bot := taskV1SeedBot(t, pool, 0)
	st := threadSeed(t, pool, bot, "")
	defer threadTestCleanup(t, pool, st.ID)

	if _, err := pool.Exec(ctx, `
		UPDATE threads SET key_hash = $1 WHERE id = $2`, threadKeyHash("kf_partial"), st.ID); err == nil {
		t.Fatal("partial key group (hash without role) unexpectedly accepted")
	}
}

// TestThreadIssueAndRevokeKeyReplaceGroup: issuing a key sets the
// whole group and overwrites any previous key in one statement;
// revocation NULLs all three.
func TestThreadIssueAndRevokeKeyReplaceGroup(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()
	bot := taskV1SeedBot(t, pool, 0)
	st := threadSeed(t, pool, bot, "")
	defer threadTestCleanup(t, pool, st.ID)

	first := threadKeyHash("kf_" + "00000000000000000000000000000001")
	if err := IssueThreadKey(ctx, pool, st.ID, first, ThreadRoleSpeaker); err != nil {
		t.Fatalf("issue: %v", err)
	}
	th, err := FindThreadByCode(ctx, pool, st.Code)
	if err != nil || th == nil {
		t.Fatalf("reload: %v %v", th, err)
	}
	if th.KeyHash == nil || *th.KeyHash != first || th.KeyRole == nil || *th.KeyRole != ThreadRoleSpeaker || th.KeyIssuedAt == nil {
		t.Fatalf("key group after issue = %+v", th)
	}

	// a second key replaces the first — same statement, no window with two keys
	second := threadKeyHash("kf_" + "00000000000000000000000000000002")
	if err := IssueThreadKey(ctx, pool, st.ID, second, ThreadRoleObserver); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	th, _ = FindThreadByCode(ctx, pool, st.Code)
	if th.KeyHash == nil || *th.KeyHash != second {
		t.Fatalf("reissue left the old key: %+v", th)
	}

	if err := RevokeThreadKey(ctx, pool, st.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	th, _ = FindThreadByCode(ctx, pool, st.Code)
	if th.KeyHash != nil || th.KeyRole != nil || th.KeyIssuedAt != nil {
		t.Fatalf("revoke did not clear the group: %+v", th)
	}
}

// TestThreadCloseClearsKeyKeepsMembers: §6.5 close — terminal status,
// key group cleared, membership rows kept (read-only room).
func TestThreadCloseClearsKeyKeepsMembers(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()
	bot := taskV1SeedBot(t, pool, 0)
	guest := taskV1SeedBot(t, pool, 0)
	st := threadSeed(t, pool, bot, ThreadRoleSpeaker)
	defer threadTestCleanup(t, pool, st.ID)
	if err := InsertThreadMember(ctx, pool, st.ID, guest, ThreadRoleObserver, &st.KeyHash); err != nil {
		t.Fatalf("guest: %v", err)
	}

	ok, err := CloseThreadByID(ctx, pool, st.ID)
	if err != nil || !ok {
		t.Fatalf("close: %v %v", ok, err)
	}
	th, _ := FindThreadByCode(ctx, pool, st.Code)
	if th.Status != ThreadStatusClosed || th.ClosedAt == nil ||
		th.KeyHash != nil || th.KeyRole != nil || th.KeyIssuedAt != nil {
		t.Fatalf("closed row = %+v", th)
	}
	if n, _ := CountThreadMembers(ctx, pool, st.ID); n != 2 {
		t.Fatalf("members after close = %d, want 2 (kept, read-only)", n)
	}
	// closing again is not a second effect
	again, err := CloseThreadByID(ctx, pool, st.ID)
	if err != nil || again {
		t.Fatalf("second close = %v %v, want false nil", again, err)
	}
	// a closed room's key is dead: key-hash lookup finds nothing
	if th, _ := FindOpenThreadByKeyHash(ctx, pool, st.KeyHash); th != nil {
		t.Fatalf("closed room still resolves its key: %+v", th)
	}
}

// TestThreadIdempotencyPrimaryKeyRejectsDuplicate: the (account, tool,
// key) identity is enforced by the database — one logical request.
func TestThreadIdempotencyPrimaryKeyRejectsDuplicate(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()
	bot := taskV1SeedBot(t, pool, 0)

	if err := InsertThreadIdempotency(ctx, pool, bot, "thread_start", "k1",
		threadKeyHash("req-a"), `{"thread":"x"}`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	err := InsertThreadIdempotency(ctx, pool, bot, "thread_start", "k1",
		threadKeyHash("req-b"), `{"thread":"y"}`)
	if err == nil {
		t.Fatal("duplicate idempotency identity unexpectedly accepted")
	}
	if !IsUniqueViolation(err) {
		t.Fatalf("duplicate insert: want unique violation, got %v", err)
	}
	// a different tool or key under the same account is a different
	// logical request
	for _, tc := range []struct{ tool, key string }{
		{"thread_join", "k1"}, {"thread_start", "k2"},
	} {
		if err := InsertThreadIdempotency(ctx, pool, bot, tc.tool, tc.key,
			threadKeyHash("req-c"), `{}`); err != nil {
			t.Fatalf("insert %s/%s: %v", tc.tool, tc.key, err)
		}
	}
}

// TestTerminateAccountThreadMembershipsCascade (§4): every membership
// ends; an open room whose only governor was the account closes in
// the same transaction with the §6.5 effects; an open room that keeps
// another governor stays open.
func TestTerminateAccountThreadMembershipsCascade(t *testing.T) {
	pool := threadTestPool(t)
	ctx := context.Background()
	sole := taskV1SeedBot(t, pool, 0)  // sole governor of room A
	cogov := taskV1SeedBot(t, pool, 0) // co-governor in room B
	speaker := taskV1SeedBot(t, pool, 0)

	roomA := threadSeed(t, pool, sole, ThreadRoleSpeaker)
	roomB := threadSeed(t, pool, cogov, ThreadRoleSpeaker)
	defer threadTestCleanup(t, pool, roomA.ID, roomB.ID)
	if err := InsertThreadMember(ctx, pool, roomB.ID, sole, ThreadRoleSpeaker, nil); err != nil {
		t.Fatalf("seed B membership: %v", err)
	}
	if err := InsertThreadMember(ctx, pool, roomB.ID, speaker, ThreadRoleGovernor, nil); err != nil {
		t.Fatalf("seed B governor: %v", err)
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	closed, err := TerminateAccountThreadMemberships(ctx, tx, sole)
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if len(closed) != 1 || closed[0] != roomA.Code {
		t.Fatalf("cascade closed %v, want exactly [%s]", closed, roomA.Code)
	}
	// room A: closed, key cleared, memberships kept read-only
	a, _ := FindThreadByCode(ctx, pool, roomA.Code)
	if a.Status != ThreadStatusClosed || a.KeyHash != nil {
		t.Fatalf("room A after cascade = %+v", a)
	}
	if m, _ := FindThreadMember(ctx, pool, roomA.ID, sole); m != nil {
		t.Fatalf("room A still has the deactivated member: %+v", m)
	}
	// room B: still open (another governor remains), membership gone
	b, _ := FindThreadByCode(ctx, pool, roomB.Code)
	if b.Status != ThreadStatusOpen {
		t.Fatalf("room B closed despite a remaining governor: %+v", b)
	}
	if m, _ := FindThreadMember(ctx, pool, roomB.ID, sole); m != nil {
		t.Fatalf("room B still has the deactivated member: %+v", m)
	}
	if n, _ := CountThreadMembers(ctx, pool, roomB.ID); n != 2 {
		t.Fatalf("room B members = %d, want 2", n)
	}
}
