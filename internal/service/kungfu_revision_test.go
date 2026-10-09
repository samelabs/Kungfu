package service

// Memory revision (D1) service tests against the local PostgreSQL
// (KF_TEST_DATABASE_URL): the §5/§9 read matrix (author any version,
// non-author current-only), public-version evolution, withdrawal
// behavior, the origin='thread' list exclusion, and L5 concurrent
// updates of one memory.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

func revisionTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	return a7TestPool(t)
}

func revisionPushInput(title, code, content string) map[string]interface{} {
	return map[string]interface{}{
		"code":    code,
		"title":   title,
		"tags":    []interface{}{"t"},
		"content": content,
	}
}

// revisionArchives returns the archived revisions of one memory as a
// map revision → content.
func revisionArchives(t *testing.T, pool *pg.Pool, code string) map[int64]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT r.revision, r.content
		FROM memory_revisions r JOIN tb_kungfus k ON k.id = r.memory_id
		WHERE k.code = $1`, code)
	if err != nil {
		t.Fatalf("archives: %v", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var rev int64
		var content string
		if err := rows.Scan(&rev, &content); err != nil {
			t.Fatal(err)
		}
		out[rev] = content
	}
	return out
}

// TestRevisionAuthorReadsHistory: the author reads every existing
// version (current row and archives); a revision that never existed
// is NOT_FOUND; the revision-less read keeps returning the current
// version, now with the revision field.
func TestRevisionAuthorReadsHistory(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("rev v1", "", strings.Repeat("a", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 2; i <= 3; i++ {
		if _, err := Push(ctx, pool, owner,
			revisionPushInput(fmt.Sprintf("rev v%d", i), created.Code, strings.Repeat("b", 60)+fmt.Sprint(i)),
			128, 10, 24, 500, 102400); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}

	// current read: revision 3, newest content
	detail, err := GetKungfuForBot(ctx, pool, owner, created.Code)
	if err != nil {
		t.Fatalf("current get: %v", err)
	}
	if got, _ := detail["revision"].(int64); got != 3 {
		t.Fatalf("current revision = %v, want 3", detail["revision"])
	}
	if got, _ := detail["content"].(string); got != strings.Repeat("b", 60)+"3" {
		t.Fatalf("current content = %q, want v3", got)
	}

	// every historical version is readable by the author, with the
	// old content
	for rev, want := range map[int64]string{
		1: strings.Repeat("a", 60),
		2: strings.Repeat("b", 60) + "2",
		3: strings.Repeat("b", 60) + "3", // current revision, requested explicitly
	} {
		snap, err := GetKungfuRevisionForBot(ctx, pool, owner, created.Code, rev)
		if err != nil {
			t.Fatalf("revision %d: %v", rev, err)
		}
		if got, _ := snap["content"].(string); got != want {
			t.Fatalf("revision %d content = %q, want %q", rev, got, want)
		}
		if got, _ := snap["revision"].(int64); got != rev {
			t.Fatalf("revision %d reports revision %v", rev, snap["revision"])
		}
	}

	// a revision that never existed is NOT_FOUND
	_, err = GetKungfuRevisionForBot(ctx, pool, owner, created.Code, 4)
	if ae, ok := apperrIs(err); !ok || ae.HTTPCode != 404 || ae.Code != "NOT_FOUND" {
		t.Fatalf("revision 4: want 404 NOT_FOUND, got %v", err)
	}
}

// TestRevisionNonAuthorAccess (negative): a non-author never receives
// a pinned revision — not for a private memory, not for a public one
// (even its current revision); the private current-version read stays
// PRIVATE_KUNGFU exactly as before.
func TestRevisionNonAuthorAccess(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	reader, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("priv v1", "", strings.Repeat("c", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Push(ctx, pool, owner, revisionPushInput("priv v2", created.Code, strings.Repeat("d", 60)),
		128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update: %v", err)
	}

	// non-author on a private memory: current read is 403
	// PRIVATE_KUNGFU (unchanged), any explicit revision is a
	// permission error
	if _, err := GetKungfuForBot(ctx, pool, reader, created.Code); err == nil {
		t.Fatal("non-author private current read must be rejected")
	} else if ae, _ := apperrIs(err); ae == nil || ae.HTTPCode != 403 || ae.Code != "PRIVATE_KUNGFU" {
		t.Fatalf("private current read: want 403 PRIVATE_KUNGFU, got %v", err)
	}
	for _, rev := range []int64{1, 2} {
		_, err := GetKungfuRevisionForBot(ctx, pool, reader, created.Code, rev)
		if ae, ok := apperrIs(err); !ok || ae.HTTPCode != 403 {
			t.Fatalf("private revision %d: want 403, got %v", rev, err)
		}
	}

	// public memory: the current version is readable without revision
	// AND with an explicit revision that equals it (§9 row 1 — the same
	// bytes, not a historical pin); an explicit HISTORICAL revision
	// stays a permission error
	if _, err := Share(ctx, pool, owner, created.Code); err != nil {
		t.Fatalf("share: %v", err)
	}
	if _, err := GetKungfuForBot(ctx, pool, reader, created.Code); err != nil {
		t.Fatalf("public current read: %v", err)
	}
	if _, err := GetKungfuRevisionForBot(ctx, pool, reader, created.Code, 2); err != nil {
		t.Fatalf("public current revision read (D-011): %v", err)
	}
	if _, err := GetKungfuRevisionForBot(ctx, pool, reader, created.Code, 1); err == nil {
		t.Fatal("public historical revision read must be rejected")
	} else if ae, ok := apperrIs(err); !ok || ae.HTTPCode != 403 {
		t.Fatalf("public revision 1: want 403 (permission), got %v", err)
	}
}

// TestRevisionUpdateArchivesExactlyOneNewRow: one update → revision+1
// and exactly ONE new archive holding the old version; a later update
// archives the next version as a new row and does not rewrite the old
// archive (archives are immutable).
func TestRevisionUpdateArchivesExactlyOneNewRow(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("arc v1", "", strings.Repeat("e", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if archives := revisionArchives(t, pool, created.Code); len(archives) != 0 {
		t.Fatalf("creation archived %d rows, want 0", len(archives))
	}

	updated, err := Push(ctx, pool, owner, revisionPushInput("arc v2", created.Code, strings.Repeat("f", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Action != "updated" || updated.Revision != 2 {
		t.Fatalf("update result = %+v, want action updated revision 2", updated)
	}
	archives := revisionArchives(t, pool, created.Code)
	if len(archives) != 1 {
		t.Fatalf("archives after one update = %d, want exactly 1", len(archives))
	}
	if got := archives[1]; got != strings.Repeat("e", 60) {
		t.Fatalf("archive of revision 1 = %q, want the original content", got)
	}

	// second update: one MORE archive; the first is untouched
	if _, err := Push(ctx, pool, owner, revisionPushInput("arc v3", created.Code, strings.Repeat("g", 60)),
		128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	archives = revisionArchives(t, pool, created.Code)
	if len(archives) != 2 {
		t.Fatalf("archives after two updates = %d, want exactly 2", len(archives))
	}
	if got := archives[1]; got != strings.Repeat("e", 60) {
		t.Fatalf("old archive rewritten by the second update: %q", got)
	}
	if got := archives[2]; got != strings.Repeat("f", 60) {
		t.Fatalf("archive of revision 2 = %q, want the intermediate content", got)
	}
}

// TestRevisionPublicEvolution (A25): public visibility belongs to the
// memory — after an update, non-author readers see the NEW current
// version, while the author can still read the old one.
func TestRevisionPublicEvolution(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	reader, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("pub v1", "", strings.Repeat("h", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Share(ctx, pool, owner, created.Code); err != nil {
		t.Fatalf("share: %v", err)
	}

	// reader sees revision 1
	first, err := GetKungfuForBot(ctx, pool, reader, created.Code)
	if err != nil {
		t.Fatalf("reader before update: %v", err)
	}
	if got, _ := first["revision"].(int64); got != 1 || first["content"] != strings.Repeat("h", 60) {
		t.Fatalf("reader before update = rev %v content %.20q", first["revision"], first["content"])
	}

	// author updates (visibility unchanged by the update)
	if _, err := Push(ctx, pool, owner, revisionPushInput("pub v2", created.Code, strings.Repeat("i", 60)),
		128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update: %v", err)
	}

	// reader now sees the NEW current version without doing anything
	second, err := GetKungfuForBot(ctx, pool, reader, created.Code)
	if err != nil {
		t.Fatalf("reader after update: %v", err)
	}
	if got, _ := second["revision"].(int64); got != 2 {
		t.Fatalf("reader after update: revision = %v, want 2", second["revision"])
	}
	if got, _ := second["content"].(string); got != strings.Repeat("i", 60) {
		t.Fatalf("reader after update: content = %.20q, want the new version", got)
	}
	if got, _ := second["visibility"].(string); got != "public" {
		t.Fatalf("visibility changed on update: %v", second["visibility"])
	}

	// the author can still read the old version
	old, err := GetKungfuRevisionForBot(ctx, pool, owner, created.Code, 1)
	if err != nil {
		t.Fatalf("author old revision: %v", err)
	}
	if got, _ := old["content"].(string); got != strings.Repeat("h", 60) {
		t.Fatalf("author old revision content = %.20q", got)
	}
}

// TestRevisionWithdrawal: after Delete the memory leaves memory_list
// and stays unreadable through the revision-less path (current
// behavior), while the author can still read every archived version
// (§5: 作者恒可读自己的任何版本).
func TestRevisionWithdrawal(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	reader, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("del v1", "", strings.Repeat("j", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Push(ctx, pool, owner, revisionPushInput("del v2", created.Code, strings.Repeat("k", 60)),
		128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := Delete(ctx, pool, owner, created.Code); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// memory_list excludes the withdrawn memory
	list, err := ListKungfusForBot(ctx, pool, owner, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, raw := range list["kungfus"].([]map[string]interface{}) {
		if raw["code"] == created.Code {
			t.Fatalf("withdrawn memory still listed: %v", raw)
		}
	}
	if total := list["meta"].(map[string]interface{})["total"]; total != int64(0) {
		t.Fatalf("list total = %v, want 0", total)
	}

	// current-version read: 404 for author AND reader — exactly the
	// pre-revisioning behavior
	for _, who := range []int64{owner, reader} {
		_, err := GetKungfuForBot(ctx, pool, who, created.Code)
		if ae, ok := apperrIs(err); !ok || ae.HTTPCode != 404 || ae.Code != "NOT_FOUND" {
			t.Fatalf("current read after withdrawal (bot %d): want 404 NOT_FOUND, got %v", who, err)
		}
	}

	// the author still reads every version of the withdrawn memory
	for rev, want := range map[int64]string{
		1: strings.Repeat("j", 60),
		2: strings.Repeat("k", 60),
	} {
		snap, err := GetKungfuRevisionForBot(ctx, pool, owner, created.Code, rev)
		if err != nil {
			t.Fatalf("author revision %d after withdrawal: %v", rev, err)
		}
		if got, _ := snap["content"].(string); got != want {
			t.Fatalf("author revision %d content = %.20q", rev, got)
		}
	}

	// a non-author gets nothing — not even existence
	if _, err := GetKungfuRevisionForBot(ctx, pool, reader, created.Code, 1); err == nil {
		t.Fatal("non-author revision read after withdrawal must be rejected")
	} else if ae, _ := apperrIs(err); ae == nil || ae.HTTPCode != 404 {
		t.Fatalf("non-author revision read after withdrawal: want 404, got %v", err)
	}
}

// TestRevisionListFieldsAndThreadOriginExclusion: list items carry
// revision and origin, and origin='thread' rows (no writer exists
// yet — seeded directly) are excluded from the default listing and
// its total.
func TestRevisionListFieldsAndThreadOriginExclusion(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("lst v1", "", strings.Repeat("l", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Push(ctx, pool, owner, revisionPushInput("lst v2", created.Code, strings.Repeat("m", 60)),
		128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("update: %v", err)
	}

	// a thread-origin row for the same bot, seeded directly (D1 has
	// no thread writer)
	threadCode := fmt.Sprintf("cccc%08d", time.Now().UnixNano()%100000000)
	if _, err := pool.Exec(ctx, `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status, origin)
		VALUES ($1, $2, 'thread entry', '["t"]', $3, 'x', 'private', 'active', 'thread')`,
		threadCode, owner, strings.Repeat("n", 60)); err != nil {
		t.Fatalf("seed thread row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM tb_kungfus WHERE code = $1`, threadCode)
	})

	list, err := ListKungfusForBot(ctx, pool, owner, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items := list["kungfus"].([]map[string]interface{})
	if len(items) != 1 {
		t.Fatalf("listed %d items, want exactly the standalone memory", len(items))
	}
	item := items[0]
	if item["code"] != created.Code {
		t.Fatalf("listed %v, want the standalone memory", item["code"])
	}
	if got, _ := item["revision"].(int64); got != 2 {
		t.Fatalf("list revision = %v, want 2", item["revision"])
	}
	if got, _ := item["origin"].(string); got != "standalone" {
		t.Fatalf("list origin = %v, want standalone", item["origin"])
	}
	if total := list["meta"].(map[string]interface{})["total"]; total != int64(1) {
		t.Fatalf("list total = %v, want 1 (thread rows excluded)", total)
	}
}

// TestRevisionConcurrentPuts (L5): two concurrent updates of one
// memory serialize on the row lock — each produces exactly one new
// version: revision advances 1 → 2 → 3 with no gap, both old versions
// are archived exactly once, nothing is lost.
func TestRevisionConcurrentPuts(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	created, err := Push(ctx, pool, owner, revisionPushInput("con v1", "", strings.Repeat("o", 60)),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// two genuinely concurrent updates (start barrier so both hit the
	// row before either commits)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	payloads := []string{strings.Repeat("p", 60), strings.Repeat("q", 60)}
	for _, content := range payloads {
		wg.Add(1)
		go func(content string) {
			defer wg.Done()
			<-start
			_, err := Push(ctx, pool, owner, revisionPushInput("con v2", created.Code, content),
				128, 10, 24, 500, 102400)
			errs <- err
		}(content)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent put failed: %v", err)
		}
	}

	// final row: revision 3, content is one of the two payloads
	var revision int64
	var current string
	if err := pool.QueryRow(ctx,
		`SELECT revision, content FROM tb_kungfus WHERE code = $1`, created.Code).
		Scan(&revision, &current); err != nil {
		t.Fatal(err)
	}
	if revision != 3 {
		t.Fatalf("revision after two concurrent puts = %d, want 3 (one new version per put, no double bump)", revision)
	}

	// archives: exactly revisions 1 and 2 — the original plus the
	// first-committed update; no gap, none lost, none rewritten
	archives := revisionArchives(t, pool, created.Code)
	if len(archives) != 2 {
		t.Fatalf("archives = %v, want exactly revisions {1,2}", archives)
	}
	if got := archives[1]; got != strings.Repeat("o", 60) {
		t.Fatalf("archive 1 = %.20q, want the original content", got)
	}
	mid := archives[2]
	if mid != payloads[0] && mid != payloads[1] {
		t.Fatalf("archive 2 = %.20q, want one of the two payloads", mid)
	}
	if mid == current {
		t.Fatal("archive 2 equals the final content — one update's version was lost")
	}
}
