package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// TestOwnerRegisterCanonicalRouteIntegration proves the Owner browser
// registration surface on its canonical route through the real router
// with a real PostgreSQL pool: success via the shared service.Register
// authority, one-time Agent key disclosure, unchanged signup credit,
// and the legacy Agent REST route stays 404.
func TestOwnerRegisterCanonicalRouteIntegration(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "ownreg_" + suffix

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	router := s.buildRouter()

	// Legacy Agent REST registration stays removed.
	legacy := httptest.NewRequest("POST", "/api/register", strings.NewReader(
		fmt.Sprintf(`{"name":%q,"password":"password123"}`, name)))
	legacy.Header.Set("Content-Type", "application/json")
	legacyRec := httptest.NewRecorder()
	router.ServeHTTP(legacyRec, legacy)
	if legacyRec.Code != 404 {
		t.Fatalf("POST /api/register = %d, want 404 (removed)", legacyRec.Code)
	}

	// Canonical Owner registration: JSON mutation gate + shared
	// service.Register authority.
	req := httptest.NewRequest("POST", "/api/owner/register", strings.NewReader(
		fmt.Sprintf(`{"name":%q,"password":"password123"}`, name)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /api/owner/register = %d %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success {
		t.Fatalf("registration not successful: %s", rec.Body.String())
	}
	if !strings.HasPrefix(body.Data.Key, "kf_live_") {
		t.Fatalf("one-time Agent key not disclosed: %q", body.Data.Key)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE bot_id=(SELECT id FROM tb_bots WHERE bot_name=$1)`, name)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE bot_name=$1`, name)
	})

	// Signup credit authority unchanged: the +66 grant_signup ledger row
	// exists in the same transaction (existing Credits mechanism).
	ctx := context.Background()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tb_transactions t JOIN tb_bots b ON b.id=t.bot_id WHERE b.bot_name=$1 AND t.type='grant_signup'`,
		name).Scan(&count); err != nil {
		t.Fatalf("signup credit query: %v", err)
	}
	if count != 1 {
		t.Fatalf("grant_signup rows = %d, want 1", count)
	}

	// Duplicate name through the canonical route: shared authority
	// 409, no second key.
	dup := httptest.NewRequest("POST", "/api/owner/register", strings.NewReader(
		fmt.Sprintf(`{"name":%q,"password":"password123"}`, name)))
	dup.Header.Set("Content-Type", "application/json")
	dupRec := httptest.NewRecorder()
	router.ServeHTTP(dupRec, dup)
	if dupRec.Code != 409 {
		t.Fatalf("duplicate register = %d, want 409", dupRec.Code)
	}
}
