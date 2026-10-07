package repository

// Memory revision (D1) repository tests against a real PostgreSQL
// (KF_TEST_DATABASE_URL, migrations applied): the versioned update
// sequence (lock → archive → bump), archive immutability by
// construction, and the 023 migration on both a fresh chain and a
// backfilled pre-023 database.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
)

// revisionPool reuses the migration-test skip contract.
func revisionPool(t *testing.T) *pg.Pool {
	t.Helper()
	return migTestPool(t)
}

// revisionChecksum mirrors the production checksum: sha256 hex (64
// chars, fits CHAR(64) without padding).
func revisionChecksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

type seededKungfu struct {
	ID   int64
	Code string
}

func revisionSeedKungfu(t *testing.T, pool *pg.Pool, botID int64, title, content string) *seededKungfu {
	t.Helper()
	ctx := context.Background()
	code, err := GenerateUniqueKungfuCode(ctx, pool)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if err := InsertNewKungfu(ctx, pool, code, botID, title,
		`["t"]`, "", content, revisionChecksum(content)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	k, err := FindOwnedActiveKungfuByCode(ctx, pool, botID, code)
	if err != nil || k == nil {
		t.Fatalf("reload seeded kungfu: %v %v", k, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM memory_revisions WHERE memory_id = $1`, k.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_kungfus WHERE id = $1`, k.ID)
	})
	return &seededKungfu{ID: k.ID, Code: k.Code}
}

// revisionUpdate runs the sanctioned versioned update: one
// transaction — row lock, archive the old version, content update +
// revision bump — returning the new revision.
func revisionUpdate(t *testing.T, pool *pg.Pool, botID, id int64, title, content string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()

	locked, err := LockOwnedActiveKungfuByID(ctx, tx, id, botID)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if locked == nil {
		t.Fatal("lock: row not found (must be active and owned)")
	}
	if err := ArchiveKungfuRevision(ctx, tx, id); err != nil {
		t.Fatalf("archive: %v", err)
	}
	newRevision, err := UpdateKungfuContentWithRevision(ctx, tx, id,
		title, `["t"]`, "", content, revisionChecksum(content))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return newRevision
}

// TestRevisionCreateIsFirstRevision: a fresh row is revision 1,
// origin standalone, and creation writes no history row.
func TestRevisionCreateIsFirstRevision(t *testing.T) {
	pool := revisionPool(t)
	bot := taskV1SeedBot(t, pool, 0)
	k := revisionSeedKungfu(t, pool, bot, "first", "original-content")

	ctx := context.Background()
	var revision int64
	var origin string
	if err := pool.QueryRow(ctx,
		`SELECT revision, origin FROM tb_kungfus WHERE id = $1`, k.ID).
		Scan(&revision, &origin); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if revision != 1 || origin != "standalone" {
		t.Fatalf("fresh row = revision %d origin %s, want 1/standalone", revision, origin)
	}
	var archived int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = $1`, k.ID).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatalf("creation archived %d rows, want 0", archived)
	}
}

// TestRevisionUpdateArchivesOnceAndBumps: one update archives
// exactly one snapshot of the OLD version and bumps the row to
// revision 2; a second update archives the intermediate version as a
// NEW row and leaves the first archive untouched (archives are
// immutable — the (memory_id, revision) key rejects any rewrite).
func TestRevisionUpdateArchivesOnceAndBumps(t *testing.T) {
	pool := revisionPool(t)
	bot := taskV1SeedBot(t, pool, 0)
	k := revisionSeedKungfu(t, pool, bot, "v1", "content-one")
	ctx := context.Background()

	if got := revisionUpdate(t, pool, bot, k.ID, "v2", "content-two"); got != 2 {
		t.Fatalf("first update revision = %d, want 2", got)
	}

	// exactly one archive: revision 1 holding the original content
	rev1, err := FindKungfuRevision(ctx, pool, k.ID, 1)
	if err != nil {
		t.Fatalf("find revision 1: %v", err)
	}
	if rev1 == nil {
		t.Fatal("revision 1 not archived")
	}
	if rev1.Content != "content-one" || rev1.Title != "v1" || rev1.Checksum != revisionChecksum("content-one") {
		t.Fatalf("archive of revision 1 = %+v, want the original version", rev1)
	}
	if rev2, err := FindKungfuRevision(ctx, pool, k.ID, 2); err != nil || rev2 != nil {
		t.Fatalf("revision 2 must not be archived after ONE update: %v %v", rev2, err)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = $1`, k.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("archives after first update = %d, want exactly 1", n)
	}

	// second update: revision 3, a NEW archive for revision 2
	if got := revisionUpdate(t, pool, bot, k.ID, "v3", "content-three"); got != 3 {
		t.Fatalf("second update revision = %d, want 3", got)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = $1`, k.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("archives after second update = %d, want exactly 2", n)
	}
	rev1After, err := FindKungfuRevision(ctx, pool, k.ID, 1)
	if err != nil {
		t.Fatalf("reload revision 1: %v", err)
	}
	if rev1After.Content != "content-one" || rev1After.Title != "v1" {
		t.Fatalf("old archive rewritten by the second update: %+v", rev1After)
	}
	rev2, err := FindKungfuRevision(ctx, pool, k.ID, 2)
	if err != nil || rev2 == nil {
		t.Fatalf("revision 2 not archived: %v %v", rev2, err)
	}
	if rev2.Content != "content-two" || rev2.Title != "v2" {
		t.Fatalf("archive of revision 2 = %+v, want the intermediate version", rev2)
	}
}

// TestRevisionArchiveKeyRejectsDuplicate: archiving the same
// (memory, revision) twice violates the primary key — the database,
// not just the service flow, makes every version archive at most
// once.
func TestRevisionArchiveKeyRejectsDuplicate(t *testing.T) {
	pool := revisionPool(t)
	bot := taskV1SeedBot(t, pool, 0)
	k := revisionSeedKungfu(t, pool, bot, "dup", "content-dup")
	ctx := context.Background()

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	if _, err := LockOwnedActiveKungfuByID(ctx, tx, k.ID, bot); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := ArchiveKungfuRevision(ctx, tx, k.ID); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if err := ArchiveKungfuRevision(ctx, tx, k.ID); err == nil {
		t.Fatal("second archive of the same revision unexpectedly succeeded")
	} else if !IsUniqueViolation(err) {
		t.Fatalf("second archive: want unique violation, got %v", err)
	}
}

// TestRevisionLockExcludesWithdrawnRow: the update lock only finds
// ACTIVE rows — a withdrawn memory cannot be locked, so it cannot be
// updated or archived again.
func TestRevisionLockExcludesWithdrawnRow(t *testing.T) {
	pool := revisionPool(t)
	bot := taskV1SeedBot(t, pool, 0)
	k := revisionSeedKungfu(t, pool, bot, "gone", "content-gone")
	ctx := context.Background()

	if err := SoftDeleteKungfuByID(ctx, pool, k.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	locked, err := LockOwnedActiveKungfuByID(ctx, tx, k.ID, bot)
	if err != nil {
		t.Fatalf("lock on withdrawn row errored: %v", err)
	}
	if locked != nil {
		t.Fatal("withdrawn memory must not be lockable for update")
	}
}

// ---- migration 023: fresh chain + backfill ----

// migrationFilesUpTo returns the chain strictly before the named
// migration and that migration's own file, from the repo working
// tree.
func migrationFilesUpTo(t *testing.T, lastName string) ([]string, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v (%d)", err, len(files))
	}
	sort.Strings(files)
	for i, f := range files {
		if strings.Contains(f, lastName) {
			return files[:i], f
		}
	}
	t.Fatalf("migration %s not found", lastName)
	return nil, ""
}

func applyMigrations(t *testing.T, db *pg.Pool, files []string) {
	t.Helper()
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(context.Background(), string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// TestMigration023FreshChainShape: on the fully migrated live schema
// tb_kungfus carries revision (BIGINT, NOT NULL, default 1) and
// origin (TEXT, NOT NULL, default standalone), and memory_revisions
// exists with the unique (memory_id, revision) key.
func TestMigration023FreshChainShape(t *testing.T) {
	pool := revisionPool(t)
	ctx := context.Background()

	for _, tc := range [][3]string{
		{"tb_kungfus", "revision", "bigint"},
		{"tb_kungfus", "origin", "text"},
	} {
		var dataType, isNullable string
		var columnDefault *string
		if err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable, column_default FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1 AND column_name = $2`, tc[0], tc[1]).
			Scan(&dataType, &isNullable, &columnDefault); err != nil {
			t.Fatalf("%s.%s: %v", tc[0], tc[1], err)
		}
		if dataType != tc[2] || isNullable != "NO" || columnDefault == nil {
			t.Fatalf("%s.%s is %s nullable=%s default=%v, want %s NOT NULL with default", tc[0], tc[1], dataType, isNullable, columnDefault, tc[2])
		}
	}

	// the unique pair on memory_revisions is the primary key
	rows, err := pool.Query(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'memory_revisions'::regclass AND i.indisprimary`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	sort.Strings(cols)
	if len(cols) != 2 || cols[0] != "memory_id" || cols[1] != "revision" {
		t.Fatalf("memory_revisions unique key = %v, want [memory_id revision]", cols)
	}
}

// TestMigration023BackfillsExistingRows: on a throwaway database with
// the pre-023 chain and OLD-shape kungfu rows (no revision column),
// applying 023 backfills every existing row with revision=1 /
// origin='standalone' and copies nothing into memory_revisions.
func TestMigration023BackfillsExistingRows(t *testing.T) {
	migTestPool(t) // presence + skip contract only; throwaway DB below
	ctx := context.Background()

	dbName := "kf_mig023_" + nanoSuffix()
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
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
	})
	db, err := pg.NewPool(adminURL[:strings.LastIndex(adminURL, "/")] + "/" + dbName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	pre, m023 := migrationFilesUpTo(t, "023_")
	applyMigrations(t, db, pre)

	// seed OLD-shape data: a bot and kungfus that predate revisioning
	migDigest := sha256.Sum256([]byte("mig023"))
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ('mig023', $1, 'x023', 'x', 0)`, migDigest[:]); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, description, content, checksum, visibility, status)
		VALUES ('aaaa00000223', (SELECT id FROM tb_bots WHERE bot_name='mig023'),
		        'pre-023', '["t"]', NULL, 'legacy body', 'legacy-checksum', 'private', 'active'),
		       ('aaaa00000224', (SELECT id FROM tb_bots WHERE bot_name='mig023'),
		        'pre-023 deleted', '["t"]', NULL, 'legacy body 2', 'legacy-checksum-2', 'public', 'deleted')`); err != nil {
		t.Fatalf("seed kungfus: %v", err)
	}

	// sanity: the pre-023 schema has no revision column
	var hasRevision int
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'tb_kungfus' AND column_name = 'revision'`).
		Scan(&hasRevision); err != nil {
		t.Fatal(err)
	}
	if hasRevision != 0 {
		t.Fatal("pre-023 chain already has tb_kungfus.revision — chain slice wrong")
	}

	applyMigrations(t, db, []string{m023})

	// every existing row (active and deleted alike) is backfilled
	rows, err := db.Query(ctx, `SELECT revision, origin FROM tb_kungfus ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var revision int64
		var origin string
		if err := rows.Scan(&revision, &origin); err != nil {
			t.Fatal(err)
		}
		if revision != 1 || origin != "standalone" {
			t.Fatalf("backfilled row = revision %d origin %s, want 1/standalone", revision, origin)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("backfilled rows = %d, want 2", n)
	}

	// existing content is NOT copied into the history table
	var archived int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM memory_revisions`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatalf("memory_revisions rows after backfill = %d, want 0 (no content copied)", archived)
	}
}
