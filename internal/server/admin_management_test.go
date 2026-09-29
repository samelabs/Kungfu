package server

// HTTP integration tests: CSRF on every new mutation endpoint,
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
// cleaned up per test; seeded directly — no bootstrap mechanism exists).
type adminEnv struct {
	s        *Server
	router   http.Handler
	username string
	password string
	cookie   *http.Cookie
	csrf     string
	adminID  int64
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	s := newAdminTestServer(t)
	s.TrustedProxies = nil

	// seed an admin directly (admin rows are operator-seeded data; the
	// runtime has no bootstrap mechanism)
	username := "b12_" + time.Now().Format("150405.000000000")
	password := "b12-pass-123"
	hash, err := hashPasswordForAdminTest(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1,'Admin',$2) RETURNING id`, username, hash).Scan(&id); err != nil {
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

	env := &adminEnv{s: s, username: username, password: password, router: s.buildRouter(), adminID: id}
	env.login(t)
	return env
}

func (e *adminEnv) login(t *testing.T) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, e.username, e.password)
	req := httptest.NewRequest("POST", "/api/samelabs/session", strings.NewReader(body))
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
func (e *adminEnv) do(t *testing.T, method, path, body string, withCSRF bool) *httptest.ResponseRecorder {
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

func TestAdminMgmtCSRFRequiredOnEveryNewMutation(t *testing.T) {
	e := newAdminEnv(t)

	mutations := []struct {
		method string
		path   string
		body   string
	}{
		{"POST", "/api/samelabs/users", `{"username":"csrf.guy","display_name":"X","password":"long-enough"}`},
		{"PATCH", "/api/samelabs/users/1", `{"display_name":"X"}`},
		{"POST", "/api/samelabs/users/1/disable", ""},
		{"POST", "/api/samelabs/users/1/enable", ""},
		{"PUT", "/api/samelabs/users/1/roles", `{"role_ids":[]}`},
		{"PUT", "/api/samelabs/users/1/password", `{"password":"newpass-123"}`},
		{"POST", "/api/samelabs/users/1/force-logout", ""},
		{"POST", "/api/samelabs/me/password", `{"current_password":"x","new_password":"y"}`},
		{"POST", "/api/samelabs/roles", `{"code":"csrf.role","name":"X"}`},
		{"PATCH", "/api/samelabs/roles/999", `{"name":"X"}`},
		{"PUT", "/api/samelabs/roles/999/permissions", `{"permission_codes":[]}`},
		{"DELETE", "/api/samelabs/sessions/999", ""},
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
	rec := e.do(t, "POST", "/api/samelabs/roles", `{"code":"csrf.ok.role","name":"CSRF OK"}`, true)
	if rec.Code == 200 {
		// created now; clean up via disable (roles are not deletable)
		_ = e.do(t, "PATCH", "/api/samelabs/roles/"+lastRoleID(t, e), `{"name":"CSRF OK","status":"disabled"}`, true)
	} else if !strings.Contains(rec.Body.String(), "CSRF_INVALID") {
		// proceeded past CSRF — acceptable
	} else {
		t.Fatalf("valid CSRF rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func lastRoleID(t *testing.T, e *adminEnv) string {
	t.Helper()
	var id int64
	_ = e.s.Pool.QueryRow(context.Background(),
		`SELECT id FROM tb_admin_roles WHERE code='csrf.ok.role'`).Scan(&id)
	return fmt.Sprintf("%d", id)
}

func TestAdminMgmtPermissionDeniedPaths(t *testing.T) {
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
	req := httptest.NewRequest("POST", "/api/samelabs/session", strings.NewReader(body))
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

	if c := get("/api/samelabs/users"); c != 403 {
		t.Fatalf("users list without perm = %d, want 403", c)
	}
	if c := get("/api/samelabs/roles"); c != 403 {
		t.Fatalf("roles list without perm = %d", c)
	}
	if c := get("/api/samelabs/sessions"); c != 403 {
		t.Fatalf("sessions without perm = %d", c)
	}
	if c := get("/api/samelabs/audit"); c != 403 {
		t.Fatalf("audit without perm = %d", c)
	}
	if c := mutate("POST", "/api/samelabs/users"); c != 403 {
		t.Fatalf("create user without perm = %d", c)
	}
}

func TestAdminMgmtSessionListNeverLeaksTokenMaterial(t *testing.T) {
	e := newAdminEnv(t)
	rec := e.do(t, "GET", "/api/samelabs/sessions", "", false)
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

func TestAdminMgmtAuditExplorerPaginationAndFilters(t *testing.T) {
	e := newAdminEnv(t)
	// generate a few audit facts
	_ = e.do(t, "GET", "/api/samelabs/audit?action=admin.login&page=1&page_size=5", "", false)
	rec := e.do(t, "GET", "/api/samelabs/audit?page=1&page_size=5", "", false)
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
	rec = e.do(t, "GET", "/api/samelabs/audit?action=admin.login", "", false)
	if rec.Code != 200 {
		t.Fatalf("audit filter: %d", rec.Code)
	}
}

func TestAdminMgmtSelfPasswordChangeClearsCookie(t *testing.T) {
	e := newAdminEnv(t)
	newPass := "rotated-pass-9"
	rec := e.do(t, "POST", "/api/samelabs/me/password",
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
	rec = e.do(t, "GET", "/api/samelabs/session", "", false)
	if rec.Code != 401 {
		t.Fatalf("old session must be revoked: %d", rec.Code)
	}
}

// TestAdminMgmtSessionsLiveByDefaultPaged (WO-21): the sessions list
// shows only unrevoked, unexpired sessions by default; include_ended=1
// adds the history; paging is normPage-bounded with a page-independent
// total.
func TestAdminMgmtSessionsLiveByDefaultPaged(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()

	// ended fixtures: one revoked, one expired — both invisible by default
	var revokedID, expiredID int64
	if err := e.s.Pool.QueryRow(ctx, `
		INSERT INTO tb_admin_sessions (admin_id, token_hash, auth_version, ip_address, user_agent, created_at, last_seen_at, expires_at, revoked_at)
		VALUES ($1, 'revokedhash0000000000000000000000000', 1, '127.0.0.1', 'test', NOW(), NOW(), NOW() + interval '1 day', NOW())
		RETURNING id`, e.adminID).Scan(&revokedID); err != nil {
		t.Fatalf("seed revoked: %v", err)
	}
	if err := e.s.Pool.QueryRow(ctx, `
		INSERT INTO tb_admin_sessions (admin_id, token_hash, auth_version, ip_address, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1, 'expiredhash0000000000000000000000000', 1, '127.0.0.1', 'test', NOW() - interval '2 day', NOW() - interval '2 day', NOW() - interval '1 day')
		RETURNING id`, e.adminID).Scan(&expiredID); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_sessions WHERE id IN ($1, $2)`, revokedID, expiredID)
	})

	// default: live only — the revoked and expired ids never appear
	rec := e.do(t, "GET", "/api/samelabs/sessions", "", false)
	if rec.Code != 200 {
		t.Fatalf("default list: %d", rec.Code)
	}
	var got struct {
		Success bool `json:"success"`
		Data    struct {
			Sessions []struct {
				ID int64 `json:"id"`
			} `json:"sessions"`
			Pagination struct {
				Total        int64 `json:"total"`
				IncludeEnded bool  `json:"include_ended"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Data.Sessions {
		if s.ID == revokedID || s.ID == expiredID {
			t.Fatal("default list included an ended session")
		}
	}
	if got.Data.Pagination.IncludeEnded {
		t.Fatal("include_ended flag echoed wrong")
	}
	liveTotal := got.Data.Pagination.Total

	// include_ended=1: both fixtures appear
	rec = e.do(t, "GET", "/api/samelabs/sessions?include_ended=1", "", false)
	if rec.Code != 200 {
		t.Fatalf("include_ended list: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`"id":%d`, revokedID)) || !strings.Contains(body, fmt.Sprintf(`"id":%d`, expiredID)) {
		t.Fatal("include_ended=1 missing ended sessions")
	}

	// paging: page 2 of size 1 keeps the full total (>= live sessions)
	rec = e.do(t, "GET", fmt.Sprintf("/api/samelabs/sessions?page=2&page_size=1"), "", false)
	if rec.Code != 200 {
		t.Fatalf("paged list: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data.Sessions) > 1 {
		t.Fatalf("page_size=1 returned %d rows", len(got.Data.Sessions))
	}
	if got.Data.Pagination.Total != liveTotal {
		t.Fatalf("paged total = %d, want the live total %d", got.Data.Pagination.Total, liveTotal)
	}
}
