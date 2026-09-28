package server

// WO-8b tests: the platform task console (/api/samelabs + /samelabs
// pages) with its permission gates and audit rows, the homepage task
// board, and the owner tool 413 parity fix.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/mcpserver"
)

// govBot seeds one account and returns its id.
func govBot(t *testing.T, e *adminEnv) int64 {
	t.Helper()
	name := fmt.Sprintf("gov_%d", time.Now().UnixNano())
	h := sha256.Sum256([]byte(name))
	var id int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		 VALUES ($1, $2, 'gv01', 'x', 7777) RETURNING id`, name, h[:]).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task_report WHERE reporter_id = $1`, id)
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task WHERE publisher_id = $1`, id)
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, id)
	})
	return id
}

// govTask seeds one task row and returns its id.
func govTask(t *testing.T, e *adminEnv, publisher int64, code, status string, budget int64) int64 {
	t.Helper()
	var id int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_task (code, publisher_id, status, budget_locked) VALUES ($1, $2, $3, $4) RETURNING id`,
		code, publisher, status, budget).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task_report WHERE task_id = $1`, id)
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task WHERE id = $1`, id)
	})
	return id
}

// govTaskVersion seeds the version snapshot behind an open task and
// points the task's version column at it (OpenTask does both; the
// board and detail queries join on tb_task.version).
func govTaskVersion(t *testing.T, e *adminEnv, taskID int64, title string, price int64) {
	t.Helper()
	contract := fmt.Sprintf(`{"title":%q,"price":%d}`, title, price)
	if _, err := e.s.Pool.Exec(context.Background(),
		`INSERT INTO tb_task_version (task_id, version, contract, harness) VALUES ($1, 1, $2, '[]'::jsonb)`,
		taskID, contract); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if _, err := e.s.Pool.Exec(context.Background(),
		`UPDATE tb_task SET version = 1 WHERE id = $1`, taskID); err != nil {
		t.Fatalf("set version: %v", err)
	}
}

// govReport seeds one open report.
func govReport(t *testing.T, e *adminEnv, taskID, reporter int64, reason string) int64 {
	t.Helper()
	var id int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_task_report (task_id, reporter_id, reason) VALUES ($1, $2, $3) RETURNING id`,
		taskID, reporter, reason).Scan(&id); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task_report WHERE id = $1`, id)
	})
	return id
}

// scopedAdminEnv creates a second admin whose role holds exactly the
// given permission codes.
func scopedAdminEnv(t *testing.T, e *adminEnv, perms ...string) *adminEnv {
	t.Helper()
	role := fmt.Sprintf("govr_%d", time.Now().UnixNano()%1_000_000)
	var roleID int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_admin_roles (code, name) VALUES ($1, 'Gov scoped') RETURNING id`, role).Scan(&roleID); err != nil {
		t.Fatalf("role: %v", err)
	}
	for _, p := range perms {
		if _, err := e.s.Pool.Exec(context.Background(),
			`INSERT INTO tb_admin_role_permissions (role_id, permission_code) VALUES ($1, $2)`, roleID, p); err != nil {
			t.Fatalf("perm %s: %v", p, err)
		}
	}
	username, password := seedAdminForHTTP(t, e.s)
	var adminID int64
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT id FROM tb_admins WHERE username=$1`, username).Scan(&adminID)
	_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, adminID)
	_, _ = e.s.Pool.Exec(context.Background(), `INSERT INTO tb_admin_user_roles (admin_id, role_id) VALUES ($1, $2)`, adminID, roleID)
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, adminID)
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_role_permissions WHERE role_id=$1`, roleID)
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_roles WHERE id=$1`, roleID)
	})
	scoped := &adminEnv{s: e.s, router: e.router, username: username, password: password}
	scoped.login(t)
	return scoped
}

func govAuditCount(t *testing.T, e *adminEnv, action, targetID string) int64 {
	t.Helper()
	var n int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action = $1 AND target_id = $2`,
		action, targetID).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func TestAdminTasksAPIPermissionGate(t *testing.T) {
	e := newAdminEnv(t)
	publisher := govBot(t, e)
	code := fmt.Sprintf("gt%d", time.Now().UnixNano()%1_000_000_000)
	govTask(t, e, publisher, code, "open", 1000)

	// superadmin (wildcard): reads 200
	if rec := e.do(t, "GET", "/api/samelabs/tasks", "", false); rec.Code != 200 {
		t.Fatalf("superadmin list = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "GET", "/api/samelabs/reports", "", false); rec.Code != 200 {
		t.Fatalf("superadmin reports = %d %s", rec.Code, rec.Body.String())
	}

	// tasks.read only: task reads 200, close 403, reports 403
	ro := scopedAdminEnv(t, e, "tasks.read")
	if rec := ro.do(t, "GET", "/api/samelabs/tasks?status=open", "", false); rec.Code != 200 {
		t.Fatalf("tasks.read list = %d", rec.Code)
	}
	if rec := ro.mutateJSON(t, "POST", "/api/samelabs/tasks/"+code+"/close", `{"reason":"x"}`); rec.Code != 403 {
		t.Fatalf("tasks.read close = %d, want 403", rec.Code)
	}
	if rec := ro.do(t, "GET", "/api/samelabs/reports", "", false); rec.Code != 403 {
		t.Fatalf("tasks.read reports = %d, want 403", rec.Code)
	}

	// unauthenticated: 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/samelabs/tasks", nil)
	e.router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("anonymous list = %d, want 401", rec.Code)
	}
}

func TestAdminTasksAPIPlatformCloseWritesAudit(t *testing.T) {
	e := newAdminEnv(t)
	publisher := govBot(t, e)
	reporter := govBot(t, e)
	code := fmt.Sprintf("gc%d", time.Now().UnixNano()%1_000_000_000)
	taskID := govTask(t, e, publisher, code, "open", 1000)
	reportID := govReport(t, e, taskID, reporter, "boundary violation")

	// detail before close (tasks.read)
	rec := e.do(t, "GET", "/api/samelabs/tasks/"+code, "", false)
	if rec.Code != 200 {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Data struct {
			Task struct {
				Status string `json:"status"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.Data.Task.Status != "open" {
		t.Fatalf("detail status = %q (err %v) %s", detail.Data.Task.Status, err, rec.Body.String())
	}

	// close with a bad reason → 4xx and nothing changed
	if rec := e.mutateJSON(t, "POST", "/api/samelabs/tasks/"+code+"/close", `{"reason":"  "}`); rec.Code < 400 {
		t.Fatalf("blank reason accepted: %d", rec.Code)
	}

	rec = e.mutateJSON(t, "POST", "/api/samelabs/tasks/"+code+"/close", `{"reason":"platform decision"}`)
	if rec.Code != 200 {
		t.Fatalf("close = %d %s", rec.Code, rec.Body.String())
	}
	var status, reason string
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT status, closed_reason FROM tb_task WHERE id = $1`, taskID).Scan(&status, &reason); err != nil {
		t.Fatalf("task: %v", err)
	}
	if status != "closed" || reason != "platform decision" {
		t.Fatalf("after close: %s / %q", status, reason)
	}
	// the open report of the closed task turned actioned
	var reportStatus string
	_ = e.s.Pool.QueryRow(context.Background(),
		`SELECT status FROM tb_task_report WHERE id = $1`, reportID).Scan(&reportStatus)
	if reportStatus != "actioned" {
		t.Fatalf("report status = %s, want actioned", reportStatus)
	}
	if n := govAuditCount(t, e, "task.platform_close", code); n != 1 {
		t.Fatalf("platform_close audit rows = %d, want 1", n)
	}
	// closing again → INVALID_STATE mapped to its HTTP status
	if rec := e.mutateJSON(t, "POST", "/api/samelabs/tasks/"+code+"/close", `{"reason":"again"}`); rec.Code != 409 {
		t.Fatalf("second close = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminReportsAPIDismissAndClose(t *testing.T) {
	e := newAdminEnv(t)
	publisher := govBot(t, e)
	reporter := govBot(t, e)
	code := fmt.Sprintf("gr%d", time.Now().UnixNano()%1_000_000_000)
	taskID := govTask(t, e, publisher, code, "open", 1000)
	reportA := govReport(t, e, taskID, reporter, "malicious rejection")
	reportB := govReport(t, e, taskID, reporter, "boundary violation")

	// the queue lists the open reports by default
	rec := e.do(t, "GET", "/api/samelabs/reports", "", false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "malicious rejection") {
		t.Fatalf("queue = %d %s", rec.Code, rec.Body.String())
	}

	// dismiss A
	if rec := e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/reports/%d/dismiss", reportA), `{}`); rec.Code != 200 {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body.String())
	}
	var status string
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT status FROM tb_task_report WHERE id = $1`, reportA).Scan(&status)
	if status != "dismissed" {
		t.Fatalf("A status = %s, want dismissed", status)
	}
	if n := govAuditCount(t, e, "report.dismiss", fmt.Sprint(reportA)); n != 1 {
		t.Fatalf("dismiss audit rows = %d, want 1", n)
	}

	// close via B: the task closes and B turns actioned
	if rec := e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/reports/%d/close", reportB), `{}`); rec.Code != 200 {
		t.Fatalf("resolve close = %d %s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT status FROM tb_task WHERE id = $1`, taskID).Scan(&taskStatus)
	if taskStatus != "closed" {
		t.Fatalf("task status = %s, want closed", taskStatus)
	}
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT status FROM tb_task_report WHERE id = $1`, reportB).Scan(&status)
	if status != "actioned" {
		t.Fatalf("B status = %s, want actioned", status)
	}
	if n := govAuditCount(t, e, "report.close", fmt.Sprint(reportB)); n != 1 {
		t.Fatalf("report.close audit rows = %d, want 1", n)
	}
	if n := govAuditCount(t, e, "task.platform_close", code); n != 1 {
		t.Fatalf("platform_close audit rows = %d, want 1", n)
	}

	// the queue now shows no open report
	rec = e.do(t, "GET", "/api/samelabs/reports", "", false)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "malicious rejection") {
		t.Fatalf("queue after resolution = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSamelabsTaskPagesRenderAndGate(t *testing.T) {
	e := newAdminEnv(t)
	publisher := govBot(t, e)
	code := fmt.Sprintf("gp%d", time.Now().UnixNano()%1_000_000_000)
	taskID := govTask(t, e, publisher, code, "open", 3000)
	govTaskVersion(t, e, taskID, "Console board task", 5)
	govReport(t, e, taskID, publisher, "page queue reason")

	// superadmin pages render with the seeded data
	page := e.page(t, "/samelabs/tasks")
	if page.Code != 200 || !strings.Contains(page.Body.String(), code) {
		t.Fatalf("tasks page = %d", page.Code)
	}
	detail := e.page(t, "/samelabs/tasks/"+code)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "Console board task") ||
		!strings.Contains(detail.Body.String(), "Platform close") {
		t.Fatalf("task detail = %d", detail.Code)
	}
	reports := e.page(t, "/samelabs/reports")
	if reports.Code != 200 || !strings.Contains(reports.Body.String(), "page queue reason") {
		t.Fatalf("reports page = %d", reports.Code)
	}
	dashboard := e.page(t, "/samelabs")
	if dashboard.Code != 200 || !strings.Contains(dashboard.Body.String(), "Draft / paused tasks") {
		t.Fatalf("dashboard counts = %d", dashboard.Code)
	}

	// the close form works end to end
	rec := e.form(t, "/samelabs/tasks/"+code+"/close", map[string][]string{"reason": {"page close reason"}}, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form close = %d", rec.Code)
	}
	var status string
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT status FROM tb_task WHERE id = $1`, taskID).Scan(&status)
	if status != "closed" {
		t.Fatalf("form close status = %s", status)
	}
	if n := govAuditCount(t, e, "task.platform_close", code); n != 1 {
		t.Fatalf("form close audit rows = %d", n)
	}

	// a tasks.read-only admin: list renders, reports page is 403, the
	// close form is hidden and its POST is rejected
	ro := scopedAdminEnv(t, e, "tasks.read")
	if p := ro.page(t, "/samelabs/tasks"); p.Code != 200 || strings.Contains(p.Body.String(), `href="/samelabs/reports"`) {
		t.Fatalf("read-only tasks page = %d", p.Code)
	}
	if p := ro.page(t, "/samelabs/reports"); p.Code != http.StatusForbidden {
		t.Fatalf("read-only reports page = %d, want 403", p.Code)
	}
	if strings.Contains(ro.page(t, "/samelabs/tasks/"+code).Body.String(), "Platform close") {
		t.Fatal("read-only admin sees the close form")
	}
	rec = ro.form(t, "/samelabs/tasks/"+code+"/close", map[string][]string{"reason": {"nope"}}, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("forbidden form = %d", rec.Code)
	}
	var reason string
	_ = e.s.Pool.QueryRow(context.Background(), `SELECT closed_reason FROM tb_task WHERE id = $1`, taskID).Scan(&reason)
	if reason != "page close reason" { // unchanged by the forbidden attempt
		t.Fatalf("task changed by forbidden admin: %q", reason)
	}
}

func TestHomepageTaskBoardShowsOpenTasksOnly(t *testing.T) {
	e := newAdminEnv(t)
	publisher := govBot(t, e)

	withBoard := func(code, title string, price, budget int64, status string) {
		t.Helper()
		id := govTask(t, e, publisher, code, status, budget)
		govTaskVersion(t, e, id, title, price)
	}
	shown := fmt.Sprintf("gb%d", time.Now().UnixNano()%1_000_000_000)
	noSlots := fmt.Sprintf("gz%d", time.Now().UnixNano()%1_000_000_000)
	draft := fmt.Sprintf("gd%d", time.Now().UnixNano()%1_000_000_000)
	closed := fmt.Sprintf("gx%d", time.Now().UnixNano()%1_000_000_000)
	withBoard(shown, "Alpha board task", 5, 1000, "open")         // slots 200 → on the board
	withBoard(noSlots, "Exhausted board task", 1000, 500, "open") // available 500 < price → no slot
	withBoard(closed, "Closed board task", 5, 1000, "closed")     // not open
	withBoard(draft, "Draft board task", 5, 1000, "draft")        // no version row, not open

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("home = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Alpha board task") || !strings.Contains(body, shown) {
		t.Fatal("open task missing from the board")
	}
	for _, banned := range []string{"Exhausted board task", noSlots, "Closed board task", closed, "Draft board task", draft} {
		if strings.Contains(body, banned) {
			t.Fatalf("%s must not be on the board", banned)
		}
	}
}

func TestOwnerToolBodyTooLarge413(t *testing.T) {
	s, pool, name, _ := ownerConsoleEnv(t)
	cookie := ocSessionCookie(t, s, pool, name)

	rec, env := ocCall(t, s, cookie, "task_create", map[string]any{
		"contract": map[string]any{"title": strings.Repeat("a", mcpserver.MaxRequestBodyBytes)},
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit body = %d, want 413", rec.Code)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("envelope = %v, want PAYLOAD_TOO_LARGE", env)
	}
	if env["next_action"] != "revise" {
		t.Fatalf("next_action = %v, want revise", env["next_action"])
	}
}
