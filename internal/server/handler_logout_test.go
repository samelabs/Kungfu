package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// These tests exercise the REAL handleOwnerSessionLogout through the real
// router. They require a local PostgreSQL (the dev environment); without
// KF_TEST_DATABASE_URL they are skipped.

// testConfig returns a minimal config for handler tests.
func testConfig() *config.Config {
	cfg := &config.Config{
		SessionSecret: "a1-logout-test-secret",
		RateLimits:    map[string]config.RateLimitConfig{},
	}
	return cfg
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	return url
}

func newLogoutTestServer(t *testing.T) *Server {
	t.Helper()
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := testConfig()
	return &Server{
		Config:      cfg,
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
		Router:      nil,
	}
}

// seededTestServer seeds a fresh active bot and returns the server plus its id.
func seededTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newLogoutTestServer(t)
	var botID int64
	err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 10)
		 RETURNING id`,
		"a1logout_"+time.Now().Format("150405.000000000"), s61SeedKeyHash("kf_live_test"+time.Now().Format("150405000000000")), s61SeedLast4("kf_live_test"+time.Now().Format("150405000000000")),
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return s, botID
}

func TestLogoutValidSessionSucceedsClearsCookieAndAudits(t *testing.T) {
	s, botID := seededTestServer(t)
	router := s.buildRouter()

	// Login path signs the cookie with the same secret the server uses.
	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	cookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	before := countOwnerLogoutLogs(t, s, botID)

	req := httptest.NewRequest(http.MethodDelete, "/api/owner/session", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["success"] != true || body["message"] != "Owner logout successful" {
		t.Fatalf("body = %v", body)
	}
	assertCookieCleared(t, rec)
	after := countOwnerLogoutLogs(t, s, botID)
	if after != before+1 {
		t.Fatalf("owner_logout audit count = %d, want %d", after, before+1)
	}
}

func TestLogoutNoSessionStillSucceedsNoAudit(t *testing.T) {
	s, botID := seededTestServer(t)
	router := s.buildRouter()

	before := countOwnerLogoutLogs(t, s, botID)

	req := httptest.NewRequest(http.MethodDelete, "/api/owner/session", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even without a session", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["success"] != true {
		t.Fatalf("body = %v", body)
	}
	assertCookieCleared(t, rec)
	if got := countOwnerLogoutLogs(t, s, botID); got != before {
		t.Fatalf("owner_logout audit written without session: %d -> %d", before, got)
	}
}

func TestLogoutInvalidCookieStillSucceedsNoAudit(t *testing.T) {
	s, botID := seededTestServer(t)
	router := s.buildRouter()

	before := countOwnerLogoutLogs(t, s, botID)

	req := httptest.NewRequest(http.MethodDelete, "/api/owner/session", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "kf_owner", Value: "garbage.sig"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with an invalid cookie", rec.Code)
	}
	assertCookieCleared(t, rec)
	if got := countOwnerLogoutLogs(t, s, botID); got != before {
		t.Fatalf("owner_logout audit written for invalid session: %d -> %d", before, got)
	}
}

func TestLogoutDeletedBotSessionStillSucceeds(t *testing.T) {
	// Session points at a bot that no longer exists (DB lookup returns nil):
	// logout must still succeed and clear the cookie, no audit.
	s, _ := seededTestServer(t)
	router := s.buildRouter()

	w := httptest.NewRecorder()
	setOwnerCookie(w, 999999999, s.Config.SessionSecret, false)
	cookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	req := httptest.NewRequest(http.MethodDelete, "/api/owner/session", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the bot no longer exists", rec.Code)
	}
	assertCookieCleared(t, rec)
}

func assertCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	found := false
	for _, c := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(c, "kf_owner=") {
			found = true
			if !strings.Contains(c, "kf_owner=;") {
				t.Fatalf("kf_owner cookie not emptied: %s", c)
			}
			if !strings.Contains(c, "Max-Age=0") && !strings.Contains(c, "Expires=") {
				t.Fatalf("kf_owner cookie not expired: %s", c)
			}
		}
	}
	if !found {
		t.Fatal("no Set-Cookie header clearing kf_owner")
	}
}

func parseSetCookie(t *testing.T, header string) *http.Cookie {
	t.Helper()
	parts := strings.SplitN(header, ";", 2)
	kv := strings.SplitN(parts[0], "=", 2)
	if len(kv) != 2 {
		t.Fatalf("bad Set-Cookie: %q", header)
	}
	return &http.Cookie{Name: kv[0], Value: kv[1]}
}

func countOwnerLogoutLogs(t *testing.T, s *Server, botID int64) int {
	t.Helper()
	var n int
	err := s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_logs WHERE bot_id = $1 AND action = 'owner_logout'`, botID).
		Scan(&n)
	if err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}
