package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// B1.1 Admin HTTP integration tests: real router + real PostgreSQL.
// Isolation proofs: kf_owner ↛ admin, kf_admin ↛ owner, X-Bot-Key ↛ admin.

func newAdminTestServer(t *testing.T) *Server {
	t.Helper()
	url := testDatabaseURL(t)
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	cfg := testConfig()
	return &Server{
		Config:      cfg,
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
}

// seedAdminForHTTP bootstraps (or reuses) an admin named root with a
// known password; the shared CI database may already have admins, in
// which case seed directly with a unique username.
func seedAdminForHTTP(t *testing.T, s *Server) (username, password string) {
	t.Helper()
	password = "http-pass-123"
	var count int
	if err := s.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_admins`).Scan(&count); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count == 0 {
		if _, err := admin.Bootstrap(context.Background(), s.Pool, "httproot", "HTTP Root", password); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		return "httproot", password
	}
	// direct seed with unique username (no bootstrap gate involved)
	username = "h_" + time.Now().Format("150405.000000000")
	hash, err := hashPasswordForAdminTest(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1, 'HTTP', $2) RETURNING id`,
		username, hash).Scan(&id); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	// superadmin role so the principal is fully privileged
	_, _ = s.Pool.Exec(context.Background(), `INSERT INTO tb_admin_user_roles (admin_id, role_id)
		SELECT $1, r.id FROM tb_admin_roles r WHERE r.code='superadmin' ON CONFLICT DO NOTHING`, id)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.Pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, id)
		_, _ = s.Pool.Exec(ctx, `DELETE FROM tb_admin_sessions WHERE admin_id=$1`, id)
		_, _ = s.Pool.Exec(ctx, `DELETE FROM tb_admins WHERE id=$1`, id)
	})
	return username, password
}

func httpLoginAdmin(t *testing.T, s *Server, username, password string) *http.Cookie {
	t.Helper()
	router := s.buildRouter()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("admin login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName {
			return c
		}
	}
	t.Fatal("no kf_admin cookie set")
	return nil
}

func TestAdminLoginSetsIndependentCookie(t *testing.T) {
	s := newAdminTestServer(t)
	username, password := seedAdminForHTTP(t, s)
	router := s.buildRouter()

	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	// cookie attributes
	cookies := rec.Result().Cookies()
	var ac *http.Cookie
	for _, c := range cookies {
		if c.Name == admin.AdminCookieName {
			ac = c
		}
	}
	if ac == nil {
		t.Fatal("kf_admin cookie missing")
	}
	if !ac.HttpOnly {
		t.Fatal("kf_admin must be HttpOnly")
	}
	if ac.SameSite != http.SameSiteStrictMode {
		t.Fatalf("kf_admin SameSite = %v, want Strict", ac.SameSite)
	}
	if ac.Path != "/" {
		t.Fatalf("kf_admin Path = %q", ac.Path)
	}
	// response must NOT leak secrets
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	data, _ := resp["data"].(map[string]interface{})
	for _, banned := range []string{"password_hash", "token", "token_hash", "auth_version"} {
		if _, ok := data[banned]; ok {
			t.Fatalf("login response leaks %q", banned)
		}
	}
	// raw token never in DB
	var stored string
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT token_hash FROM tb_admin_sessions ORDER BY id DESC LIMIT 1`).Scan(&stored)
	if stored == ac.Value {
		t.Fatal("raw cookie value stored in DB")
	}
	if stored != admin.HashSessionToken(ac.Value) {
		t.Fatal("stored hash != sha256(cookie value)")
	}
}

func TestAdminSessionGetReturnsPrincipalAndCSRF(t *testing.T) {
	s := newAdminTestServer(t)
	username, password := seedAdminForHTTP(t, s)
	router := s.buildRouter()
	cookie := httpLoginAdmin(t, s, username, password)

	req := httptest.NewRequest("GET", "/api/admin/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get session: %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	data, _ := resp["data"].(map[string]interface{})
	for _, key := range []string{"id", "username", "display_name", "roles", "permissions", "expires_at", "csrf_token"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("GET session missing %q: %v", key, data)
		}
	}
	for _, banned := range []string{"password_hash", "token", "token_hash", "auth_version"} {
		if _, ok := data[banned]; ok {
			t.Fatalf("GET session leaks %q", banned)
		}
	}
}

// Isolation: kf_owner cookie must NOT authenticate the admin plane.
func TestOwnerCookieCannotAccessAdminPlane(t *testing.T) {
	s := newAdminTestServer(t)
	_, botID := seededTestServer(t)
	router := s.buildRouter()

	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	ownerCookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	req := httptest.NewRequest("GET", "/api/admin/session", nil)
	req.AddCookie(ownerCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("kf_owner reached admin plane: %d %s", rec.Code, rec.Body.String())
	}
}

// Isolation: X-Bot-Key must NOT authenticate the admin plane.
func TestBotKeyCannotAccessAdminPlane(t *testing.T) {
	s := newAdminTestServer(t)
	router := s.buildRouter()
	req := httptest.NewRequest("GET", "/api/admin/session", nil)
	req.Header.Set("X-Bot-Key", "kf_live_"+strings.Repeat("a", 64))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("X-Bot-Key reached admin plane: %d", rec.Code)
	}
}

// Isolation: kf_admin cookie must NOT authenticate the owner plane.
func TestAdminCookieCannotAccessOwnerPlane(t *testing.T) {
	s := newAdminTestServer(t)
	username, password := seedAdminForHTTP(t, s)
	router := s.buildRouter()
	adminCookie := httpLoginAdmin(t, s, username, password)

	req := httptest.NewRequest("GET", "/api/owner/session", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("kf_admin impersonated owner: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminLogoutCSRFAndRevocation(t *testing.T) {
	s := newAdminTestServer(t)
	username, password := seedAdminForHTTP(t, s)
	router := s.buildRouter()
	cookie := httpLoginAdmin(t, s, username, password)

	// 1) DELETE without CSRF token → rejected, session stays live
	req := httptest.NewRequest("DELETE", "/api/admin/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("logout without CSRF must be 403, got %d", rec.Code)
	}
	req2 := httptest.NewRequest("GET", "/api/admin/session", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatal("session must still be live after CSRF-rejected logout")
	}

	// 2) DELETE with wrong CSRF → rejected
	req3 := httptest.NewRequest("DELETE", "/api/admin/session", nil)
	req3.AddCookie(cookie)
	req3.Header.Set("X-CSRF-Token", "bogus")
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != 403 {
		t.Fatalf("wrong CSRF must be 403, got %d", rec3.Code)
	}

	// 3) DELETE with valid CSRF → revoked + cookie cleared
	req4 := httptest.NewRequest("DELETE", "/api/admin/session", nil)
	req4.AddCookie(cookie)
	req4.Header.Set("X-CSRF-Token", admin.CSRFToken(cookie.Value, s.Config.SessionSecret))
	rec4 := httptest.NewRecorder()
	router.ServeHTTP(rec4, req4)
	if rec4.Code != 200 {
		t.Fatalf("logout: %d %s", rec4.Code, rec4.Body.String())
	}
	cleared := false
	for _, c := range rec4.Result().Cookies() {
		if c.Name == admin.AdminCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("kf_admin cookie not cleared on logout")
	}
	// session now dead
	req5 := httptest.NewRequest("GET", "/api/admin/session", nil)
	req5.AddCookie(cookie)
	rec5 := httptest.NewRecorder()
	router.ServeHTTP(rec5, req5)
	if rec5.Code != 401 {
		t.Fatal("revoked session must 401")
	}
}

func TestAdminLogoutWithStaleCookieStillClearsLocally(t *testing.T) {
	s := newAdminTestServer(t)
	router := s.buildRouter()
	// bogus token → invalid session, no CSRF header
	req := httptest.NewRequest("DELETE", "/api/admin/session", nil)
	req.AddCookie(&http.Cookie{Name: admin.AdminCookieName, Value: "stale-garbage"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("stale-cookie logout must succeed: %d %s", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("stale kf_admin cookie must still be cleared")
	}
}

func TestAdminLoginRateLimitIndependentNamespace(t *testing.T) {
	s := newAdminTestServer(t)
	s.RateLimiter = ratelimit.NewLimiter(map[string]ratelimit.Config{
		"admin_login": {Window: 900, Limit: 3, Enabled: true},
	})
	username, password := seedAdminForHTTP(t, s)
	router := s.buildRouter()

	doLogin := func() int {
		body := `{"username":"` + username + `","password":"` + password + `"}`
		req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "9.9.9.9:1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if c := doLogin(); c != 200 {
			t.Fatalf("login %d should pass, got %d", i, c)
		}
	}
	if c := doLogin(); c != 429 {
		t.Fatalf("4th login must be rate limited, got %d", c)
	}
	// owner namespace NOT drained: owner login from same IP still has budget
	res := s.RateLimiter.CheckOwnerLogin("9.9.9.9")
	if !res.Allowed {
		t.Fatal("admin rate limit must not drain the owner_login namespace")
	}
}

func TestAdminLoginUniform401Externally(t *testing.T) {
	s := newAdminTestServer(t)
	router := s.buildRouter()

	attempt := func(user, pass string) (int, string) {
		body := `{"username":"` + user + `","password":"` + pass + `"}`
		req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var resp map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		data, _ := resp["error"].(map[string]interface{})
		code := ""
		if data != nil {
			code, _ = data["code"].(string)
		}
		return rec.Code, code
	}
	c1, code1 := attempt("definitely.missing", "whatever-1")
	c2, code2 := attempt("httproot", "wrong-password")
	if !(c1 == c2 && code1 == code2) {
		t.Fatalf("unknown vs wrong-password differ externally: (%d,%s) vs (%d,%s)", c1, code1, c2, code2)
	}
	if c1 != 401 || code1 != "INVALID_CREDENTIALS" {
		t.Fatalf("expected 401 INVALID_CREDENTIALS, got (%d,%s)", c1, code1)
	}
}
