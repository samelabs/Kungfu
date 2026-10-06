package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func threadTestRoleName(t *testing.T, pool *pg.Pool, id int64) string {
	t.Helper()
	b, err := repository.FindActiveBotSummaryByID(context.Background(), pool, id)
	if err != nil || b == nil {
		t.Fatalf("role %d: %v", id, err)
	}
	return b.BotName
}

func threadTestActiveLink(t *testing.T, pool *pg.Pool, requesterID, targetID int64) {
	t.Helper()
	requesterName := threadTestRoleName(t, pool, requesterID)
	targetName := threadTestRoleName(t, pool, targetID)
	if _, _, err := requestRoleLink(context.Background(), pool, requesterID, targetName); err != nil {
		t.Fatalf("request active Link: %v", err)
	}
	if _, err := acceptRoleLink(context.Background(), pool, targetID, requesterName); err != nil {
		t.Fatalf("accept active Link: %v", err)
	}
}

func TestRoleLookupAndLinkLifecycle(t *testing.T) {
	pool := pubTestPool(t)
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	aName, bName := threadTestRoleName(t, pool, a), threadTestRoleName(t, pool, b)

	resolved, err := resolveRoleExact(ctx, pool, bName)
	if err != nil || resolved == nil || resolved.ID != b || resolved.BotName != bName {
		t.Fatalf("exact Role lookup = %+v err=%v", resolved, err)
	}
	if _, err := resolveRoleExact(ctx, pool, bName+"x"); err == nil {
		t.Fatal("nonexistent exact Role unexpectedly resolved")
	}

	link, target, err := requestRoleLink(ctx, pool, a, bName)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if target.ID != b || link.Status != model.RoleLinkPending || link.RequestedByRoleID != a {
		t.Fatalf("pending link = %+v target=%+v", link, target)
	}
	if _, err := acceptRoleLink(ctx, pool, a, bName); err == nil {
		t.Fatal("requester accepted its own pending Link")
	}
	link, err = acceptRoleLink(ctx, pool, b, aName)
	if err != nil || link.Status != model.RoleLinkActive || link.AcceptedAt == nil {
		t.Fatalf("accept = %+v err=%v", link, err)
	}
	active, err := rolesHaveActiveLink(ctx, pool, a, b)
	if err != nil || !active {
		t.Fatalf("active Link = %v err=%v", active, err)
	}
	if err := removeRoleLink(ctx, pool, a, bName); err != nil {
		t.Fatalf("remove active: %v", err)
	}
	if active, _ := rolesHaveActiveLink(ctx, pool, a, b); active {
		t.Fatal("removed Link remained active")
	}

	if _, _, err := requestRoleLink(ctx, pool, b, aName); err != nil {
		t.Fatalf("request for cancel: %v", err)
	}
	if err := cancelRoleLink(ctx, pool, b, aName); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := requestRoleLink(ctx, pool, a, bName); err != nil {
		t.Fatalf("request for decline: %v", err)
	}
	if err := declineRoleLink(ctx, pool, b, aName); err != nil {
		t.Fatalf("decline: %v", err)
	}
}

func TestConcurrentCrossLinkRequestsConvergeOnCanonicalPair(t *testing.T) {
	pool := pubTestPool(t)
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	aName, bName := threadTestRoleName(t, pool, a), threadTestRoleName(t, pool, b)
	ctx := context.Background()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, err := requestRoleLink(ctx, pool, a, bName)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, _, err := requestRoleLink(ctx, pool, b, aName)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent request: %v", err)
		}
	}

	low, high := canonicalRolePair(a, b)
	var count int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM role_links
		WHERE role_low_id=$1 AND role_high_id=$2`, low, high).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("canonical pair rows = %d, want 1", count)
	}
	link, err := repository.FindRoleLink(ctx, pool, low, high)
	if err != nil || link == nil || link.Status != model.RoleLinkPending {
		t.Fatalf("canonical Link = %+v err=%v", link, err)
	}
	if link.RequestedByRoleID != a && link.RequestedByRoleID != b {
		t.Fatalf("unexpected requester %d", link.RequestedByRoleID)
	}
}

func TestThreadKernelLineageGovernanceAndChildIndependence(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	rootCreator := pubSeedBot(t, pool, 0)
	worker := pubSeedBot(t, pool, 0)
	observer := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, rootCreator, worker)
	threadTestActiveLink(t, pool, rootCreator, observer)

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root, rootEntry, err := createRootThreadKernel(ctx, tx, rootCreator, "Root subject", "root input")
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("create root: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if root.ParentThreadID != nil || root.AnchorEntryID != nil || rootEntry.Seq != 1 || rootEntry.MemoryRevision != 1 {
		t.Fatalf("root shape thread=%+v entry=%+v", root, rootEntry)
	}

	tx, _ = pool.TxBegin(ctx)
	added, err := addThreadRoleKernel(ctx, tx, rootCreator, root.ID, worker, model.ThreadWrite, rootEntry.ID)
	if err != nil || !added {
		_ = pg.Rollback(tx)
		t.Fatalf("add worker: added=%v err=%v", added, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, _ = pool.TxBegin(ctx)
	firstChild, err := createChildThreadKernel(ctx, tx, worker, root.ID, rootEntry.ID, "Child 1")
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("child 1: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if firstChild.ParentThreadID == nil || *firstChild.ParentThreadID != root.ID ||
		firstChild.AnchorEntryID == nil || *firstChild.AnchorEntryID != rootEntry.ID {
		t.Fatalf("child anchor = %+v", firstChild)
	}
	anchor, err := repository.FindThreadMemoryByID(ctx, pool, *firstChild.AnchorEntryID)
	if err != nil || anchor == nil || anchor.ThreadID != root.ID || anchor.MemoryRevision != 1 {
		t.Fatalf("resolved child anchor = %+v err=%v", anchor, err)
	}

	canGovern, err := repository.RootCreatorCanGovernThread(ctx, pool, firstChild.ID, rootCreator)
	if err != nil || !canGovern {
		t.Fatalf("root inherited govern = %v err=%v", canGovern, err)
	}
	if role, err := repository.FindThreadRole(ctx, pool, firstChild.ID, rootCreator); err != nil || role != nil {
		t.Fatalf("root creator leaked into child participation: %+v err=%v", role, err)
	}

	// Inherited govern can add an observer using the Child's direct anchor
	// without creating a participant row for the governor.
	tx, _ = pool.TxBegin(ctx)
	added, err = addThreadRoleKernel(ctx, tx, rootCreator, firstChild.ID, observer, model.ThreadRead, rootEntry.ID)
	if err != nil || !added {
		_ = pg.Rollback(tx)
		t.Fatalf("govern add observer: added=%v err=%v", added, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if role, _ := repository.FindThreadRole(ctx, pool, firstChild.ID, observer); role == nil || role.Permission != model.ThreadRead {
		t.Fatalf("observer role = %+v", role)
	}

	current := firstChild
	for depth := 2; depth <= 20; depth++ {
		tx, _ = pool.TxBegin(ctx)
		entry, err := appendThreadMemoryKernel(ctx, tx, current.ID, worker,
			fmt.Sprintf("child depth %d handoff", depth), nil)
		if err != nil {
			_ = pg.Rollback(tx)
			t.Fatalf("append depth %d: %v", depth, err)
		}
		next, err := createChildThreadKernel(ctx, tx, worker, current.ID, entry.ID,
			fmt.Sprintf("Child %d", depth))
		if err != nil {
			_ = pg.Rollback(tx)
			t.Fatalf("create depth %d: %v", depth, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		current = next
	}

	lineage, err := repository.ThreadLineage(ctx, pool, current.ID)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if len(lineage) != 21 || lineage[0].ID != root.ID || lineage[len(lineage)-1].ID != current.ID {
		t.Fatalf("lineage len/root/current = %d/%d/%d, want 21/%d/%d",
			len(lineage), lineage[0].ID, lineage[len(lineage)-1].ID, root.ID, current.ID)
	}
	if can, err := repository.RootCreatorCanGovernThread(ctx, pool, current.ID, rootCreator); err != nil || !can {
		t.Fatalf("deep inherited govern = %v err=%v", can, err)
	}
	if role, _ := repository.FindThreadRole(ctx, pool, current.ID, rootCreator); role != nil {
		t.Fatalf("deep governance synthesized participation: %+v", role)
	}
	var rootTodo int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_receipts WHERE role_id=$1`, rootCreator).Scan(&rootTodo); err != nil {
		t.Fatal(err)
	}
	if rootTodo != 0 {
		t.Fatalf("T2 governance synthesized %d receipts/Todos", rootTodo)
	}

	children, err := repository.ListDirectChildren(ctx, pool, root.ID)
	if err != nil || len(children) != 1 || children[0].ID != firstChild.ID {
		t.Fatalf("root direct children = %+v err=%v", children, err)
	}

	// Structural independence: parent close/removal does not mutate existing Child.
	if _, err := pool.Exec(ctx, `UPDATE threads SET status='closed' WHERE id=$1`, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM thread_roles WHERE thread_id=$1 AND role_id=$2`, root.ID, worker); err != nil {
		t.Fatal(err)
	}
	childAfter, err := repository.FindThreadByID(ctx, pool, firstChild.ID)
	if err != nil || childAfter == nil || childAfter.Status != model.ThreadOpen {
		t.Fatalf("child changed with parent close: %+v err=%v", childAfter, err)
	}
	if role, err := repository.FindThreadRole(ctx, pool, firstChild.ID, worker); err != nil || role == nil || role.Permission != model.ThreadManage {
		t.Fatalf("child participation changed with parent removal: %+v err=%v", role, err)
	}
}

func TestThreadJoinKeyHashResetAndStructuralJoin(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	creator := pubSeedBot(t, pool, 0)
	joiner := pubSeedBot(t, pool, 0)

	tx, _ := pool.TxBegin(ctx)
	root, entry, err := createRootThreadKernel(ctx, tx, creator, "Joinable", "root entry")
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatal(err)
	}
	raw, err := resetThreadJoinKeyKernel(ctx, tx, creator, root.ID, entry.ID)
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("reset join key: %v", err)
	}
	if raw == "" {
		_ = pg.Rollback(tx)
		t.Fatal("raw join key is empty")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	var storedEntry int64
	if err := pool.QueryRow(ctx,
		`SELECT join_key_hash, join_entry_id FROM threads WHERE id=$1`, root.ID).Scan(&stored, &storedEntry); err != nil {
		t.Fatal(err)
	}
	if string(stored) == raw || storedEntry != entry.ID {
		t.Fatalf("join key persistence leaked raw key or wrong entry")
	}

	tx, _ = pool.TxBegin(ctx)
	joinedThread, role, inserted, err := joinThreadByKeyKernel(ctx, tx, joiner, raw)
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("join: %v", err)
	}
	if !inserted || joinedThread.ID != root.ID || role.Permission != model.ThreadWrite || role.EntryID != entry.ID {
		_ = pg.Rollback(tx)
		t.Fatalf("joined = thread %+v role %+v inserted=%v", joinedThread, role, inserted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var receipts int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_receipts WHERE thread_id=$1 AND role_id=$2`, root.ID, joiner).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("T2 structural join created %d receipts; T3 owns receipt creation", receipts)
	}

	// Reuse preserves an existing permission instead of upgrading it.
	if _, err := pool.Exec(ctx,
		`UPDATE thread_roles SET permission='read' WHERE thread_id=$1 AND role_id=$2`, root.ID, joiner); err != nil {
		t.Fatal(err)
	}
	tx, _ = pool.TxBegin(ctx)
	_, role, inserted, err = joinThreadByKeyKernel(ctx, tx, joiner, raw)
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatalf("repeat join: %v", err)
	}
	if inserted || role.Permission != model.ThreadRead {
		_ = pg.Rollback(tx)
		t.Fatalf("repeat join changed membership: role=%+v inserted=%v", role, inserted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, _ = pool.TxBegin(ctx)
	raw2, err := resetThreadJoinKeyKernel(ctx, tx, creator, root.ID, entry.ID)
	if err != nil || raw2 == raw {
		_ = pg.Rollback(tx)
		t.Fatalf("reset did not rotate key: equal=%v err=%v", raw2 == raw, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = pool.TxBegin(ctx)
	if _, _, _, err := joinThreadByKeyKernel(ctx, tx, pubSeedBot(t, pool, 0), raw); err == nil {
		_ = pg.Rollback(tx)
		t.Fatal("old join key remained valid after reset")
	}
	_ = pg.Rollback(tx)

	tx, _ = pool.TxBegin(ctx)
	revoked, err := revokeThreadJoinKeyKernel(ctx, tx, creator, root.ID)
	if err != nil || !revoked {
		_ = pg.Rollback(tx)
		t.Fatalf("revoke: revoked=%v err=%v", revoked, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDirectParticipantAddRequiresActiveLinkAndBumpsRevision(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	creator := pubSeedBot(t, pool, 0)
	target := pubSeedBot(t, pool, 0)

	tx, _ := pool.TxBegin(ctx)
	root, entry, err := createRootThreadKernel(ctx, tx, creator, "Link gate", "root")
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := repository.FindThreadByID(ctx, pool, root.ID)

	tx, _ = pool.TxBegin(ctx)
	if _, err := addThreadRoleKernel(ctx, tx, creator, root.ID, target, model.ThreadWrite, entry.ID); err == nil {
		_ = pg.Rollback(tx)
		t.Fatal("non-linked Role was directly added")
	}
	_ = pg.Rollback(tx)

	threadTestActiveLink(t, pool, creator, target)
	tx, _ = pool.TxBegin(ctx)
	added, err := addThreadRoleKernel(ctx, tx, creator, root.ID, target, model.ThreadWrite, entry.ID)
	if err != nil || !added {
		_ = pg.Rollback(tx)
		t.Fatalf("linked add: added=%v err=%v", added, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := repository.FindThreadByID(ctx, pool, root.ID)
	if after.Revision != before.Revision+1 {
		t.Fatalf("participant add revision = %d, want %d", after.Revision, before.Revision+1)
	}
}

func TestThreadConcurrentSeqAndRoleJoinAreUnique(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	creator := pubSeedBot(t, pool, 0)
	target := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, creator, target)

	tx, _ := pool.TxBegin(ctx)
	root, rootEntry, err := createRootThreadKernel(ctx, tx, creator, "Concurrency", "root")
	if err != nil {
		_ = pg.Rollback(tx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	seqs := make(chan int64, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.TxBegin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = pg.Rollback(tx) }()
			entry, err := appendThreadMemoryKernel(ctx, tx, root.ID, creator, fmt.Sprintf("parallel entry %d", i), nil)
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			seqs <- entry.Seq
			errs <- nil
		}(i)
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel append: %v", err)
		}
	}
	var got []int
	seen := map[int64]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Fatalf("duplicate seq %d", seq)
		}
		seen[seq] = true
		got = append(got, int(seq))
	}
	sort.Ints(got)
	if len(got) != writers || got[0] != 2 || got[len(got)-1] != writers+1 {
		t.Fatalf("parallel seqs = %v, root seq=%d", got, rootEntry.Seq)
	}

	// Concurrent participant establishment converges on one ThreadRole.
	created := make(chan bool, writers)
	errs = make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := pool.TxBegin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = pg.Rollback(tx) }()
			ok, err := addThreadRoleKernel(ctx, tx, creator, root.ID, target, model.ThreadWrite, rootEntry.ID)
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			created <- ok
			errs <- nil
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	successes := 0
	for ok := range created {
		if ok {
			successes++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel role add: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("created ThreadRole count from callers = %d, want 1", successes)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_roles WHERE thread_id=$1 AND role_id=$2`, root.ID, target).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("persisted ThreadRole rows = %d, want 1", rows)
	}
}
