package service

import (
	"context"
	"strings"
	"testing"

	"kungfu.md/internal/repository"
)

func TestMemoryRevisionUpdateArchivesCurrentAndPinsRemainReadable(t *testing.T) {
	pool := pushTestPool(t)
	botID := pushTestBot(t, pool)
	ctx := context.Background()

	created, err := Push(ctx, pool, botID, map[string]interface{}{
		"title":   "Revision one",
		"tags":    []interface{}{"revision"},
		"content": strings.Repeat("a", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, created.Code)
	if err != nil || before == nil {
		t.Fatalf("load before: %v", err)
	}
	if before.Revision != 1 || before.Origin != "standalone" {
		t.Fatalf("before = revision %d origin %q, want 1/standalone", before.Revision, before.Origin)
	}

	_, err = Push(ctx, pool, botID, map[string]interface{}{
		"code":    created.Code,
		"title":   "Revision two",
		"tags":    []interface{}{"revision", "updated"},
		"content": strings.Repeat("b", 64),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	current, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, created.Code)
	if err != nil || current == nil {
		t.Fatalf("load current: %v", err)
	}
	if current.Revision != 2 || current.Origin != "standalone" || current.Title != "Revision two" {
		t.Fatalf("current = revision %d origin %q title %q", current.Revision, current.Origin, current.Title)
	}

	rev1, err := repository.FindMemoryRevision(ctx, pool, current.ID, 1)
	if err != nil || rev1 == nil {
		t.Fatalf("resolve revision 1: %v", err)
	}
	if rev1.Title != "Revision one" || rev1.Content != strings.Repeat("a", 60) {
		t.Fatalf("revision 1 changed: %+v", rev1)
	}
	rev2, err := repository.FindMemoryRevision(ctx, pool, current.ID, 2)
	if err != nil || rev2 == nil {
		t.Fatalf("resolve current revision 2: %v", err)
	}
	if rev2.Title != "Revision two" || rev2.Content != strings.Repeat("b", 64) {
		t.Fatalf("revision 2 mismatch: %+v", rev2)
	}

	var historyRows int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id=$1`, current.ID).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if historyRows != 1 {
		t.Fatalf("history rows = %d, want 1 (only the overwritten revision)", historyRows)
	}

	if err := repository.SoftDeleteKungfuByID(ctx, pool, current.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if pinned, err := repository.FindMemoryRevision(ctx, pool, current.ID, 1); err != nil || pinned == nil || pinned.Title != "Revision one" {
		t.Fatalf("historical pinned revision after soft-delete = %+v, err=%v", pinned, err)
	}
	if pinned, err := repository.FindMemoryRevision(ctx, pool, current.ID, 2); err != nil || pinned == nil || pinned.Title != "Revision two" {
		t.Fatalf("current pinned revision after soft-delete = %+v, err=%v", pinned, err)
	}
}

func TestMemoryUpdateFailureRollsBackArchiveAndRevision(t *testing.T) {
	pool := pushTestPool(t)
	botID := pushTestBot(t, pool)
	ctx := context.Background()

	created, err := Push(ctx, pool, botID, pushInput("Before failure", ""),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, created.Code)
	if err != nil || before == nil {
		t.Fatalf("load: %v", err)
	}

	withFailTitleConstraint(t, pool)
	if _, err := Push(ctx, pool, botID, pushInput("A1-FAILWRITE", created.Code),
		128, 10, 24, 500, 102400); err == nil {
		t.Fatal("update unexpectedly succeeded")
	}

	after, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, created.Code)
	if err != nil || after == nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Revision != 1 || after.Title != before.Title || after.Content != before.Content {
		t.Fatalf("failed update mutated current row: before=%+v after=%+v", before, after)
	}
	var historyRows int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id=$1`, after.ID).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if historyRows != 0 {
		t.Fatalf("failed update committed %d archived revisions, want 0", historyRows)
	}
}

func TestThreadOriginMemoryIsInternalAndExcludedFromStoreList(t *testing.T) {
	pool := pushTestPool(t)
	botID := pushTestBot(t, pool)
	ctx := context.Background()

	var spendsBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_push'`, botID).Scan(&spendsBefore); err != nil {
		t.Fatal(err)
	}

	k, err := persistThreadMemory(ctx, pool, botID, "", nil, "", "short thread reply")
	if err != nil {
		t.Fatalf("persist Thread Memory: %v", err)
	}
	if k.Origin != "thread" || k.Revision != 1 {
		t.Fatalf("Thread Memory = revision %d origin %q", k.Revision, k.Origin)
	}

	listed, err := ListKungfusForBot(ctx, pool, botID, 50, 0)
	if err != nil {
		t.Fatalf("memory_list projection: %v", err)
	}
	if listed["meta"].(map[string]interface{})["total"].(int64) != 0 {
		t.Fatalf("thread-origin Memory leaked into standalone list: %#v", listed)
	}

	stats, err := repository.KungfuStatsByBotID(ctx, pool, botID)
	if err != nil {
		t.Fatalf("account Memory stats: %v", err)
	}
	if stats.Total != 0 || stats.PublicTotal != 0 {
		t.Fatalf("thread-origin Memory leaked into account Store stats: %+v", stats)
	}

	var spendsAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_push'`, botID).Scan(&spendsAfter); err != nil {
		t.Fatal(err)
	}
	if spendsAfter != spendsBefore {
		t.Fatalf("Thread Memory invoked standalone Store consumption: %d -> %d", spendsBefore, spendsAfter)
	}
	var historyRows int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id=$1`, k.ID).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if historyRows != 0 {
		t.Fatalf("first Thread Memory write duplicated body into history: %d rows", historyRows)
	}
}

func TestTaskHarnessStillReadsCurrentMemoryAfterRevisionUpdate(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	created, err := Push(ctx, pool, publisher, map[string]interface{}{
		"title":   "Harness v1",
		"tags":    []interface{}{"harness"},
		"content": strings.Repeat("h", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create harness Memory: %v", err)
	}

	c := pubContract("")
	c.HarnessRefs = []string{created.Code}
	taskCode := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, taskCode); err != nil {
		t.Fatalf("open task: %v", err)
	}

	first, err := GetHarness(ctx, pool, agent, taskCode, created.Code)
	if err != nil || first["title"] != "Harness v1" || first["content"] != strings.Repeat("h", 60) {
		t.Fatalf("initial harness = %#v err=%v", first, err)
	}

	if _, err := Push(ctx, pool, publisher, map[string]interface{}{
		"code":    created.Code,
		"title":   "Harness v2",
		"tags":    []interface{}{"harness"},
		"content": strings.Repeat("n", 60),
	}, 128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update harness Memory: %v", err)
	}

	second, err := GetHarness(ctx, pool, agent, taskCode, created.Code)
	if err != nil {
		t.Fatalf("current harness: %v", err)
	}
	if second["title"] != "Harness v2" || second["content"] != strings.Repeat("n", 60) {
		t.Fatalf("Task harness did not read current Memory: %#v", second)
	}
}
