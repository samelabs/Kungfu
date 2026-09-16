package server

// B1.2 HTTP integration tests: CSRF on every new mutation endpoint,
// permission 403 paths, session listing never leaks token material,
// HTML routes render, admin assets serve. Real router + real PG.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/admin"
)

// b12TestEnv: fresh private-ish env on the SHARED test DB (admin rows
// cleaned up per test; bootstrap gate handled by seeding directly).
type b12Env struct {
	s        *Server
	router   http.Handler
	username string
	password string
	cookie   *http.Cookie
	csrf     string
}

func newB12Env(t *testing.T) *b12Env {
	t.Helper()
	s := newAdminTestServer(t)
	s.TrustedProxies = nil

	// ensure an admin exists (shared DB may be empty)
	var count int
	if err := s.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_admins`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	username := "b12_" + time.Now().Format("150405.000000000")
	password := "b12-pass-123"
	if count == 0 {
		if _, err := admin.Bootstrap(context.Background(), s.Pool, username, "B12 Root", password); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	} else {
		hash, err := hashPasswordForAdminTest(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		var id int64
		if err := s.Pool.QueryRow(context.Background(),
			`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1,'B12',$2) RETURNING id`,
			username, hash).Scan(&id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// superadmin so the principal holds the wildcard
		if _, err := s.Pool.Exec(context.Background(), `INSERT INTO tb_admin_user_roles (admin_id, role_id)
			SELECT $1, r.id FROM tb_admin_roles r WHERE r.code='superadmin' ON CONFLICT DO NOTHING`, id); err != nil {
			t.Fatalf("role: %v", err)
		}
		t.Cleanup(func() {
			_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, id)
			_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_sessions WHERE admin_id=$1`, id)
			_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admins WHERE id=$1`, id)
		})
	}

	env := &b12Env{s: s, username: username, password: password, router: s.buildRouter()}
	env.login(t)
	return env
}

func (e *b12Env) login(t *testing.T) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, e.username, e.password)
	req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName {
			e.cookie = c
		}
	}
	var resp struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e.csrf = resp.Data.CSRFToken
}

// do executes an authenticated request; withCSRF adds the header.
func (e *b12Env) do(t *testing.T, method, path, body string, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(e.cookie)
	if withCSRF {
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestB12CSRFRequiredOnEveryNewMutation(t *testing.T) {
	e := newB12Env(t)

	mutations := []struct {
		method string
		path   string
		body   string
	}{
		{"POST", "/api/admin/users", `{"username":"csrf.guy","display_name":"X","password":"long-enough"}`},
		{"PATCH", "/api/admin/users/1", `{"display_name":"X"}`},
		{"POST", "/api/admin/users/1/disable", ""},
		{"POST", "/api/admin/users/1/enable", ""},
		{"PUT", "/api/admin/users/1/roles", `{"role_ids":[]}`},
		{"PUT", "/api/admin/users/1/password", `{"password":"newpass-123"}`},
		{"POST", "/api/admin/users/1/force-logout", ""},
		{"POST", "/api/admin/me/password", `{"current_password":"x","new_password":"y"}`},
		{"POST", "/api/admin/roles", `{"code":"csrf.role","name":"X"}`},
		{"PATCH", "/api/admin/roles/999", `{"name":"X"}`},
		{"PUT", "/api/admin/roles/999/permissions", `{"permission_codes":[]}`},
		{"DELETE", "/api/admin/sessions/999", ""},
	}
	for _, m := range mutations {
		// WITHOUT CSRF → 403 CSRF_INVALID (or 401/404 if it fails
		// earlier, but never 200; principal is valid so CSRF is the
		// gate)
		rec := e.do(t, m.method, m.path, m.body, false)
		if rec.Code == 200 {
			t.Fatalf("%s %s without CSRF must not succeed", m.method, m.path)
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if code, _ := resp["error"].(map[string]interface{})["code"].(string); code == "CSRF_INVALID" {
			continue // ideal
		}
		// fallback: any rejection is acceptable as long as not 2xx,
		// but CSRF_INVALID is expected for valid sessions
		t.Fatalf("%s %s without CSRF: expected CSRF_INVALID, got %d %s", m.method, m.path, rec.Code, rec.Body.String())
	}

	// WITH valid CSRF: request proceeds past the gate (may 404 etc.,
	// but not CSRF_INVALID)
	rec := e.do(t, "POST", "/api/admin/roles", `{"code":"csrf.ok.role","name":"CSRF OK"}`, true)
	if rec.Code == 200 {
		// created now; clean up via disable (roles are not deletable)
		_ = e.do(t, "PATCH", "/api/admin/roles/"+lastRoleID(t, e), `{"name":"CSRF OK","status":"disabled"}`, true)
	} else if !strings.Contains(rec.Body.String(), "CSRF_INVALID") {
		// proceeded past CSRF — acceptable
	} else {
		t.Fatalf("valid CSRF rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func lastRoleID(t *testing.T, e *b12Env) string {
	t.Helper()
	var id int64
	_ = e.s.Pool.QueryRow(context.Background(),
		`SELECT id FROM tb_admin_roles WHERE code='csrf.ok.role'`).Scan(&id)
	return fmt.Sprintf("%d", id)
}

func TestB12PermissionDeniedPaths(t *testing.T) {
	// limited admin: no permissions at all
	s := newAdminTestServer(t)
	username := "noperm_" + time.Now().Format("150405.000000000")
	password := "noperm-pass-1"
	hash, _ := hashPasswordForAdminTest(password)
	var id int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1,'NP',$2) RETURNING id`,
		username, hash).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_sessions WHERE admin_id=$1`, id)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admins WHERE id=$1`, id)
	})
	router := s.buildRouter()

	// login
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest("POST", "/api/admin/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login: %d", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName {
			cookie = c
		}
	}
	var login struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	get := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	mutate := func(method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", login.Data.CSRFToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if c := get("/api/admin/users"); c != 403 {
		t.Fatalf("users list without perm = %d, want 403", c)
	}
	if c := get("/api/admin/roles"); c != 403 {
		t.Fatalf("roles list without perm = %d", c)
	}
	if c := get("/api/admin/sessions"); c != 403 {
		t.Fatalf("sessions without perm = %d", c)
	}
	if c := get("/api/admin/audit"); c != 403 {
		t.Fatalf("audit without perm = %d", c)
	}
	if c := mutate("POST", "/api/admin/users"); c != 403 {
		t.Fatalf("create user without perm = %d", c)
	}
}

func TestB12SessionListNeverLeaksTokenMaterial(t *testing.T) {
	e := newB12Env(t)
	rec := e.do(t, "GET", "/api/admin/sessions", "", false)
	if rec.Code != 200 {
		t.Fatalf("sessions list: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "token_hash") || strings.Contains(body, e.cookie.Value) {
		t.Fatal("sessions response leaked token material")
	}
	if !strings.Contains(body, `"username"`) {
		t.Fatal("sessions response missing username")
	}
}

func TestB12AuditExplorerPaginationAndFilters(t *testing.T) {
	e := newB12Env(t)
	// generate a few audit facts
	_ = e.do(t, "GET", "/api/admin/audit?action=admin.login&page=1&page_size=5", "", false)
	rec := e.do(t, "GET", "/api/admin/audit?page=1&page_size=5", "", false)
	if rec.Code != 200 {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Items    []map[string]interface{} `json:"items"`
			Page     int                      `json:"page"`
			PageSize int                      `json:"page_size"`
			Total    int64                    `json:"total"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Data.Page != 1 || resp.Data.PageSize != 5 {
		t.Fatalf("pagination echo: %+v", resp.Data)
	}
	if resp.Data.Total < 1 {
		t.Fatalf("total = %d, want >= 1 (login audit exists)", resp.Data.Total)
	}
	if len(resp.Data.Items) > 5 {
		t.Fatal("page_size exceeded")
	}
	// filter by action
	rec = e.do(t, "GET", "/api/admin/audit?action=admin.login", "", false)
	if rec.Code != 200 {
		t.Fatalf("audit filter: %d", rec.Code)
	}
}

func TestB12AdminHTMLRoutesRender(t *testing.T) {
	e := newB12Env(t)
	for _, path := range []string{"/admin", "/admin/login", "/admin/account", "/admin/users",
		"/admin/roles", "/admin/sessions", "/admin/audit"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s content-type = %s", path, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), "Admin Workspace") {
			t.Fatalf("%s did not render the admin shell", path)
		}
	}
	// no owner identity references in the admin shell
	for _, banned := range []string{"kf_owner", "/api/owner/", "OWNER_I18N"} {
		req := httptest.NewRequest("GET", "/admin/users", nil)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), banned) {
			t.Fatalf("admin shell references %s", banned)
		}
	}
}

func TestB12AdminAssetsServe(t *testing.T) {
	e := newB12Env(t)
	for _, asset := range []string{
		"/assets/admin/core.js", "/assets/admin/api.js", "/assets/admin/auth.js",
		"/assets/admin/users.js", "/assets/admin/roles.js", "/assets/admin/sessions.js",
		"/assets/admin/audit.js", "/assets/admin/init.js", "/assets/admin.css",
	} {
		req := httptest.NewRequest("GET", asset, nil)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", asset, rec.Code)
		}
	}
}

func TestB12SelfPasswordChangeClearsCookie(t *testing.T) {
	e := newB12Env(t)
	newPass := "rotated-pass-9"
	rec := e.do(t, "POST", "/api/admin/me/password",
		fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, e.password, newPass), true)
	if rec.Code != 200 {
		t.Fatalf("me/password: %d %s", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("kf_admin cookie not cleared after self password change")
	}
	// audit row without secrets
	var blob string
	_ = e.s.Pool.QueryRow(context.Background(),
		`SELECT after_json::text || ' ' || COALESCE(metadata_json::text,'') FROM tb_admin_audit_logs
		 WHERE action='admin.password.change' AND success ORDER BY id DESC LIMIT 1`).Scan(&blob)
	if strings.Contains(blob, newPass) || strings.Contains(blob, e.password) {
		t.Fatal("audit leaked password")
	}
	// old session invalid
	rec = e.do(t, "GET", "/api/admin/session", "", false)
	if rec.Code != 401 {
		t.Fatalf("old session must be revoked: %d", rec.Code)
	}
}
