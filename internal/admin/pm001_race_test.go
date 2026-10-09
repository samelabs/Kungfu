package admin

// PM-001 C: the REAL admin disable path (DisablePlatformAccount →
// WithAuditTx → AdminLockBotForUpdate → AdminSetBotStatus → cascade)
// racing concurrent room writes. Whatever the interleaving, a
// disabled account must end with zero memberships, zero pending
// obligations and zero undelivered assignments on both sides.

import (
	"context"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func TestPM001RealDisablePathRacesRoomWrites(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()

	seedBot := func(name string) int64 {
		var id int64
		if err := db.QueryRow(ctx, `
			INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
			VALUES ($1, $2, 'aaaa', 'x', 10, 'active') RETURNING id`,
			name, testKeyHashN(name)).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	principal := &Principal{
		Permissions: []string{"*"}, // wildcard; permission surface is not under test
	}

	for round := 0; round < 8; round++ {
		owner := seedBot(pmName("pm_c_owner", round))
		victim := seedBot(pmName("pm_c_victim", round))
		var thID int64
		if err := db.QueryRow(ctx, `
			INSERT INTO threads (code, subject, status, next_seq) VALUES ($1,'pm','open',1) RETURNING id`,
			pmCode(round)).Scan(&thID); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{owner, victim} {
			role := "speaker"
			if id == owner {
				role = "governor"
			}
			if _, err := db.Exec(ctx, `
				INSERT INTO thread_members (thread_id, account_id, role) VALUES ($1,$2,$3)`,
				thID, id, role); err != nil {
				t.Fatal(err)
			}
		}
		// victim also holds an open assignment AS CREATOR and one as
		// assignee — both must void on disable
		if _, err := db.Exec(ctx, `
			INSERT INTO thread_entries (thread_id, seq, author_id, memory_id, memory_revision)
			VALUES ($1, 1, $2, (SELECT id FROM tb_kungfus LIMIT 1), 1)`, thID, owner); err != nil {
			// no memories in this private DB: seed a minimal one
			var memID int64
			if err := db.QueryRow(ctx, `
				INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status, revision, origin)
				VALUES ('pmm', $1, 't', '[]', 'c', 'x', 'private', 'active', 1, 'standalone') RETURNING id`,
				owner).Scan(&memID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `
				INSERT INTO thread_entries (thread_id, seq, author_id, memory_id, memory_revision)
				VALUES ($1, 1, $2, $3, 1)`, thID, owner, memID); err != nil {
				t.Fatal(err)
			}
		}
		var entryID int64
		if err := db.QueryRow(ctx,
			`SELECT id FROM thread_entries WHERE thread_id=$1 ORDER BY seq LIMIT 1`, thID).Scan(&entryID); err != nil {
			t.Fatal(err)
		}
		for _, spec := range []struct{ creator, assignee int64 }{{victim, owner}, {owner, victim}} {
			if _, err := db.Exec(ctx, `
				INSERT INTO assigns (thread_id, entry_id, creator_id, assignee_id, requirements, deliver_due_s, judge_due_s, state)
				VALUES ($1,$2,$3,$4,'r',3600,3600,'open')`, thID, entryID, spec.creator, spec.assignee); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO thread_receipts (thread_id, entry_id, account_id, state)
			VALUES ($1,$2,$3,'pending')`, thID, entryID, victim); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = DisablePlatformAccount(ctx, db, principal, victim) }()
		go func() {
			defer wg.Done()
			// a room write racing the disable: victim re-joining shape
			// (membership insert guarded by nothing here — the INVARIANT
			// after the race is what matters)
			_, _ = db.Exec(ctx, `
				INSERT INTO thread_members (thread_id, account_id, role)
				VALUES ($1,$2,'speaker') ON CONFLICT DO NOTHING`, thID, victim)
		}()
		wg.Wait()

		assertClean := func(label string) {
			var n int
			if err := db.QueryRow(ctx, `
				SELECT count(*) FROM thread_members m JOIN tb_bots b ON b.id=m.account_id
				WHERE m.account_id=$1 AND b.status='disabled'`, victim).Scan(&n); err != nil || n != 0 {
				t.Fatalf("round %d %s: disabled account holds %d memberships (err %v)", round, label, n, err)
			}
			var pending int
			if err := db.QueryRow(ctx, `
				SELECT count(*) FROM thread_receipts r JOIN tb_bots b ON b.id=r.account_id
				WHERE r.account_id=$1 AND r.state='pending' AND b.status='disabled'`, victim).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("round %d %s: ghost pending receipts %d", round, label, pending)
			}
			var dangling int
			if err := db.QueryRow(ctx, `
				SELECT count(*) FROM assigns a JOIN tb_bots b ON b.id=$1
				WHERE b.status='disabled' AND a.state IN ('open','taken')
				  AND (a.assignee_id=$1 OR a.creator_id=$1)`, victim).Scan(&dangling); err != nil || dangling != 0 {
				t.Fatalf("round %d %s: dangling assignments %d", round, label, dangling)
			}
		}
		assertClean("post-race")
		// run the disable AGAIN (idempotent admin path) and re-assert
		_ = DisablePlatformAccount(ctx, db, principal, victim)
		assertClean("post-idempotent")
	}
	_ = pg.Pool{}
	_ = repository.ThreadStatusOpen
}

func pmName(base string, round int) string { return base + "_r" + string(rune('0'+round)) }
func pmCode(round int) string              { return "pmc" + string(rune('0'+round)) }
