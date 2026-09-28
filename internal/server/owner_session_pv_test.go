package server

// WO-17b A2: the owner session cookie carries pv (the password
// version), so a password change invalidates every previously issued
// cookie — and the change-password response re-issues one bound to the
// NEW password, keeping the signed-in owner signed in.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

func pvTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	// a REAL bcrypt hash: the pv comparison runs against the stored hash
	hash, err := auth.HashPassword("passpass123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	name := "pvbot" + time.Now().Format("150405.000000000")
	keyDigest := sha256.Sum256([]byte(name)) // 32 bytes: ck_bots_api_key_hash_len
	var botID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		VALUES ($1, $2, 'pv00', $3, 'active', 10) RETURNING id`,
		name, keyDigest[:], hash).Scan(&botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return s, name, hash
}

func TestOwnerPasswordChangeInvalidatesOldCookies(t *testing.T) {
	s, name, hash := pvTestServer(t)
	router := s.buildRouter()

	// login as the owner → cookie bound to the current password version
	loginBody, _ := json.Marshal(map[string]string{"name": name, "password": "passpass123"})
	req := httptest.NewRequest(http.MethodPost, "/api/owner/session", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	oldCookie := parseSetCookie(t, rec.Header().Get("Set-Cookie"))

	// the cookie authenticates
	req = httptest.NewRequest(http.MethodGet, "/api/account", nil)
	req.AddCookie(oldCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-change session: %d", rec.Code)
	}

	// change the password through the session
	chgBody, _ := json.Marshal(map[string]string{"password": "passpass123", "new_password": "newpass456"})
	req = httptest.NewRequest(http.MethodPost, "/api/change-password", bytes.NewReader(chgBody))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(oldCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password: %d %s", rec.Code, rec.Body.String())
	}
	var chgResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &chgResp)
	if _, leaked := chgResp["password_hash"]; leaked {
		t.Fatal("change-password response leaked the password hash")
	}
	newCookie := parseSetCookie(t, rec.Header().Get("Set-Cookie"))
	if newCookie == nil {
		t.Fatal("change-password did not re-issue the session cookie")
	}

	// the OLD cookie no longer authenticates (401), the new one does
	req = httptest.NewRequest(http.MethodGet, "/api/account", nil)
	req.AddCookie(oldCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after change: %d, want 401", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/account", nil)
	req.AddCookie(newCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-issued cookie: %d, want 200", rec.Code)
	}

	// a cookie minted with a WRONG pv (forged stale version) is rejected
	w := httptest.NewRecorder()
	setOwnerCookie(w, 1, hash, s.Config.SessionSecret, false) // bot 1's hash mismatch
	forged := parseSetCookie(t, w.Header().Get("Set-Cookie"))
	req = httptest.NewRequest(http.MethodGet, "/api/account", nil)
	req.AddCookie(forged)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("mismatched pv cookie: %d, want 401", rec.Code)
	}
}
