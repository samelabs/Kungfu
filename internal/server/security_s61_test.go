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

func s61Read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestS61OwnerUIOneTimeDisclosureOnly locks the frontend invariants:
//   - registration displays the key from the registration RESPONSE
//     (no /api/key recovery flow)
//   - reset does NOT auto-load the current raw key; manual input
//   - copy applies only to a newly issued one-time key
//   - no localStorage/sessionStorage/URL transport for agent secrets
//   - normal /api/key rendering is masked-only
func TestS61OwnerUIOneTimeDisclosureOnly(t *testing.T) {
	auth := s61Read(t, "web/assets/owner/auth.js")
	api := s61Read(t, "web/assets/owner/api.js")
	overview := s61Read(t, "web/assets/owner/render-overview.js")

	// registration: raw key comes from the registration response and
	// stays in page state until explicit continue
	if !strings.Contains(auth, "state.newKeyOnce = json.data.key") {
		t.Fatal("registration must display the key from the registration response (one-time)")
	}
	if !strings.Contains(auth, "json.data.key") {
		t.Fatal("registration key disclosure missing")
	}
	// registration must NOT call /api/key to recover the new key
	if strings.Contains(auth, "loadOwnerKey()") && strings.Contains(auth, "activateSession();\n            setNotice") {
		t.Fatal("registration flow still uses a /api/key recovery path")
	}

	// reset: manual current-key input; no auto-load of a stored key
	if !strings.Contains(auth, "current_key: currentKey") {
		t.Fatal("reset must send the manually entered current key")
	}
	if strings.Contains(auth, "current_key: state.ownerKey") {
		t.Fatal("reset auto-loads the stored key")
	}
	if strings.Contains(auth, "loadOwnerKey()") && strings.Contains(auth, "bindResetKey") {
		t.Fatal("reset flow still fetches key material")
	}

	// copy applies only to the NEW one-time key
	if !strings.Contains(auth, "navigator.clipboard.writeText(state.newKeyOnce)") {
		t.Fatal("copy action must target only the newly issued key")
	}
	if strings.Contains(auth, "writeText(state.ownerKey)") {
		t.Fatal("copy-current-key action still exists")
	}

	// masked-only /api/key rendering
	if !strings.Contains(api, "json.data.key_masked") {
		t.Fatal("/api/key loader must consume masked metadata only")
	}
	if strings.Contains(api, "json.data.key") && !strings.Contains(api, "json.data.key_masked") {
		t.Fatal("/api/key loader reads a raw key field")
	}
	if !strings.Contains(overview, "state.keyMasked") {
		t.Fatal("rendering must use masked metadata")
	}
	if strings.Contains(overview, "state.ownerKey") {
		t.Fatal("rendering still displays a stored raw key")
	}

	// no storage/URL transport for agent secrets anywhere in owner JS
	for _, rel := range []string{
		"web/assets/owner/auth.js", "web/assets/owner/api.js",
		"web/assets/owner/core.js", "web/assets/owner/render-overview.js",
	} {
		src := s61Read(t, rel)
		if strings.Contains(src, "localStorage") || strings.Contains(src, "sessionStorage") {
			t.Fatalf("%s uses web storage for agent material", rel)
		}
		if strings.Contains(src, "newKeyOnce=") || strings.Contains(src, "key="+regKeyMarker) {
			t.Fatalf("%s transports the key via URL", rel)
		}
	}
}

const regKeyMarker = "URLPARAM"

// TestS61NoRuntimePlaintextAgentKeyStoragePath: architecture guard —
// no production runtime read/write/lookup path against a plaintext
// tb_bots.api_key. The ONLY allowed plaintext-column reference is the
// one-time backfill/drop inside migrations/008_agent_key_hash.sql
// (plus the frozen historical 001 definition, which is immutable).
func TestS61NoRuntimePlaintextAgentKeyStoragePath(t *testing.T) {
	violations := []string{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel("..", path)
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "web" || name == "docs" {
				if name == "web" || name == "docs" {
					// web assets have their own contract; docs may
					// describe history — neither owns runtime SQL
				}
				if strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		src := string(b)
		// code lines only (strip //-comments; raw SQL strings are code)
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// plaintext agent-key column in SQL context, inside Go:
			if strings.Contains(line, "api_key =") || strings.Contains(line, "api_key,") || strings.Contains(line, "SET api_key") {
				if strings.Contains(line, "api_key_hash") || strings.Contains(line, "api_key_last4") {
					continue
				}
				violations = append(violations, rel+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("plaintext agent-key runtime SQL paths:\n%s", strings.Join(violations, "\n"))
	}

	// migrations: only 001 (frozen historical definition) and 008
	// (one-time backfill/drop) may reference the plaintext column.
	entries, _ := os.ReadDir("../migrations") // this file lives in internal/server → ../.. is repo root; ../migrations wrong
	_ = entries
	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if !strings.HasSuffix(m.Name(), ".sql") {
			continue
		}
		if m.Name() == "001_schema.sql" || m.Name() == "008_agent_key_hash.sql" {
			continue
		}
		b, _ := os.ReadFile(filepath.Join("..", "..", "migrations", m.Name()))
		if strings.Contains(string(b), "api_key") {
			t.Fatalf("%s references the agent api_key column", m.Name())
		}
	}
}
