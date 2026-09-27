package repository

// Migration chain regression tests (integer-credits round):
//   1. the fresh chain (001→009) leaves every Credits column BIGINT;
//   2. no Credit-valued column anywhere is NUMERIC.
// The 009 upgrade-path behavior (integral conversion succeeds with
// signs preserved; fractional historical data aborts without mutation)
// is additionally verified end-to-end against throwaway databases in
// this test — the migration files are applied from the repo working
// tree, so CI exercises the exact shipped SQL.
//
// Real PostgreSQL required (KF_TEST_DATABASE_URL); skipped otherwise.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

func migrationFiles(t *testing.T) []string {
	t.Helper()
	// tests run with the package dir as CWD: ../../migrations
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v (%d)", err, len(files))
	}
	sort.Strings(files)
	// These are 009-mechanism tests: the chain under test ends at 009
	// (the last file may no longer be 009 as later migrations land).
	for i, f := range files {
		if strings.Contains(f, "009_") {
			return files[:i+1]
		}
	}
	t.Fatalf("009 migration not found")
	return nil
}

func migTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// creditColumns returns the (table, column) pairs for every
// Credit-valued column of the live model. (The v1 tb_tasks columns
// are gone with migration 015; the Task 1.0 tables are created
// BIGINT natively.)
var creditColumns = [][2]string{
	{"tb_bots", "balance"},
	{"tb_transactions", "amount"},
	{"tb_transactions", "balance_after"},
	{"tb_payments", "credits"},
	{"tb_rewards_products", "credits_price"},
	{"tb_redemptions", "credits_cost"},
}

// TestFreshChainCreatesBigintCreditColumns: after the full chain
// every Credits column is BIGINT — never NUMERIC.
func TestFreshChainCreatesBigintCreditColumns(t *testing.T) {
	pool := migTestPool(t)

	// The CI harness (and this repo's test setup) already applied the
	// full chain; verify against the live schema. A fresh chain that
	// missed 009 or left NUMERIC anywhere fails here.
	ctx := context.Background()
	for _, tc := range creditColumns {
		var dataType string
		if err := pool.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1 AND column_name = $2`, tc[0], tc[1]).Scan(&dataType); err != nil {
			t.Fatalf("%s.%s: %v", tc[0], tc[1], err)
		}
		if dataType != "bigint" {
			t.Fatalf("%s.%s is %s, want bigint", tc[0], tc[1], dataType)
		}
	}
}

// TestNoNumericCreditColumnRemains: repository-wide sweep — no column on
// any Credits table is left NUMERIC for the credit-valued names.
func TestNoNumericCreditColumnRemains(t *testing.T) {
	pool := migTestPool(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name LIKE 'tb_%'
		  AND data_type = 'numeric'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col, dt string
		_ = rows.Scan(&tbl, &col, &dt)
		for _, tc := range creditColumns {
			if tc[0] == tbl && tc[1] == col {
				t.Fatalf("%s.%s still numeric", tbl, col)
			}
		}
	}
}

// TestMigration009UpgradeOnNumericSchema: builds a throwaway database,
// applies 001–003 in their PRE-integer shape is impossible from the
// working tree (already BIGINT); instead this test recreates the
// upgrade condition by casting columns back to NUMERIC(20,4) with
// integral + negative data, then applies 009 and asserts:
//   - integral values (66.0000) become exactly 66,
//   - signs are preserved (-40 stays -40),
//   - all rows survive.
//
// A second phase re-corrupts one value to 66.2500 and asserts 009
// ABORTS with the fractional-data error and leaves schema + data
// untouched (no rounding, no truncation).
func TestMigration009UpgradeOnNumericSchema(t *testing.T) {
	migTestPool(t) // presence + skip contract only; throwaway DB below
	ctx := context.Background()

	dbName := "kf_mig009_" + nanoSuffix()

	var adminURL string
	// derive an admin-capable URL: same DSN, connect to the server's
	// maintenance path by swapping the database name.
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

	dbURL := adminURL[:strings.LastIndex(adminURL, "/")] + "/" + dbName
	db, err := pg.NewPool(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	// Apply the full current chain first (creates BIGINT schema), then
	// simulate the deployed NUMERIC(20,4) shape on the affected columns.
	for _, f := range migrationFiles(t) {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	for _, stmt := range []string{
		`ALTER TABLE tb_bots ALTER COLUMN balance TYPE numeric(20,4)`,
		`ALTER TABLE tb_transactions ALTER COLUMN amount TYPE numeric(20,4), ALTER COLUMN balance_after TYPE numeric(20,4)`,
		`ALTER TABLE tb_payments ALTER COLUMN credits TYPE numeric(20,4)`,
		`ALTER TABLE tb_store_products ALTER COLUMN credits_price TYPE numeric(20,4)`,
		`ALTER TABLE tb_redemptions ALTER COLUMN credits_cost TYPE numeric(20,4)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("backcast: %v (%s)", err, stmt)
		}
	}

	// Seed integral historical facts with signs (upgrade_db fixture shape).
	d1 := sha256.Sum256([]byte("m1"))
	d2 := sha256.Sum256([]byte("m2"))
	seed := `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('m1','x',$1,'ab12',66.0000),
		       ('m2','x',$2,'cd34',-40.0000);
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after)
		SELECT id,'grant_signup',66.0000,66.0000 FROM tb_bots WHERE bot_name='m1';
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after)
		SELECT id,'reverse_payment',-40.0000,-40.0000 FROM tb_bots WHERE bot_name='m2';`
	for i, stmt := range strings.Split(seed, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		// only the first statement carries the $1/$2 digest parameters
		if i == 0 {
			if _, err := db.Exec(ctx, stmt, d1[:], d2[:]); err != nil {
				t.Fatalf("seed stmt: %v (%.80s)", err, stmt)
			}
			continue
		}
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed stmt: %v (%.80s)", err, stmt)
		}
	}

	// Apply 009 (last file) — integral data must convert.
	files := migrationFiles(t)
	m009 := files[len(files)-1]
	sql009, _ := os.ReadFile(m009)
	if !strings.Contains(string(sql009), "fractional Credits values exist") {
		t.Fatalf("%s is not the 009 migration", m009)
	}
	if _, err := db.Exec(ctx, string(sql009)); err != nil {
		t.Fatalf("009 on integral numeric data must succeed: %v", err)
	}

	var bal1, bal2 int64
	if err := db.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE bot_name='m1'`).Scan(&bal1); err != nil || bal1 != 66 {
		t.Fatalf("66.0000 -> %d (err %v), want exactly 66", bal1, err)
	}
	if err := db.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE bot_name='m2'`).Scan(&bal2); err != nil || bal2 != -40 {
		t.Fatalf("-40.0000 -> %d (err %v), sign must be preserved", bal2, err)
	}
	var amount, balanceAfter int64
	if err := db.QueryRow(ctx,
		`SELECT amount, balance_after FROM tb_transactions WHERE type='reverse_payment'`).Scan(&amount, &balanceAfter); err != nil || amount != -40 || balanceAfter != -40 {
		t.Fatalf("negative ledger fact: %d/%d (err %v)", amount, balanceAfter, err)
	}

	// Phase 2: fractional historical data aborts, nothing mutated.
	if _, err := db.Exec(ctx, `ALTER TABLE tb_bots ALTER COLUMN balance TYPE numeric(20,4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE tb_bots SET balance = 66.2500 WHERE bot_name='m1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(sql009)); err == nil {
		t.Fatal("009 must abort on fractional historical data (66.2500)")
	} else if !strings.Contains(err.Error(), "fractional Credits values exist") {
		t.Fatalf("009 abort reason = %v, want the fractional-data error", err)
	}
	var stillNumeric string
	_ = db.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='tb_bots' AND column_name='balance'`).Scan(&stillNumeric)
	if stillNumeric != "numeric" {
		t.Fatalf("aborted 009 must leave schema untouched, balance is %s", stillNumeric)
	}
	var raw string
	_ = db.QueryRow(ctx, `SELECT balance::text FROM tb_bots WHERE bot_name='m1'`).Scan(&raw)
	if raw != "66.2500" {
		t.Fatalf("fractional fact mutated to %s — never round, never truncate", raw)
	}
}

// TestMigration009ExplicitRollbackOnLaterFailure proves the shipped 009
// SQL owns its transaction: a deterministic failure injected at the
// FINAL ALTER statement (tb_redemptions.credits_cost) — after earlier
// 009 ALTERs (tb_bots.balance, tb_transactions, tb_payments,
// tb_store_products — renamed tb_rewards_products by 018) have already
// executed successfully inside the same
// transaction — rolls back the ENTIRE migration: all Credits columns
// stay NUMERIC(20,4), seeded data unchanged. (The v1 tb_tasks columns
// no longer exist after 015 and are no longer part of the simulation.) This exercises explicit
// transaction rollback, NOT the fractional preflight (which fails
// before any ALTER). The event trigger inspects
// pg_event_trigger_ddl_commands() and raises ONLY when the altered
// object is tb_redemptions; earlier ALTER TABLE commands in the same
// 009 run complete normally.
func TestMigration009ExplicitRollbackOnLaterFailure(t *testing.T) {
	migTestPool(t)
	ctx := context.Background()

	dbName := "kf_mig009rb_" + nanoSuffix()
	var adminURL string
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
	dbURL := adminURL[:strings.LastIndex(adminURL, "/")] + "/" + dbName
	db, err := pg.NewPool(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	for _, f := range migrationFiles(t) {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE tb_bots ALTER COLUMN balance TYPE numeric(20,4)`,
		`ALTER TABLE tb_transactions ALTER COLUMN amount TYPE numeric(20,4), ALTER COLUMN balance_after TYPE numeric(20,4)`,
		`ALTER TABLE tb_payments ALTER COLUMN credits TYPE numeric(20,4)`,
		`ALTER TABLE tb_store_products ALTER COLUMN credits_price TYPE numeric(20,4)`,
		`ALTER TABLE tb_redemptions ALTER COLUMN credits_cost TYPE numeric(20,4)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("backcast: %v (%s)", err, stmt)
		}
	}

	d1 := sha256.Sum256([]byte("rb1"))
	d2 := sha256.Sum256([]byte("rb2"))
	seed := `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('rb1','x',$1,'ab12',100.0000),
		       ('rb2','x',$2,'cd34',250.0000);`
	if _, err := db.Exec(ctx, seed, d1[:], d2[:]); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Deterministic failure at the FINAL ALTER in shipped 009: the
	// event-trigger function raises ONLY when the DDL command's target
	// object is tb_redemptions (the last affected table in 009). The
	// earlier ALTER TABLE commands in 009 (tb_bots,
	// tb_transactions, tb_payments, tb_rewards_products) run to completion
	// inside the open transaction BEFORE the failure fires.
	if _, err := db.Exec(ctx, `
		CREATE OR REPLACE FUNCTION kf_rb_boom() RETURNS event_trigger AS $fn$
		DECLARE cmd record;
		BEGIN
			FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
				IF cmd.command_tag = 'ALTER TABLE'
				   AND cmd.object_type = 'table'
				   AND cmd.object_identity = 'public.tb_redemptions' THEN
					RAISE EXCEPTION '009-rollback-proof: injected failure at final ALTER (tb_redemptions), after earlier 009 ALTERs already executed';
				END IF;
			END LOOP;
		END
		$fn$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("boom fn: %v", err)
	}
	if _, err := db.Exec(ctx, `
		CREATE EVENT TRIGGER kf_rb_009_guard ON ddl_command_end
		WHEN tag IN ('ALTER TABLE')
		EXECUTE FUNCTION kf_rb_boom()`); err != nil {
		// Event triggers require superuser; environments running the
		// suite as a plain role cannot exercise the rollback proof.
		if strings.Contains(err.Error(), "permission denied to create event trigger") {
			t.Skipf("event trigger requires superuser: %v", err)
		}
		t.Fatalf("event trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(ctx, `DROP EVENT TRIGGER IF EXISTS kf_rb_009_guard`)
	})

	files := migrationFiles(t)
	m009 := files[len(files)-1]
	sql009, _ := os.ReadFile(m009)
	if !strings.Contains(string(sql009), "BEGIN;") || !strings.Contains(string(sql009), "COMMIT;") {
		t.Fatalf("%s must own its transaction (BEGIN/COMMIT)", m009)
	}
	if _, err := db.Exec(ctx, string(sql009)); err == nil {
		t.Fatal("009 must fail when a later ALTER fails — rollback proof needs the failure")
	}

	// EVERY affected column must remain NUMERIC — the earlier successful
	// ALTERs (tb_bots.balance et al.) must have been rolled back with
	// the transaction.
	for _, col := range [][2]string{
		{"tb_bots", "balance"},
		{"tb_transactions", "amount"}, {"tb_transactions", "balance_after"},
		{"tb_payments", "credits"},
		{"tb_rewards_products", "credits_price"},
		{"tb_redemptions", "credits_cost"},
	} {
		var dt string
		if err := db.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`,
			col[0], col[1]).Scan(&dt); err != nil {
			t.Fatalf("%s.%s: %v", col[0], col[1], err)
		}
		if dt != "numeric" {
			t.Fatalf("partial application: %s.%s is %s, must remain numeric after failed 009", col[0], col[1], dt)
		}
	}

	// Data unchanged.
	var b1, b2 string
	if err := db.QueryRow(ctx, `SELECT balance::text FROM tb_bots WHERE bot_name='rb1'`).Scan(&b1); err != nil || b1 != "100.0000" {
		t.Fatalf("data mutated on rollback: %q (err %v)", b1, err)
	}
	if err := db.QueryRow(ctx, `SELECT balance::text FROM tb_bots WHERE bot_name='rb2'`).Scan(&b2); err != nil || b2 != "250.0000" {
		t.Fatalf("data mutated on rollback: %q (err %v)", b2, err)
	}
}

func nanoSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
