package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/security"
)

// Behavior tests for the server-rendered console (/samelabs): session
// gate, sign-in, CSRF on every form, permission gating, and the
// governance actions' effects on real rows.

func (e *b12Env) page(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// form posts an HTML form; csrf=false omits the token.
func (e *b12Env) form(t *testing.T, path string, vals url.Values, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	if vals == nil {
		vals = url.Values{}
	}
	if csrf {
		vals.Set("_csrf", e.csrf)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func flashOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == slFlashName && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

type slSeed struct {
	botID    int64
	taskCode string
	memCode  string
}

func seedConsoleData(t *testing.T, e *b12Env) slSeed {
	t.Helper()
	ctx := context.Background()
	n := time.Now().UnixNano() % 1_000_000_000
	name := fmt.Sprintf("sl_%d", n)
	h := sha256.Sum256([]byte(name))
	sd := slSeed{taskCode: fmt.Sprintf("st%010d", n), memCode: fmt.Sprintf("sm%010d", n)}
	if err := e.s.Pool.QueryRow(ctx, `INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
		VALUES ($1, $2, '1234', 'x', 7777, 'active') RETURNING id`, name, h[:]).Scan(&sd.botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	if _, err := e.s.Pool.Exec(ctx, `INSERT INTO tb_tasks (code, bot_id, title, requirements, postapi, budget, price, status, opened_at)
		VALUES ($1, $2, 'Console task', 'do it', 'https://example.com/hook', 3000, 100, 'open', NOW())`, sd.taskCode, sd.botID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := e.s.Pool.Exec(ctx, `INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility)
		VALUES ($1, $2, 'Console memory', '["x"]', 'body', repeat('a', 64), 'public')`, sd.memCode, sd.botID); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_kungfus WHERE bot_id = $1`, sd.botID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_tasks WHERE bot_id = $1`, sd.botID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_bots WHERE id = $1`, sd.botID)
	})
	return sd
}

func TestSamelabsRequiresSession(t *testing.T) {
	e := newB12Env(t)
	req := httptest.NewRequest("GET", "/samelabs/tasks?status=open", nil)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/samelabs/login?next=%2Fsamelabs%2Ftasks%3Fstatus%3Dopen" {
		t.Fatalf("anonymous page = %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	req = httptest.NewRequest("POST", "/samelabs/tasks/x/close", strings.NewReader("reason=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/samelabs/login") {
		t.Fatalf("anonymous action = %d → %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSamelabsSignIn(t *testing.T) {
	e := newB12Env(t)
	post := func(user, pass, next string) *httptest.ResponseRecorder {
		v := url.Values{"username": {user}, "password": {pass}, "next": {next}}
		req := httptest.NewRequest("POST", "/samelabs/login", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(e.username, "wrong-password", "/samelabs"); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "Username or password is incorrect") {
		t.Fatalf("bad password = %d", rec.Code)
	}
	for next, want := range map[string]string{
		"/samelabs/tasks":        "/samelabs/tasks",
		"https://evil.example/":  "/samelabs",
		"//evil.example/x":       "/samelabs",
		"/samelabsevil":          "/samelabs",
		"/samelabs/login?next=x": "/samelabs",
	} {
		rec := post(e.username, e.password, next)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Fatalf("next=%q → %d %q, want %q", next, rec.Code, rec.Header().Get("Location"), want)
		}
		var got bool
		for _, c := range rec.Result().Cookies() {
			if c.Name == admin.AdminCookieName && c.Value != "" && c.HttpOnly {
				got = true
			}
		}
		if !got {
			t.Fatal("sign-in did not set the HttpOnly admin cookie")
		}
	}
}

func TestSamelabsPagesRender(t *testing.T) {
	e := newB12Env(t)
	sd := seedConsoleData(t, e)
	for _, path := range []string{
		"/samelabs", "/samelabs/account", "/samelabs/tasks", "/samelabs/tasks/" + sd.taskCode,
		"/samelabs/memories", "/samelabs/memories/" + sd.memCode, "/samelabs/accounts",
		fmt.Sprintf("/samelabs/accounts/%d", sd.botID), "/samelabs/finance", "/samelabs/finance?tab=adjustments",
		"/samelabs/finance?tab=ledger", "/samelabs/store/products", "/samelabs/store/redemptions",
		"/samelabs/settings/payment", "/samelabs/admins", "/samelabs/roles", "/samelabs/sessions", "/samelabs/audit",
	} {
		rec := e.page(t, path)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s is cacheable", path)
		}
		for _, banned := range []string{"kf_owner", "/api/owner/", "OWNER_I18N"} {
			if strings.Contains(body, banned) {
				t.Fatalf("%s references %s", path, banned)
			}
		}
	}
	if rec := e.page(t, "/samelabs/tasks/nope00000000"); rec.Code != 404 {
		t.Fatalf("missing task = %d", rec.Code)
	}
	if rec := e.page(t, "/samelabs/accounts/999999999"); rec.Code != 404 {
		t.Fatalf("missing account = %d", rec.Code)
	}
	for _, asset := range []string{"/assets/samelabs.css", "/assets/samelabs.js"} {
		if rec := e.page(t, asset); rec.Code != 200 {
			t.Fatalf("%s = %d", asset, rec.Code)
		}
	}
}

func TestSamelabsFormsRequireCSRF(t *testing.T) {
	e := newB12Env(t)
	sd := seedConsoleData(t, e)
	var pinned bool
	pinnedNow := func() bool {
		_ = e.s.Pool.QueryRow(context.Background(), `SELECT pinned FROM tb_tasks WHERE code=$1`, sd.taskCode).Scan(&pinned)
		return pinned
	}
	if rec := e.form(t, "/samelabs/tasks/"+sd.taskCode+"/pin", nil, false); rec.Code != http.StatusForbidden || pinnedNow() {
		t.Fatalf("pin without CSRF = %d pinned=%v", rec.Code, pinned)
	}
	bad := url.Values{"_csrf": {"forged"}}
	if rec := e.form(t, "/samelabs/tasks/"+sd.taskCode+"/pin", bad, false); rec.Code != http.StatusForbidden || pinnedNow() {
		t.Fatalf("pin with forged CSRF = %d", rec.Code)
	}
	rec := e.form(t, "/samelabs/tasks/"+sd.taskCode+"/pin", nil, true)
	if rec.Code != http.StatusSeeOther || !pinnedNow() || flashOf(rec) == "" {
		t.Fatalf("pin = %d pinned=%v", rec.Code, pinned)
	}
}

func TestSamelabsTaskCloseMovesNoCredits(t *testing.T) {
	e := newB12Env(t)
	sd := seedConsoleData(t, e)
	ctx := context.Background()
	state := func() (status, note string, budget, balance int64) {
		_ = e.s.Pool.QueryRow(ctx, `SELECT t.status, COALESCE(t.review_note,''), t.budget, b.balance
			FROM tb_tasks t JOIN tb_bots b ON b.id = t.bot_id WHERE t.code=$1`, sd.taskCode).Scan(&status, &note, &budget, &balance)
		return
	}
	// A reason is mandatory.
	e.form(t, "/samelabs/tasks/"+sd.taskCode+"/close", url.Values{"reason": {"  "}}, true)
	if st, _, _, _ := state(); st != "open" {
		t.Fatal("closed without a reason")
	}
	rec := e.form(t, "/samelabs/tasks/"+sd.taskCode+"/close", url.Values{"reason": {"Breaks the rules"}}, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("close = %d", rec.Code)
	}
	st, note, budget, balance := state()
	if st != "closed" || note != "Breaks the rules" || budget != 3000 || balance != 7777 {
		t.Fatalf("after close: status=%s note=%q budget=%d balance=%d", st, note, budget, balance)
	}
	var audits int
	_ = e.s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='task.close' AND target_id=$1`, sd.taskCode).Scan(&audits)
	if audits != 1 {
		t.Fatalf("task.close audit rows = %d", audits)
	}
}

func TestSamelabsMemoryRemove(t *testing.T) {
	e := newB12Env(t)
	sd := seedConsoleData(t, e)
	rec := e.form(t, "/samelabs/memories/"+sd.memCode+"/remove", nil, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d", rec.Code)
	}
	var vis, st string
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT visibility, status FROM tb_kungfus WHERE code=$1`, sd.memCode).Scan(&vis, &st)
	if vis != "private" || st != "deleted" {
		t.Fatalf("after remove: %s/%s", vis, st)
	}
}

func TestSamelabsPermissionGating(t *testing.T) {
	e := newB12Env(t)
	sd := seedConsoleData(t, e)
	ctx := context.Background()
	// A second admin whose only role grants tasks.read.
	role := fmt.Sprintf("tasks_ro_%d", time.Now().UnixNano()%1_000_000)
	var roleID int64
	if err := e.s.Pool.QueryRow(ctx, `INSERT INTO tb_admin_roles (code, name) VALUES ($1, 'Tasks RO') RETURNING id`, role).Scan(&roleID); err != nil {
		t.Fatalf("role: %v", err)
	}
	_, _ = e.s.Pool.Exec(ctx, `INSERT INTO tb_admin_role_permissions (role_id, permission_code) VALUES ($1, 'tasks.read')`, roleID)
	username, password := seedAdminForHTTP(t, e.s)
	var adminID int64
	_ = e.s.Pool.QueryRow(ctx, `SELECT id FROM tb_admins WHERE username=$1`, username).Scan(&adminID)
	_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, adminID)
	_, _ = e.s.Pool.Exec(ctx, `INSERT INTO tb_admin_user_roles (admin_id, role_id) VALUES ($1, $2)`, adminID, roleID)
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE role_id=$1`, roleID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_role_permissions WHERE role_id=$1`, roleID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_roles WHERE id=$1`, roleID)
	})
	ro := &b12Env{s: e.s, router: e.router, username: username, password: password}
	ro.login(t)

	tasks := ro.page(t, "/samelabs/tasks")
	if tasks.Code != 200 || strings.Contains(tasks.Body.String(), `href="/samelabs/memories"`) {
		t.Fatalf("read-only admin tasks page = %d, or nav shows Memories", tasks.Code)
	}
	if rec := ro.page(t, "/samelabs/memories"); rec.Code != http.StatusForbidden {
		t.Fatalf("memories without permission = %d", rec.Code)
	}
	if strings.Contains(ro.page(t, "/samelabs/tasks/"+sd.taskCode).Body.String(), "/close") {
		t.Fatal("read-only admin sees the close form")
	}
	rec := ro.form(t, "/samelabs/tasks/"+sd.taskCode+"/close", url.Values{"reason": {"x"}}, true)
	var st string
	_ = e.s.Pool.QueryRow(ctx, `SELECT status FROM tb_tasks WHERE code=$1`, sd.taskCode).Scan(&st)
	if rec.Code != http.StatusSeeOther || st != "open" || flashOf(rec) == "" {
		t.Fatalf("forbidden close = %d status=%s", rec.Code, st)
	}
}

func TestSamelabsPaymentSettingsForm(t *testing.T) {
	e := newB12Env(t)
	ctx := context.Background()
	key, _ := security.ParseSecretBoxKey(strings.Repeat("7c", 32))
	e.s.secretBox, _ = security.NewSecretBox(key)
	_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_payment_provider_settings`)
	t.Cleanup(func() { _, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_payment_provider_settings`) })

	base := url.Values{
		"enabled": {"1"}, "mode": {"test"}, "success_url": {"https://kungfu.md/owner/credits"},
		"api_key": {"creem_form_key_1111"}, "webhook_secret": {"whsec_form_2222"},
		"pkg_code": {"starter", "", "pro"}, "pkg_product": {"prod_a", "", "prod_b"}, "pkg_credits": {"1000", "", "10.5"},
	}
	e.form(t, "/samelabs/settings/payment", base, true)
	var n int
	_ = e.s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_payment_provider_settings`).Scan(&n)
	if n != 0 {
		t.Fatal("fractional credits were saved")
	}
	base["pkg_credits"] = []string{"1000", "", "9007199254740993"}
	if rec := e.form(t, "/samelabs/settings/payment", base, true); rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d", rec.Code)
	}
	st := e.s.creemSettings(ctx)
	if st == nil || !st.CheckoutEnabled || len(st.Packages) != 2 || st.Packages["pro"].Credits != 9007199254740993 {
		t.Fatalf("saved settings = %+v", st)
	}
	if strings.Contains(e.page(t, "/samelabs/settings/payment").Body.String(), "creem_form_key_1111") {
		t.Fatal("settings page echoes the API key")
	}
}
