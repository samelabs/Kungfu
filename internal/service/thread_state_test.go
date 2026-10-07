package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func threadIdem(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func int64Ptr(v int64) *int64 { return &v }

type threadPairFixture struct {
	A      int64
	B      int64
	Thread *model.Thread
	Root   *model.ThreadMemory
}

func threadPairWithPendingB(t *testing.T, pool *pg.Pool) threadPairFixture {
	t.Helper()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)
	result, err := CreateThreadState(context.Background(), pool, a, ThreadCreateInput{
		Subject: "Pair work",
		Content: "root input",
		Participants: []ThreadParticipantSpec{
			{RoleID: b, Permission: model.ThreadWrite},
		},
		IdempotencyKey: threadIdem("create"),
	})
	if err != nil {
		t.Fatalf("CreateThreadState: %v", err)
	}
	return threadPairFixture{A: a, B: b, Thread: result.Thread, Root: result.RootEntry}
}

func countReceipts(t *testing.T, pool *pg.Pool, threadID, roleID int64, state string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM thread_receipts
		WHERE thread_id=$1 AND role_id=$2 AND state=$3`,
		threadID, roleID, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestThreadRoundTripTodoReplyHandle(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)

	if got := countReceipts(t, pool, f.Thread.ID, f.B, model.ThreadReceiptPending); got != 1 {
		t.Fatalf("B initial pending = %d, want 1", got)
	}
	reply, err := ReplyThreadState(ctx, pool, f.B, ThreadReplyInput{
		ThreadID: f.Thread.ID, ReplyToEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
		Content: "B response", IdempotencyKey: threadIdem("reply"),
	})
	if err != nil {
		t.Fatalf("B reply: %v", err)
	}
	bReceipt, _ := repository.FindThreadReceipt(ctx, pool, f.Thread.ID, f.Root.ID, f.B)
	if bReceipt == nil || bReceipt.State != model.ThreadReceiptHandled {
		t.Fatalf("B root receipt = %+v", bReceipt)
	}
	aReceipt, _ := repository.FindThreadReceipt(ctx, pool, f.Thread.ID, reply.Entry.ID, f.A)
	if aReceipt == nil || aReceipt.State != model.ThreadReceiptPending || aReceipt.Reason != model.ThreadReceiptReply {
		t.Fatalf("A reply receipt = %+v", aReceipt)
	}
	if _, err := HandleThreadInput(ctx, pool, f.A, f.Thread.ID, reply.Entry.ID, threadIdem("handle")); err != nil {
		t.Fatalf("A handle: %v", err)
	}
	if got := countReceipts(t, pool, f.Thread.ID, f.A, model.ThreadReceiptPending); got != 0 {
		t.Fatalf("A pending after handle = %d", got)
	}
}

func TestThreadReplyIdempotencyAndConflict(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)
	key := threadIdem("reply-idem")
	in := ThreadReplyInput{
		ThreadID: f.Thread.ID, ReplyToEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
		Content: "one durable reply", IdempotencyKey: key,
	}
	first, err := ReplyThreadState(ctx, pool, f.B, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReplyThreadState(ctx, pool, f.B, in)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !second.AlreadyApplied || second.Entry.ID != first.Entry.ID {
		t.Fatalf("reply replay = %+v first=%+v", second, first)
	}
	var entries int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Fatalf("timeline entries after replay = %d, want root+one reply", entries)
	}
	conflict := in
	conflict.Content = "different request under same key"
	if _, err := ReplyThreadState(ctx, pool, f.B, conflict); !errors.Is(err, ErrThreadIdempotencyConflict) {
		t.Fatalf("same key/different request err=%v, want IDEMPOTENCY conflict", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Fatalf("conflict wrote an extra entry: %d", entries)
	}
}

func TestStaleTodoReplyProducesNoWrite(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)

	if _, err := HandleThreadInput(ctx, pool, f.B, f.Thread.ID, f.Root.ID, threadIdem("consume")); err != nil {
		t.Fatal(err)
	}
	var memoryBefore, entryBefore, childBefore int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus WHERE origin='thread'`).Scan(&memoryBefore)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entryBefore)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM threads WHERE parent_thread_id=$1`, f.Thread.ID).Scan(&childBefore)

	_, err := ReplyThreadState(ctx, pool, f.B, ThreadReplyInput{
		ThreadID: f.Thread.ID, ReplyToEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
		Content: "must not persist", IdempotencyKey: threadIdem("stale-reply"),
	})
	if !errors.Is(err, ErrThreadStaleInput) {
		t.Fatalf("stale reply err=%v", err)
	}
	var memoryAfter, entryAfter, childAfter int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus WHERE origin='thread'`).Scan(&memoryAfter)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entryAfter)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM threads WHERE parent_thread_id=$1`, f.Thread.ID).Scan(&childAfter)
	if memoryAfter != memoryBefore || entryAfter != entryBefore || childAfter != childBefore {
		t.Fatalf("stale action wrote state: memory %d→%d entry %d→%d child %d→%d",
			memoryBefore, memoryAfter, entryBefore, entryAfter, childBefore, childAfter)
	}
}

func TestTodoRaceHandleVsReply(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := HandleThreadInput(ctx, pool, f.B, f.Thread.ID, f.Root.ID, threadIdem("race-handle"))
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := ReplyThreadState(ctx, pool, f.B, ThreadReplyInput{
			ThreadID: f.Thread.ID, ReplyToEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
			Content: "race reply", IdempotencyKey: threadIdem("race-reply"),
		})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)

	success, stale := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrThreadStaleInput):
			stale++
		default:
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("handle/reply race success=%d stale=%d, want 1/1", success, stale)
	}
	var entries int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries < 1 || entries > 2 {
		t.Fatalf("race produced impossible timeline count %d", entries)
	}
}

func TestTodoRaceReplyVsBranch(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := ReplyThreadState(ctx, pool, f.B, ThreadReplyInput{
			ThreadID: f.Thread.ID, ReplyToEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
			Content: "race reply", IdempotencyKey: threadIdem("race-rb-reply"),
		})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := BranchThreadState(ctx, pool, f.B, ThreadBranchInput{
			ParentThreadID: f.Thread.ID, AnchorEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
			Subject: "race child", IdempotencyKey: threadIdem("race-rb-branch"),
		})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrThreadStaleInput):
			stale++
		default:
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("reply/branch race success=%d stale=%d, want 1/1", success, stale)
	}
	var entries, children int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_memories WHERE thread_id=$1`, f.Thread.ID).Scan(&entries)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM threads WHERE parent_thread_id=$1`, f.Thread.ID).Scan(&children)
	if (entries == 2 && children != 0) || (entries == 1 && children != 1) || entries < 1 || entries > 2 || children > 1 {
		t.Fatalf("reply/branch race effects entries=%d children=%d", entries, children)
	}
}

func TestMultiplePendingAndBranchChildTodo(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	c := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)
	threadTestActiveLink(t, pool, a, c)
	threadTestActiveLink(t, pool, b, c)

	root, err := CreateThreadState(ctx, pool, a, ThreadCreateInput{
		Subject: "Multi", Content: "root",
		Participants:   []ThreadParticipantSpec{{RoleID: b}, {RoleID: c}},
		IdempotencyKey: threadIdem("multi-create"),
	})
	if err != nil {
		t.Fatal(err)
	}
	bReply, err := ReplyThreadState(ctx, pool, b, ThreadReplyInput{
		ThreadID: root.Thread.ID, ReplyToEntryID: root.RootEntry.ID, InputEntryID: int64Ptr(root.RootEntry.ID),
		Content: "B reply", IdempotencyKey: threadIdem("multi-b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplyThreadState(ctx, pool, c, ThreadReplyInput{
		ThreadID: root.Thread.ID, ReplyToEntryID: root.RootEntry.ID, InputEntryID: int64Ptr(root.RootEntry.ID),
		Content: "C reply", IdempotencyKey: threadIdem("multi-c"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, a, model.ThreadReceiptPending); got != 2 {
		t.Fatalf("A pending inputs = %d, want 2", got)
	}

	// Branch one specific pending input; the other remains pending.
	child, err := BranchThreadState(ctx, pool, a, ThreadBranchInput{
		ParentThreadID: root.Thread.ID, AnchorEntryID: bReply.Entry.ID, InputEntryID: int64Ptr(bReply.Entry.ID),
		Subject:        "Child work",
		Participants:   []ThreadParticipantSpec{{RoleID: c, Permission: model.ThreadWrite}},
		IdempotencyKey: threadIdem("multi-branch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, a, model.ThreadReceiptPending); got != 1 {
		t.Fatalf("A pending after one branch = %d, want 1", got)
	}
	childReceipt, _ := repository.FindThreadReceipt(ctx, pool, child.Thread.ID, bReply.Entry.ID, c)
	if childReceipt == nil || childReceipt.State != model.ThreadReceiptPending || childReceipt.Reason != model.ThreadReceiptEntry {
		t.Fatalf("Child initial receipt = %+v", childReceipt)
	}
}

func TestPermissionCloseReopenAndReadRoleTodoRules(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	c := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)
	threadTestActiveLink(t, pool, a, c)

	root, err := CreateThreadState(ctx, pool, a, ThreadCreateInput{
		Subject: "Permissions", Content: "root",
		Participants: []ThreadParticipantSpec{
			{RoleID: b, Permission: model.ThreadWrite},
			{RoleID: c, Permission: model.ThreadRead},
		},
		IdempotencyKey: threadIdem("perm-create"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, c, model.ThreadReceiptPending); got != 0 {
		t.Fatalf("read Role got %d Todo receipts", got)
	}
	bTodo, err := repository.ListPendingThreadReceiptsForRole(ctx, pool, b)
	if err != nil || len(bTodo) != 1 || bTodo[0].InputEntryID != root.RootEntry.ID {
		t.Fatalf("B Todo facts = %+v err=%v", bTodo, err)
	}
	cTodo, err := repository.ListPendingThreadReceiptsForRole(ctx, pool, c)
	if err != nil || len(cTodo) != 0 {
		t.Fatalf("read Role leaked into Todo facts = %+v err=%v", cTodo, err)
	}
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadRead, nil, threadIdem("downgrade")); err != nil {
		t.Fatal(err)
	}
	r, _ := repository.FindThreadReceipt(ctx, pool, root.Thread.ID, root.RootEntry.ID, b)
	if r == nil || r.State != model.ThreadReceiptWithdrawn {
		t.Fatalf("downgraded receipt = %+v", r)
	}
	if todo, err := repository.ListPendingThreadReceiptsForRole(ctx, pool, b); err != nil || len(todo) != 0 {
		t.Fatalf("downgraded Role remained in Todo facts = %+v err=%v", todo, err)
	}

	// Create a different current entry for B's new actionable entry.
	proactive, err := ReplyThreadState(ctx, pool, a, ThreadReplyInput{
		ThreadID: root.Thread.ID, ReplyToEntryID: root.RootEntry.ID,
		Content: "new work point", IdempotencyKey: threadIdem("proactive"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadWrite,
		int64Ptr(proactive.Entry.ID), threadIdem("upgrade")); err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, b, model.ThreadReceiptPending); got != 1 {
		t.Fatalf("upgraded B pending = %d, want 1", got)
	}
	if todo, err := repository.ListPendingThreadReceiptsForRole(ctx, pool, b); err != nil || len(todo) != 1 || todo[0].InputEntryID != proactive.Entry.ID {
		t.Fatalf("upgraded Role Todo facts = %+v err=%v", todo, err)
	}

	beforeEntryGuard, _ := repository.FindThreadByID(ctx, pool, root.Thread.ID)
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadWrite,
		int64Ptr(root.RootEntry.ID), threadIdem("same-permission-entry-change")); err == nil {
		t.Fatal("same actionable permission silently accepted a different entry")
	}
	afterEntryGuard, _ := repository.FindThreadByID(ctx, pool, root.Thread.ID)
	if afterEntryGuard.Revision != beforeEntryGuard.Revision {
		t.Fatalf("rejected entry change bumped revision: %d -> %d", beforeEntryGuard.Revision, afterEntryGuard.Revision)
	}

	if _, err := CloseThreadState(ctx, pool, a, root.Thread.ID, threadIdem("close")); err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, b, model.ThreadReceiptPending); got != 0 {
		t.Fatalf("close left %d pending", got)
	}
	if todo, err := repository.ListPendingThreadReceiptsForRole(ctx, pool, b); err != nil || len(todo) != 0 {
		t.Fatalf("closed Thread leaked into Todo facts = %+v err=%v", todo, err)
	}
	if _, err := ReopenThreadState(ctx, pool, a, root.Thread.ID, threadIdem("reopen")); err != nil {
		t.Fatal(err)
	}
	if got := countReceipts(t, pool, root.Thread.ID, b, model.ThreadReceiptPending); got != 0 {
		t.Fatalf("reopen restored %d old pending receipts", got)
	}
}

func TestParticipantRemovalRevokesJoinKeyButChildSurvives(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)

	child, err := BranchThreadState(ctx, pool, f.B, ThreadBranchInput{
		ParentThreadID: f.Thread.ID, AnchorEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
		Subject: "Independent child", IdempotencyKey: threadIdem("child"),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ResetThreadJoinKeyState(ctx, pool, f.A, f.Thread.ID, f.Root.ID, threadIdem("key"))
	if err != nil || key.JoinKey == "" {
		t.Fatalf("reset key: %+v err=%v", key, err)
	}
	oldKeyJoiner := pubSeedBot(t, pool, 0)
	if _, err := RemoveThreadParticipant(ctx, pool, f.A, f.Thread.ID, f.B, threadIdem("remove")); err != nil {
		t.Fatal(err)
	}
	parent, _ := repository.FindThreadByID(ctx, pool, f.Thread.ID)
	if len(parent.JoinKeyHash) != 0 || parent.JoinEntryID != nil {
		t.Fatalf("participant removal did not revoke join key: %+v", parent)
	}
	if _, err := JoinThreadState(ctx, pool, oldKeyJoiner, key.JoinKey, threadIdem("old-key-after-remove")); err == nil {
		t.Fatal("revoked join key remained usable after participant removal")
	}
	if role, err := repository.FindThreadRole(ctx, pool, child.Thread.ID, f.B); err != nil || role == nil || role.Permission != model.ThreadManage {
		t.Fatalf("Child participation was cascaded: role=%+v err=%v", role, err)
	}
	childAfter, _ := repository.FindThreadByID(ctx, pool, child.Thread.ID)
	if childAfter == nil || childAfter.Status != model.ThreadOpen {
		t.Fatalf("Child lifecycle changed: %+v", childAfter)
	}
}

func TestCreateJoinAndKeyResetOneTimeDisclosure(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	createKey := threadIdem("key-create")
	in := ThreadCreateInput{
		Subject: "Join path", Content: "entry", IssueJoinKey: true, IdempotencyKey: createKey,
	}
	first, err := CreateThreadState(ctx, pool, a, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.JoinKey == "" || first.JoinKeyFingerprint == "" {
		t.Fatalf("first create did not disclose join key once: %+v", first)
	}
	replay, err := CreateThreadState(ctx, pool, a, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.AlreadyApplied || replay.JoinKey != "" || replay.JoinKeyFingerprint != first.JoinKeyFingerprint {
		t.Fatalf("create replay re-disclosed or lost fingerprint: %+v", replay)
	}

	joined, err := JoinThreadState(ctx, pool, b, first.JoinKey, threadIdem("join"))
	if err != nil || !joined.Joined || joined.Role.Permission != model.ThreadWrite || joined.Role.EntryID != first.RootEntry.ID {
		t.Fatalf("join = %+v err=%v", joined, err)
	}
	if got := countReceipts(t, pool, first.Thread.ID, b, model.ThreadReceiptPending); got != 1 {
		t.Fatalf("join pending = %d, want 1", got)
	}
	repeatJoin, err := JoinThreadState(ctx, pool, b, first.JoinKey, threadIdem("join-again"))
	if err != nil || repeatJoin.Joined {
		t.Fatalf("existing participant join = %+v err=%v", repeatJoin, err)
	}
	if got := countReceipts(t, pool, first.Thread.ID, b, model.ThreadReceiptPending); got != 1 {
		t.Fatalf("repeat join duplicated receipt: %d", got)
	}

	resetKey := threadIdem("reset")
	reset, err := ResetThreadJoinKeyState(ctx, pool, a, first.Thread.ID, first.RootEntry.ID, resetKey)
	if err != nil || reset.JoinKey == "" || reset.JoinKey == first.JoinKey {
		t.Fatalf("reset = %+v err=%v", reset, err)
	}
	resetReplay, err := ResetThreadJoinKeyState(ctx, pool, a, first.Thread.ID, first.RootEntry.ID, resetKey)
	if err != nil {
		t.Fatal(err)
	}
	if !resetReplay.AlreadyApplied || resetReplay.JoinKey != "" || resetReplay.JoinKeyFingerprint != reset.JoinKeyFingerprint {
		t.Fatalf("reset replay = %+v", resetReplay)
	}

	reset2, err := ResetThreadJoinKeyState(ctx, pool, a, first.Thread.ID, first.RootEntry.ID, threadIdem("reset-2"))
	if err != nil || reset2.JoinKey == "" || reset2.JoinKeyFingerprint == reset.JoinKeyFingerprint {
		t.Fatalf("second reset = %+v err=%v", reset2, err)
	}
	oldResetReplay, err := ResetThreadJoinKeyState(ctx, pool, a, first.Thread.ID, first.RootEntry.ID, resetKey)
	if err != nil {
		t.Fatal(err)
	}
	if !oldResetReplay.AlreadyApplied || oldResetReplay.JoinKey != "" || oldResetReplay.JoinKeyFingerprint != reset.JoinKeyFingerprint {
		t.Fatalf("old reset replay drifted from original fingerprint: %+v original=%+v current=%+v", oldResetReplay, reset, reset2)
	}
	createReplayAfterReset, err := CreateThreadState(ctx, pool, a, in)
	if err != nil {
		t.Fatal(err)
	}
	if !createReplayAfterReset.AlreadyApplied || createReplayAfterReset.JoinKey != "" || createReplayAfterReset.JoinKeyFingerprint != first.JoinKeyFingerprint {
		t.Fatalf("create replay drifted from original fingerprint: %+v original=%+v current=%+v", createReplayAfterReset, first, reset2)
	}
}

func TestIdempotentCreateAndReplySnapshotsDoNotDrift(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)

	createKey := threadIdem("snapshot-create")
	createIn := ThreadCreateInput{
		Subject: "original subject", Content: "root",
		Participants: []ThreadParticipantSpec{{RoleID: b}},
		IdempotencyKey: createKey,
	}
	created, err := CreateThreadState(ctx, pool, a, createIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateThreadSubjectState(ctx, pool, a, created.Thread.ID, "changed subject", threadIdem("snapshot-create-subject")); err != nil {
		t.Fatal(err)
	}
	createReplay, err := CreateThreadState(ctx, pool, a, createIn)
	if err != nil {
		t.Fatal(err)
	}
	if !createReplay.AlreadyApplied || createReplay.Thread.Subject != "original subject" {
		t.Fatalf("create replay drifted: %+v", createReplay)
	}
	if createReplay.Thread.Revision != created.Thread.Revision {
		t.Fatalf("create replay revision drifted: got %d want %d", createReplay.Thread.Revision, created.Thread.Revision)
	}

	replyKey := threadIdem("snapshot-reply")
	replyIn := ThreadReplyInput{
		ThreadID: created.Thread.ID, ReplyToEntryID: created.RootEntry.ID, InputEntryID: int64Ptr(created.RootEntry.ID),
		Content: "B response", IdempotencyKey: replyKey,
	}
	reply, err := ReplyThreadState(ctx, pool, b, replyIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateThreadSubjectState(ctx, pool, a, created.Thread.ID, "changed after reply", threadIdem("snapshot-reply-subject")); err != nil {
		t.Fatal(err)
	}
	replyReplay, err := ReplyThreadState(ctx, pool, b, replyIn)
	if err != nil {
		t.Fatal(err)
	}
	if !replyReplay.AlreadyApplied || replyReplay.Entry.ID != reply.Entry.ID {
		t.Fatalf("reply replay lost original entry: %+v first=%+v", replyReplay, reply)
	}
	if replyReplay.Thread.Subject != reply.Thread.Subject || replyReplay.Thread.Revision != reply.Thread.Revision {
		t.Fatalf("reply replay drifted: replay=%+v first=%+v", replyReplay.Thread, reply.Thread)
	}
}

func TestIdempotentMembershipSnapshotsDoNotDrift(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	c := pubSeedBot(t, pool, 0)
	d := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)
	threadTestActiveLink(t, pool, a, c)

	root, err := CreateThreadState(ctx, pool, a, ThreadCreateInput{
		Subject: "membership snapshots", Content: "root",
		Participants: []ThreadParticipantSpec{{RoleID: b}},
		IdempotencyKey: threadIdem("snapshot-membership-create"),
	})
	if err != nil {
		t.Fatal(err)
	}

	addKey := threadIdem("snapshot-add")
	added, changed, err := AddThreadParticipant(ctx, pool, a, root.Thread.ID,
		ThreadParticipantSpec{RoleID: c, Permission: model.ThreadWrite}, root.RootEntry.ID, addKey)
	if err != nil || !changed {
		t.Fatalf("add: role=%+v changed=%v err=%v", added, changed, err)
	}
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, c, model.ThreadRead, nil, threadIdem("snapshot-add-mutate")); err != nil {
		t.Fatal(err)
	}
	addReplay, replayChanged, err := AddThreadParticipant(ctx, pool, a, root.Thread.ID,
		ThreadParticipantSpec{RoleID: c, Permission: model.ThreadWrite}, root.RootEntry.ID, addKey)
	if err != nil {
		t.Fatal(err)
	}
	if !replayChanged || addReplay.Permission != model.ThreadWrite || !addReplay.JoinedAt.Equal(added.JoinedAt) {
		t.Fatalf("add replay drifted: replay=%+v changed=%v first=%+v", addReplay, replayChanged, added)
	}

	permKey := threadIdem("snapshot-permission")
	permFirst, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadRead, nil, permKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadWrite,
		int64Ptr(root.RootEntry.ID), threadIdem("snapshot-permission-mutate")); err != nil {
		t.Fatal(err)
	}
	permReplay, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, b, model.ThreadRead, nil, permKey)
	if err != nil {
		t.Fatal(err)
	}
	if permReplay.Permission != permFirst.Permission || permReplay.EntryID != permFirst.EntryID {
		t.Fatalf("permission replay drifted: replay=%+v first=%+v", permReplay, permFirst)
	}

	key, err := ResetThreadJoinKeyState(ctx, pool, a, root.Thread.ID, root.RootEntry.ID, threadIdem("snapshot-join-key"))
	if err != nil {
		t.Fatal(err)
	}
	joinKey := threadIdem("snapshot-join")
	joined, err := JoinThreadState(ctx, pool, d, key.JoinKey, joinKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ChangeThreadPermission(ctx, pool, a, root.Thread.ID, d, model.ThreadRead, nil, threadIdem("snapshot-join-mutate")); err != nil {
		t.Fatal(err)
	}
	joinReplay, err := JoinThreadState(ctx, pool, d, key.JoinKey, joinKey)
	if err != nil {
		t.Fatal(err)
	}
	if !joinReplay.AlreadyApplied || !joinReplay.Joined || joinReplay.Role.Permission != joined.Role.Permission ||
		joinReplay.Role.EntryID != joined.Role.EntryID || joinReplay.Thread.Revision != joined.Thread.Revision {
		t.Fatalf("join replay drifted: replay=%+v first=%+v", joinReplay, joined)
	}
}

func TestIdempotentThreadStateSnapshotsDoNotDrift(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	root, err := CreateThreadState(ctx, pool, a, ThreadCreateInput{
		Subject: "state snapshots", Content: "root", IdempotencyKey: threadIdem("snapshot-state-create"),
	})
	if err != nil {
		t.Fatal(err)
	}

	subjectKey := threadIdem("snapshot-subject")
	subjectFirst, err := UpdateThreadSubjectState(ctx, pool, a, root.Thread.ID, "subject B", subjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateThreadSubjectState(ctx, pool, a, root.Thread.ID, "subject C", threadIdem("snapshot-subject-mutate")); err != nil {
		t.Fatal(err)
	}
	subjectReplay, err := UpdateThreadSubjectState(ctx, pool, a, root.Thread.ID, "subject B", subjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if subjectReplay.Subject != subjectFirst.Subject || subjectReplay.Revision != subjectFirst.Revision {
		t.Fatalf("subject replay drifted: replay=%+v first=%+v", subjectReplay, subjectFirst)
	}

	closeKey := threadIdem("snapshot-close")
	closed, err := CloseThreadState(ctx, pool, a, root.Thread.ID, closeKey)
	if err != nil {
		t.Fatal(err)
	}
	reopenKey := threadIdem("snapshot-reopen")
	reopened, err := ReopenThreadState(ctx, pool, a, root.Thread.ID, reopenKey)
	if err != nil {
		t.Fatal(err)
	}
	closeReplay, err := CloseThreadState(ctx, pool, a, root.Thread.ID, closeKey)
	if err != nil {
		t.Fatal(err)
	}
	if closeReplay.Status != model.ThreadClosed || closeReplay.Revision != closed.Revision {
		t.Fatalf("close replay drifted: replay=%+v first=%+v current=%+v", closeReplay, closed, reopened)
	}

	if _, err := CloseThreadState(ctx, pool, a, root.Thread.ID, threadIdem("snapshot-reopen-mutate")); err != nil {
		t.Fatal(err)
	}
	reopenReplay, err := ReopenThreadState(ctx, pool, a, root.Thread.ID, reopenKey)
	if err != nil {
		t.Fatal(err)
	}
	if reopenReplay.Status != model.ThreadOpen || reopenReplay.Revision != reopened.Revision {
		t.Fatalf("reopen replay drifted: replay=%+v first=%+v", reopenReplay, reopened)
	}
}

func TestIdempotentBranchSnapshotDoesNotDriftOrRediscloseKey(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	threadTestActiveLink(t, pool, a, b)

	root, err := CreateThreadState(ctx, pool, a, ThreadCreateInput{
		Subject: "branch parent", Content: "root",
		Participants: []ThreadParticipantSpec{{RoleID: b}},
		IdempotencyKey: threadIdem("snapshot-branch-parent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	branchKey := threadIdem("snapshot-branch")
	in := ThreadBranchInput{
		ParentThreadID: root.Thread.ID, AnchorEntryID: root.RootEntry.ID, InputEntryID: int64Ptr(root.RootEntry.ID),
		Subject: "branch original", IssueJoinKey: true, IdempotencyKey: branchKey,
	}
	first, err := BranchThreadState(ctx, pool, b, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.JoinKey == "" || first.JoinKeyFingerprint == "" {
		t.Fatalf("branch did not return first-use key: %+v", first)
	}
	if _, err := UpdateThreadSubjectState(ctx, pool, b, first.Thread.ID, "branch changed", threadIdem("snapshot-branch-subject")); err != nil {
		t.Fatal(err)
	}
	secondKey, err := ResetThreadJoinKeyState(ctx, pool, b, first.Thread.ID, root.RootEntry.ID, threadIdem("snapshot-branch-key-mutate"))
	if err != nil {
		t.Fatal(err)
	}
	if secondKey.JoinKeyFingerprint == first.JoinKeyFingerprint {
		t.Fatal("branch key mutation did not change fingerprint")
	}
	replay, err := BranchThreadState(ctx, pool, b, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.AlreadyApplied || replay.JoinKey != "" {
		t.Fatalf("branch replay re-disclosed key: %+v", replay)
	}
	if replay.JoinKeyFingerprint != first.JoinKeyFingerprint || replay.Thread.Subject != first.Thread.Subject ||
		replay.Thread.Revision != first.Thread.Revision {
		t.Fatalf("branch replay drifted: replay=%+v first=%+v currentKey=%+v", replay, first, secondKey)
	}
}

func TestChildCloseReopenBumpsParentWithoutCascading(t *testing.T) {
	pool := pubTestPool(t)
	ctx := context.Background()
	f := threadPairWithPendingB(t, pool)
	child, err := BranchThreadState(ctx, pool, f.B, ThreadBranchInput{
		ParentThreadID: f.Thread.ID, AnchorEntryID: f.Root.ID, InputEntryID: int64Ptr(f.Root.ID),
		Subject: "Child status", IdempotencyKey: threadIdem("branch-status"),
	})
	if err != nil {
		t.Fatal(err)
	}
	parentBefore, _ := repository.FindThreadByID(ctx, pool, f.Thread.ID)
	if _, err := CloseThreadState(ctx, pool, f.A, child.Thread.ID, threadIdem("close-child")); err != nil {
		t.Fatalf("root govern close child: %v", err)
	}
	parentAfterClose, _ := repository.FindThreadByID(ctx, pool, f.Thread.ID)
	if parentAfterClose.Revision != parentBefore.Revision+1 {
		t.Fatalf("child close parent revision = %d, want %d", parentAfterClose.Revision, parentBefore.Revision+1)
	}
	if _, err := ReopenThreadState(ctx, pool, f.A, child.Thread.ID, threadIdem("reopen-child")); err != nil {
		t.Fatalf("root govern reopen child: %v", err)
	}
	parentAfterReopen, _ := repository.FindThreadByID(ctx, pool, f.Thread.ID)
	if parentAfterReopen.Revision != parentAfterClose.Revision+1 {
		t.Fatalf("child reopen parent revision = %d, want %d", parentAfterReopen.Revision, parentAfterClose.Revision+1)
	}
}
