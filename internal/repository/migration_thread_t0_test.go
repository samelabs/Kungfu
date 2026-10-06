package repository

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
)

func threadMigrationFilesThrough(t *testing.T, through string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v (%d)", err, len(files))
	}
	sort.Strings(files)
	for i, f := range files {
		if strings.HasPrefix(filepath.Base(f), through+"_") {
			return files[:i+1]
		}
	}
	t.Fatalf("migration %s not found", through)
	return nil
}

func applyMigrationFiles(t *testing.T, db *pg.Pool, files []string) {
	t.Helper()
	ctx := context.Background()
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}

func TestThreadT0FreshMigrationSchema(t *testing.T) {
	pool := migTestPool(t)
	ctx := context.Background()

	for _, table := range []string{
		"memory_revisions",
		"role_links",
		"threads",
		"thread_roles",
		"thread_memories",
		"thread_receipts",
		"thread_idempotency",
	} {
		var reg string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(to_regclass('public.' || $1)::text, '')`, table).Scan(&reg); err != nil {
			t.Fatalf("lookup %s: %v", table, err)
		}
		if reg == "" {
			t.Fatalf("fresh migration chain is missing %s", table)
		}
	}

	// Existing standalone inserts still work before T1 application code
	// learns about the new fields; database defaults are the compatibility
	// contract for T0.
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Rollback(tx) }()

	digest := sha256.Sum256([]byte("thread-t0-fresh"))
	var botID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash)
		VALUES ('thread_t0_fresh', $1, 't0f1', 'x') RETURNING id`, digest[:]).Scan(&botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	var revision int64
	var origin string
	if err := tx.QueryRow(ctx, `
		INSERT INTO tb_kungfus
			(code, bot_id, title, tags_json, content, checksum, visibility, status)
		VALUES ('t0fresh00001', $1, 'fresh memory', '["t0"]', 'fresh memory body',
		        repeat('a', 64), 'private', 'active')
		RETURNING revision, origin`, botID).Scan(&revision, &origin); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if revision != 1 || origin != "standalone" {
		t.Fatalf("new standalone Memory defaults = revision %d origin %q, want 1/standalone", revision, origin)
	}
}

func TestThreadT0UpgradeRehearsal(t *testing.T) {
	migTestPool(t) // presence + skip contract; the rehearsal uses a throwaway DB below.
	ctx := context.Background()

	baseURL := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	cut := strings.LastIndex(baseURL, "/")
	if cut < 0 {
		t.Skip("cannot derive admin DSN")
	}
	adminURL := baseURL[:cut] + "/postgres"
	admin, err := pg.NewPool(adminURL)
	if err != nil {
		t.Skipf("admin connection unavailable: %v", err)
	}
	defer admin.Close()

	dbName := "kf_thread_t0_" + nanoSuffix()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Skipf("create rehearsal database: %v", err)
	}
	defer func() { _, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`) }()

	dbURL := baseURL[:cut] + "/" + dbName
	db, err := pg.NewPool(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	applyMigrationFiles(t, db, threadMigrationFilesThrough(t, "022"))

	digest := sha256.Sum256([]byte("thread-t0-upgrade"))
	var botID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ('thread_t0_upgrade', $1, 't0u1', 'x', 500)
		RETURNING id`, digest[:]).Scan(&botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	const memoryCode = "t0memory0001"
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_kungfus
			(code, bot_id, title, tags_json, description, content, checksum, visibility, status)
		VALUES ($1, $2, 'upgrade memory', '["thread","t0"]', 'before 023',
		        'memory body must survive the Thread schema migration unchanged',
		        repeat('b', 64), 'private', 'active')`, memoryCode, botID); err != nil {
		t.Fatalf("seed Memory: %v", err)
	}

	const taskCode = "threadt0task"
	const contract = `{"title":"T0 task","requirements":"Keep this Task contract byte-for-byte equivalent across Thread migrations.","harness_refs":["t0memory0001"],"receiver":{"url":"https://example.com/receiver"},"price":5,"limits":{"max_rejected_per_agent":5},"claim":{"required":false,"ttl":1800,"max_duration":7200}}`
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_task
			(code, publisher_id, status, budget_locked, settled, reserved, refunded, contract)
		VALUES ($1, $2, 'paused', 100, 10, 15, 20, $3::jsonb)`,
		taskCode, botID, contract); err != nil {
		t.Fatalf("seed Task: %v", err)
	}

	var beforeMemoryCount, beforeTaskCount int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus`).Scan(&beforeMemoryCount); err != nil {
		t.Fatalf("snapshot Memory count: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task`).Scan(&beforeTaskCount); err != nil {
		t.Fatalf("snapshot Task count: %v", err)
	}

	var beforeMemoryID int64
	var beforeTitle, beforeTags, beforeDescription, beforeContent, beforeChecksum, beforeVisibility, beforeStatus string
	if err := db.QueryRow(ctx, `
		SELECT id, title, tags_json::text, description, content, checksum, visibility, status
		FROM tb_kungfus WHERE code = $1`, memoryCode).Scan(
		&beforeMemoryID, &beforeTitle, &beforeTags, &beforeDescription,
		&beforeContent, &beforeChecksum, &beforeVisibility, &beforeStatus); err != nil {
		t.Fatalf("snapshot Memory: %v", err)
	}

	var beforeTaskStatus, beforeTaskContract string
	var beforeLocked, beforeSettled, beforeReserved, beforeRefunded int64
	if err := db.QueryRow(ctx, `
		SELECT status, budget_locked, settled, reserved, refunded, contract::text
		FROM tb_task WHERE code = $1`, taskCode).Scan(
		&beforeTaskStatus, &beforeLocked, &beforeSettled,
		&beforeReserved, &beforeRefunded, &beforeTaskContract); err != nil {
		t.Fatalf("snapshot Task: %v", err)
	}

	all := threadMigrationFilesThrough(t, "025")
	applyMigrationFiles(t, db, all[len(threadMigrationFilesThrough(t, "022")):])

	var afterMemoryCount, afterTaskCount int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus`).Scan(&afterMemoryCount); err != nil {
		t.Fatalf("compare Memory count: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task`).Scan(&afterTaskCount); err != nil {
		t.Fatalf("compare Task count: %v", err)
	}
	if afterMemoryCount != beforeMemoryCount {
		t.Fatalf("Memory count changed across 023-025: %d -> %d", beforeMemoryCount, afterMemoryCount)
	}
	if afterTaskCount != beforeTaskCount {
		t.Fatalf("Task count changed across 023-025: %d -> %d", beforeTaskCount, afterTaskCount)
	}

	var afterMemoryID, revision int64
	var afterTitle, afterTags, afterDescription, afterContent, afterChecksum, afterVisibility, afterStatus, origin string
	if err := db.QueryRow(ctx, `
		SELECT id, title, tags_json::text, description, content, checksum, visibility, status, revision, origin
		FROM tb_kungfus WHERE code = $1`, memoryCode).Scan(
		&afterMemoryID, &afterTitle, &afterTags, &afterDescription,
		&afterContent, &afterChecksum, &afterVisibility, &afterStatus, &revision, &origin); err != nil {
		t.Fatalf("compare Memory: %v", err)
	}
	if afterMemoryID != beforeMemoryID || afterTitle != beforeTitle || afterTags != beforeTags ||
		afterDescription != beforeDescription || afterContent != beforeContent || afterChecksum != beforeChecksum ||
		afterVisibility != beforeVisibility || afterStatus != beforeStatus {
		t.Fatalf("Memory fact changed across 023-025")
	}
	if revision != 1 || origin != "standalone" {
		t.Fatalf("upgraded Memory = revision %d origin %q, want 1/standalone", revision, origin)
	}
	var historyRows int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM memory_revisions WHERE memory_id = $1`, afterMemoryID).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if historyRows != 0 {
		t.Fatalf("023 copied current Memory into history: got %d rows, want 0", historyRows)
	}

	var afterTaskStatus, afterTaskContract string
	var afterLocked, afterSettled, afterReserved, afterRefunded int64
	if err := db.QueryRow(ctx, `
		SELECT status, budget_locked, settled, reserved, refunded, contract::text
		FROM tb_task WHERE code = $1`, taskCode).Scan(
		&afterTaskStatus, &afterLocked, &afterSettled,
		&afterReserved, &afterRefunded, &afterTaskContract); err != nil {
		t.Fatalf("compare Task: %v", err)
	}
	if afterTaskStatus != beforeTaskStatus || afterLocked != beforeLocked || afterSettled != beforeSettled ||
		afterReserved != beforeReserved || afterRefunded != beforeRefunded ||
		afterTaskContract != beforeTaskContract {
		t.Fatalf("Task 1.0 fact changed across 023-025")
	}

	for _, table := range []string{"role_links", "threads", "thread_roles", "thread_memories", "thread_receipts", "thread_idempotency"} {
		var count int64
		if err := db.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("upgrade migration synthesized %d rows in %s", count, table)
		}
	}
}
