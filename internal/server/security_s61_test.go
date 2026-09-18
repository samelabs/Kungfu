package server

// S6.1: HTTP-surface + migration + UI-contract proofs.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/service"
)

// TestS61MigrationBackfillsExistingKeysAndDropsPlaintext: private
// throwaway database — apply 001→007, insert a legacy plaintext agent
// key, apply 008, and prove the backfill/drop contract.
func TestS61MigrationBackfillsExistingKeysAndDropsPlaintext(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if base == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	admin, err := pg.NewPool(base)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	dbName := fmt.Sprintf("kf_s61_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		admin.Close()
	})

	dsn := strings.Replace(base, "/kungfu_md", "/"+dbName, 1)
	if dsn == base { // fallback: assume .../<db> tail
		t.Fatalf("cannot derive private DSN from %s", base)
	}
	db, err := pg.NewPool(dsn)
	if err != nil {
		t.Fatalf("private pool: %v", err)
	}
	t.Cleanup(db.Close)

	migrations, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(migrations) < 8 {
		t.Fatalf("migrations: %v (%d)", err, len(migrations))
	}
	sort.Strings(migrations)
	for _, m := range migrations {
		sqlBytes, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	// legacy row with a valid plaintext key, inserted under the 008
	// schema it does NOT exist yet — so seed BEFORE 008. Redo: create
	// a fresh db applying 001..007 first, seed, then apply 008.
	// (this test applied everything above; instead verify against a
	// second private db below)
	t.Run("apply008Last", func(t *testing.T) {
		db2Name := dbName + "_b"
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+db2Name); err != nil {
			t.Fatalf("create db2: %v", err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+db2Name+` WITH (FORCE)`)
		})
		dsn2 := strings.Replace(base, "/kungfu_md", "/"+db2Name, 1)
		db2, err := pg.NewPool(dsn2)
		if err != nil {
			t.Fatalf("db2: %v", err)
		}
		defer db2.Close()

		// 001..007
		for _, m := range migrations {
			if strings.HasSuffix(m, "008_agent_key_hash.sql") {
				continue
			}
			sqlBytes, _ := os.ReadFile(m)
			if _, err := db2.Exec(ctx, string(sqlBytes)); err != nil {
				t.Fatalf("pre-008 apply %s: %v", m, err)
			}
		}
		legacyKey := auth.GenerateKey()
		var botID int64
		if err := db2.QueryRow(ctx, `
			INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
			VALUES ($1, $2, 'x', 'active', 5) RETURNING id`,
			"s61legacy"+fmt.Sprint(time.Now().UnixNano()), legacyKey).Scan(&botID); err != nil {
			t.Fatalf("seed legacy bot: %v", err)
		}

		// apply 008
		sqlBytes, err := os.ReadFile("../../migrations/008_agent_key_hash.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db2.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply 008: %v", err)
		}

		// 1) legacy key hashes to the stored digest
		var hashBytes []byte
		if err := db2.QueryRow(ctx, `SELECT api_key_hash FROM tb_bots WHERE id=$1`, botID).Scan(&hashBytes); err != nil {
			t.Fatal(err)
		}
		if string(hashBytes) != string(auth.HashAgentKey(legacyKey)) {
			t.Fatal("backfill digest != SHA-256(legacy plaintext)")
		}
		// 2) last4 preserved
		var last4 string
		_ = db2.QueryRow(ctx, `SELECT api_key_last4 FROM tb_bots WHERE id=$1`, botID).Scan(&last4)
		if last4 != legacyKey[len(legacyKey)-4:] {
			t.Fatalf("last4 = %q", last4)
		}
		// 3) plaintext column gone
		var colCount int
		_ = db2.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='tb_bots' AND column_name='api_key'`).Scan(&colCount)
		if colCount != 0 {
			t.Fatal("plaintext column still exists after 008")
		}
		// 4) uk_api_key_hash exists; uk_api_key gone
		var ukHash, ukOld int
		_ = db2.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conname='uk_api_key_hash'`).Scan(&ukHash)
		_ = db2.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conname='uk_api_key'`).Scan(&ukOld)
		if ukHash != 1 || ukOld != 0 {
			t.Fatalf("constraints: uk_api_key_hash=%d uk_api_key=%d", ukHash, ukOld)
		}
		// 5) hash is 32 bytes
		if len(hashBytes) != 32 {
			t.Fatalf("hash len = %d", len(hashBytes))
		}
		// 6) legacy key still authenticates under the new mechanism
		srv := &Server{
			Config:      testConfig(),
			Pool:        db2,
			RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
		}
		router := srv.buildRouter()
		req := httptest.NewRequest("GET", "/api/ping", nil)
		req.Header.Set("X-Bot-Key", legacyKey)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("legacy key rejected after migration: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestS61OwnerKeyHTTPMaskedOnly: authenticated owner GET /api/key
// returns the metadata contract and no recoverable credential.
func TestS61OwnerKeyHTTPMaskedOnly(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "s61http_" + suffix
	reg, err := service.Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	router := s.buildRouter()

	// owner login
	login := httptest.NewRequest("POST", "/api/owner/session", strings.NewReader(
		fmt.Sprintf(`{"name":%q,"password":"password123"}`, name)))
	login.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, login)
	if loginRec.Code != 200 {
		t.Fatalf("login: %d %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := parseSetCookie(t, loginRec.Header().Get("Set-Cookie"))

	req := httptest.NewRequest("GET", "/api/key", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, reg.Key) {
		t.Fatal("raw key returned by /api/key")
	}
	if strings.Contains(body, "api_key_hash") {
		t.Fatal("hash exposed by /api/key")
	}
	for _, want := range []string{"key_masked", "key_last4", "key_issued_at", "balance", "status", "bot_name"} {
		if !strings.Contains(body, want) {
			t.Fatalf("metadata contract missing %q: %s", want, body)
		}
	}
}
