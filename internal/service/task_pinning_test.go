package service

// Task 1.1 §7.1/§7.3 input pinning: a confirmed engagement binds the
// required input versions. work_claim freezes each harness_ref's
// current revision; work_harness under that claim serves the pinned
// revision — through publisher edits and withdrawals — while
// everyone else keeps the live read. Claim-less submissions record
// the revisions they were prepared against. The 032 drill upgrades a
// database whose claims predate pinning and asserts they keep the
// live-read behavior.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// pinSeedBot seeds a bot on any pool (the drill's throwaway DB).
func pinSeedBot(t *testing.T, pool *pg.Pool, balance int64) int64 {
	t.Helper()
	name := fmt.Sprintf("pin_%d_%d", time.Now().UnixNano(), balance)
	digest := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'p1p1', 'x', $3) RETURNING id`,
		name, digest[:], balance).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return id
}

// bumpKungfuRevision performs the author's update transaction on one
// memory: lock → archive the current version → write the new content
// (revision + 1).
func bumpKungfuRevision(t *testing.T, pool *pg.Pool, botID int64, code, newContent string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	k, err := repository.LockOwnedActiveKungfuByID(ctx, tx, repositoryID(t, pool, code), botID)
	if err != nil || k == nil {
		t.Fatalf("lock memory: %v", err)
	}
	if err := repository.ArchiveKungfuRevision(ctx, tx, k.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	rev, err := repository.UpdateKungfuContentWithRevision(ctx, tx, k.ID, k.Title, k.TagsJSON, "", newContent, "checksum-"+newContent)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rev
}

func repositoryID(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	k, err := repository.FindKungfuByCodeAnyStatus(context.Background(), pool, code)
	if err != nil || k == nil {
		t.Fatalf("memory %s: %v", code, err)
	}
	return k.ID
}

// pinSeq names unique memories across this file's tests.
var pinSeq int64

// pinSeedKungfu inserts a publisher memory with custom content and
// returns its code.
func pinSeedKungfu(t *testing.T, pool *pg.Pool, botID int64, content string) string {
	t.Helper()
	pinSeq++
	code := fmt.Sprintf("pin%09d", pinSeq) // tb_kungfus.code is char(12)
	sum := sha256.Sum256([]byte(code))
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status)
		VALUES ($1, $2, 'Harness', '["t"]', $3, $4, 'private', 'active')`,
		code, botID, content, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("seed kungfu: %v", err)
	}
	return code
}

// pinningTask opens a claim-required task whose contract references
// one harness memory with the given content; returns (code, memoryCode).
func pinningTask(t *testing.T, pool *pg.Pool, publisher int64, memContent string) (string, string) {
	t.Helper()
	memCode := pinSeedKungfu(t, pool, publisher, memContent)
	c := claimContract()
	c.HarnessRefs = []string{memCode}
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code, memCode
}

// TestClaimPinsHarnessRevisions: after work_claim, publisher edits
// and even WITHDRAWALS of the memory do not change what the engaged
// agent reads; other agents read the live content.
func TestClaimPinsHarnessRevisions(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	bystander := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, memCode := pinningTask(t, pool, publisher, "harness v1 body")
	claim, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// the pin is on the claim row
	pinned, err := repository.ClaimHarnessRevision(ctx, pool, claim.ClaimID.Int64(), repositoryID(t, pool, memCode))
	if err != nil || pinned == nil || *pinned != 1 {
		t.Fatalf("pin = %v err=%v, want revision 1", pinned, err)
	}

	// engaged read (explicit claim_id): pinned revision 1
	gh, err := GetHarness(ctx, pool, agent, code, memCode, claim.ClaimID.Int64Ptr(), time.Now())
	if err != nil {
		t.Fatalf("harness engaged: %v", err)
	}
	if gh["content"] != "harness v1 body" || gh["revision"] != int64(1) || gh["pinned"] != true {
		t.Fatalf("engaged read = %v", gh)
	}
	// engaged read (claim discovered implicitly): same
	gh2, err := GetHarness(ctx, pool, agent, code, memCode, nil, time.Now())
	if err != nil || gh2["content"] != "harness v1 body" || gh2["pinned"] != true {
		t.Fatalf("implicit-claim read = %v err=%v", gh2, err)
	}

	// publisher edits the memory: engaged still reads revision 1,
	// the bystander reads the new content live
	bumpKungfuRevision(t, pool, publisher, memCode, "harness v2 body")
	gh, err = GetHarness(ctx, pool, agent, code, memCode, claim.ClaimID.Int64Ptr(), time.Now())
	if err != nil {
		t.Fatalf("harness after edit: %v", err)
	}
	if gh["content"] != "harness v1 body" || gh["revision"] != int64(1) || gh["pinned"] != true {
		t.Fatalf("engaged read after edit = %v, want pinned v1", gh)
	}
	live, err := GetHarness(ctx, pool, bystander, code, memCode, nil, time.Now())
	if err != nil {
		t.Fatalf("harness bystander: %v", err)
	}
	if live["content"] != "harness v2 body" || live["revision"] != int64(2) || live["pinned"] != false {
		t.Fatalf("live read = %v, want current v2", live)
	}

	// publisher withdraws the memory: the pinned version stays
	// readable to the engaged agent (§9); live readers get
	// HARNESS_REF_NOT_FOUND
	if _, err := pool.Exec(ctx, `UPDATE tb_kungfus SET status = 'deleted' WHERE code = $1`, memCode); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	gh, err = GetHarness(ctx, pool, agent, code, memCode, claim.ClaimID.Int64Ptr(), time.Now())
	if err != nil {
		t.Fatalf("harness after withdrawal: %v", err)
	}
	if gh["content"] != "harness v1 body" || gh["pinned"] != true {
		t.Fatalf("engaged read after withdrawal = %v, want the pinned v1", gh)
	}
	if _, err := GetHarness(ctx, pool, bystander, code, memCode, nil, time.Now()); err == nil {
		t.Fatal("live read of a withdrawn memory must be HARNESS_REF_NOT_FOUND")
	}
	claimTaskReserved(t, pool, code)
}

// TestWorkHarnessClaimGuards: an explicit claim that is not the
// caller's active claim on this task is CLAIM_INVALID; a ref outside
// the bound contract's harness_refs is HARNESS_REF_NOT_FOUND even
// when a later contract version references it.
func TestWorkHarnessClaimGuards(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	other := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, memCode := pinningTask(t, pool, publisher, "harness v1 body")
	claim, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	foreign, err := ClaimTask(ctx, pool, other, code, time.Now())
	if err != nil {
		t.Fatalf("foreign claim: %v", err)
	}
	if _, err := GetHarness(ctx, pool, agent, code, memCode, foreign.ClaimID.Int64Ptr(), time.Now()); err == nil {
		t.Fatal("another agent's claim_id must be CLAIM_INVALID")
	}

	// a ref that only a LATER version references is not part of the
	// bound contract
	mem2 := pinSeedKungfu(t, pool, publisher, "second harness")
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	v2 := claimContract()
	v2.HarnessRefs = []string{mem2}
	v2.Receiver = task.Receiver{URL: okReceiverURL}
	if _, err := UpdateTask(ctx, pool, publisher, code, v2); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := GetHarness(ctx, pool, agent, code, mem2, claim.ClaimID.Int64Ptr(), time.Now()); err == nil {
		t.Fatal("a ref outside the bound version must be HARNESS_REF_NOT_FOUND")
	}
	// an unengaged caller reads it live from the current version
	// (`other` holds the foreign claim, so take a fourth bot)
	gh, err := GetHarness(ctx, pool, pubSeedBot(t, pool, 0), code, mem2, nil, time.Now())
	if err != nil || gh["pinned"] != false {
		t.Fatalf("live read of the new ref = %v err=%v", gh, err)
	}
	claimTaskReserved(t, pool, code)
}

// TestClaimlessSubmissionRecordsHarnessSnapshot: a submission without
// a claim records the harness revisions current at intake on its row.
func TestClaimlessSubmissionRecordsHarnessSnapshot(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	memCode := pinSeedKungfu(t, pool, publisher, "harness body")
	c := submitContract() // claim not required
	c.HarnessRefs = []string{memCode}
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}

	view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "snap-1", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	var harness []byte
	if err := pool.QueryRow(ctx,
		`SELECT harness_json FROM tb_task_submission WHERE submission_id = $1`,
		view.SubmissionID.Int64()).Scan(&harness); err != nil {
		t.Fatalf("load harness_json: %v", err)
	}
	var entries []harnessSnapshotEntry
	if err := json.Unmarshal(harness, &entries); err != nil {
		t.Fatalf("harness_json = %s: %v", harness, err)
	}
	if len(entries) != 1 || entries[0].Code != memCode || entries[0].Revision != 1 {
		t.Fatalf("harness_json = %s, want [{%s revision 1}]", harness, memCode)
	}
	claimTaskReserved(t, pool, code) // + invariants
}

// -- the 032 upgrade drill --

// TestTask11HarnessPinningUpgradeDrill: claims that predate
// migration 032 carry no pins and keep the Task 1.0 live read —
// including through a later memory edit (the read follows the
// current content). Claims formed after the upgrade pin normally.
func TestTask11HarnessPinningUpgradeDrill(t *testing.T) {
	pubTestPool(t) // presence + skip contract; throwaway DB below
	ctx := context.Background()

	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v", err)
	}
	sort.Strings(files)
	split := -1
	for i, f := range files {
		if strings.Contains(f, "032_") {
			split = i
			break
		}
	}
	if split < 0 {
		t.Fatal("032 migration not found")
	}
	pre, m032 := files[:split], files[split]

	dbName := "kf_task11b_" + nanoSuffix()
	adminURL := ""
	if i := strings.LastIndex(os.Getenv("KF_TEST_DATABASE_URL"), "/"); i >= 0 {
		adminURL = os.Getenv("KF_TEST_DATABASE_URL")[:i] + "/postgres"
	}
	if adminURL == "" {
		t.Skip("cannot derive admin DSN")
	}
	admin, err := pg.NewPool(adminURL)
	if err != nil {
		t.Skipf("admin connection unavailable: %v", err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Skipf("create db: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`) })
	dbURL := adminURL[:strings.LastIndex(adminURL, "/")] + "/" + dbName
	db, err := pg.NewPool(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	for _, f := range pre {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	// publisher + agent + memory + open task with a harness ref + an
	// ACTIVE claim (the pre-032 shape: no pins table yet)
	publisher := pinSeedBot(t, db, 100000)
	agent := pinSeedBot(t, db, 0)
	memCode := pinSeedKungfu(t, db, publisher, "pre-upgrade body")
	c := claimContract()
	c.HarnessRefs = []string{memCode}
	code := pubCreateForTest(t, db, publisher, c, 1000)
	if _, err := OpenTask(ctx, db, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	// the pre-032 claim is seeded raw: ClaimTask in its 1.1 shape
	// writes claim_harness_revisions, which does not exist yet
	var legacyClaimID int64
	tr, err := repository.FindTaskByCode(ctx, db, code)
	if err != nil || tr == nil {
		t.Fatalf("load task: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_task_claim (task_id, agent_id, contract_version, expires_at, deadline, amount, status)
		VALUES ($1, $2, 1, NOW() + interval '1 hour', NOW() + interval '2 hours', $3, 'active')
		RETURNING claim_id`, tr.ID, agent, c.Price).Scan(&legacyClaimID); err != nil {
		t.Fatalf("seed pre-upgrade claim: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE tb_task SET reserved = $2 WHERE id = $1`, tr.ID, c.Price); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	legacyClaim := claimView{ClaimID: WireID(legacyClaimID)}

	// apply the shipped 032
	sqlBytes, err := os.ReadFile(m032)
	if err != nil {
		t.Fatalf("read %s: %v", m032, err)
	}
	if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply 032: %v", err)
	}

	// the legacy claim has no pin and keeps the live read
	gh, err := GetHarness(ctx, db, agent, code, memCode, legacyClaim.ClaimID.Int64Ptr(), time.Now())
	if err != nil || gh["pinned"] != false {
		t.Fatalf("legacy claim read = %v err=%v, want live (pinned=false)", gh, err)
	}
	bumpKungfuRevision(t, db, publisher, memCode, "post-upgrade body")
	gh, err = GetHarness(ctx, db, agent, code, memCode, legacyClaim.ClaimID.Int64Ptr(), time.Now())
	if err != nil || gh["content"] != "post-upgrade body" {
		t.Fatalf("legacy claim still reads live = %v err=%v, want the current content", gh, err)
	}

	// a claim formed after the upgrade pins normally
	if _, err := ReleaseClaim(ctx, db, agent, legacyClaimID, time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	fresh, err := ClaimTask(ctx, db, agent, code, time.Now())
	if err != nil {
		t.Fatalf("post-upgrade claim: %v", err)
	}
	bumpKungfuRevision(t, db, publisher, memCode, "third body")
	gh, err = GetHarness(ctx, db, agent, code, memCode, fresh.ClaimID.Int64Ptr(), time.Now())
	if err != nil || gh["content"] != "post-upgrade body" || gh["pinned"] != true {
		t.Fatalf("fresh claim pinned read = %v err=%v, want the revision it pinned", gh, err)
	}
}
