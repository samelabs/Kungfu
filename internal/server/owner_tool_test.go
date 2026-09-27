package server

// WO-8a tests: the /api/owner/tool bridge (auth, CSRF gate, publisher
// allowlist, envelope parity), the four console pages, and the
// end-to-end publisher journey ending with task.CheckInvariants.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/mcpserver"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

// ownerConsoleEnv: a pool + server + session bot with balance.
func ownerConsoleEnv(t *testing.T) (*Server, *pg.Pool, string, int64) {
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
	// a bot with balance and an owner session cookie
	var botID int64
	name := "ocowner" + time.Now().Format("150405.000000000")
	digest := auth.HashAgentKey("kf_live_" + strings.Repeat("ab", 32) + name)
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'oc01', 'x', 100000) RETURNING id`, name, digest).Scan(&botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return s, pool, name, botID
}

// ocCall posts one console tool call with the given cookie.
func ocCall(t *testing.T, s *Server, cookie *http.Cookie, tool string, args map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(args)
	req := httptest.NewRequest(http.MethodPost, "/api/owner/tool/"+tool, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	var env map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
	}
	return rec, env
}

func TestOwnerToolAuthAndCSRF(t *testing.T) {
	s, pool, name, botID := ownerConsoleEnv(t)

	// no session → 401 UNAUTHORIZED envelope
	rec, env := ocCall(t, s, nil, "task_list", map[string]any{})
	if rec.Code != 401 || env["error"].(map[string]any)["code"] != "UNAUTHORIZED" {
		t.Fatalf("no session: %d %v", rec.Code, env)
	}

	// session cookie: sign in through the owner session service
	cookie := ocSessionCookie(t, s, pool, name)

	// no Content-Type (form-encoded) → the ownerMutation gate rejects 415
	req := httptest.NewRequest(http.MethodPost, "/api/owner/tool/task_list",
		strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form-encoded mutation = %d, want 415", rec.Code)
	}

	// non-publisher tool → 404 UNKNOWN_TOOL (even a real registry tool)
	for _, banned := range []string{"work_list", "account_register", "memory_list", "no_such_tool"} {
		rec, env = ocCall(t, s, cookie, banned, map[string]any{})
		if rec.Code != 404 || env["error"].(map[string]any)["code"] != "UNKNOWN_TOOL" {
			t.Fatalf("%s: %d %v, want 404 UNKNOWN_TOOL", banned, rec.Code, env)
		}
	}
	_ = botID
}

// TestOwnerToolLifecycleAndParity: task_create → task_open → task_list
// through the console; the returned envelopes equal CallTool's for the
// same input (same code path, verified on create).
func TestOwnerToolLifecycleAndParity(t *testing.T) {
	s, pool, name, botID := ownerConsoleEnv(t)
	ctx := context.Background()
	cookie := ocSessionCookie(t, s, pool, name)

	contract := ocAsyncContract()
	// route first (builds the router, wiring mcpDepsValue), then the
	// same call through the registry directly for parity
	routeRec, routeEnv := ocCall(t, s, cookie, "task_create", map[string]any{"contract": contract, "budget": 2000})
	deps := s.mcpDeps()
	botRow, err := repository.FindActiveBotAccountByID(ctx, pool, botID)
	if err != nil || botRow == nil {
		t.Fatalf("bot lookup: %v", err)
	}
	directEnv, directStatus := mcpserver.CallTool(ctx, deps, "task_create", botRow,
		mustJSON(t, map[string]any{"contract": contract, "budget": 2000}))
	if routeRec.Code != directStatus || routeRec.Code != 200 {
		t.Fatalf("create: route %d vs direct %d", routeRec.Code, directStatus)
	}
	// codes differ (random); compare the shared shape
	if routeEnv["ok"] != directEnv["ok"] || routeEnv["status"] != "draft" ||
		routeEnv["budget_locked"].(float64) != 2000 || routeEnv["next_action"] != nil {
		t.Fatalf("create envelope: %v vs %v", routeEnv, directEnv)
	}
	code, _ := routeEnv["code"].(string)

	if _, env := ocCall(t, s, cookie, "task_open", map[string]any{"code": code}); env["ok"] != true || env["status"] != "open" {
		t.Fatalf("open: %v", env)
	}
	rec, env := ocCall(t, s, cookie, "task_list", map[string]any{})
	if rec.Code != 200 || env["ok"] != true {
		t.Fatalf("list: %d %v", rec.Code, env)
	}
	found := false
	for _, it := range env["tasks"].([]any) {
		if it.(map[string]any)["code"] == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("task_list missing %s", code)
	}
	if err := task.CheckInvariants(ctx, pool, ocTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestOwnerTaskPages(t *testing.T) {
	s, pool, name, _ := ownerConsoleEnv(t)
	cookie := ocSessionCookie(t, s, pool, name)

	for _, path := range []string{"/owner/tasks", "/owner/tasks/new", "/owner/tasks/any0123456"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.buildRouter().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "tasks-console.js") || !strings.Contains(body, "render-tasks-console.js") {
			t.Fatalf("%s missing console scripts", path)
		}
	}

	// unauthenticated /owner/tasks keeps the existing owner-page
	// behavior (renders the guest shell; the JS layer redirects) — the
	// API under it is the auth boundary and is tested above.
	req := httptest.NewRequest(http.MethodGet, "/owner/tasks", nil)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/owner/tasks not routed")
	}
}

// TestOwnerConsoleEndToEnd: create(async) → open → another bot submits
// via /api/v1 work_submit → console sees under_review → task_verdict
// accept → settled; CheckInvariants at the end.
// parseWireID reads a wire id (string) as int64.
func parseWireID(t *testing.T, v any) int64 {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("wire id %v is not a string", v)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("wire id %q: %v", s, err)
	}
	return n
}

func TestOwnerConsoleEndToEnd(t *testing.T) {
	s, pool, name, botID := ownerConsoleEnv(t)
	ctx := context.Background()
	cookie := ocSessionCookie(t, s, pool, name)

	// create + open an async no-receiver task
	_, env := ocCall(t, s, cookie, "task_create", map[string]any{"contract": ocAsyncContract(), "budget": 2000})
	if env["ok"] != true {
		t.Fatalf("create: %v", env)
	}
	code := env["code"].(string)
	if _, env = ocCall(t, s, cookie, "task_open", map[string]any{"code": code}); env["ok"] != true {
		t.Fatalf("open: %v", env)
	}

	// the agent registers through /api/v1 and submits
	agentKey, agentID := ocRegisterAgent(t, s)
	agentBody, _ := json.Marshal(map[string]any{
		"code": code, "request_key": "e2e-1",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/work_submit", bytes.NewReader(agentBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	var subEnv map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &subEnv)
	if rec.Code != 200 || subEnv["state"] != "under_review" {
		t.Fatalf("agent submit: %d %v", rec.Code, subEnv)
	}
	subID := parseWireID(t, subEnv["submission_id"])

	// console queue shows the under_review row with agent_ref + payload
	_, env = ocCall(t, s, cookie, "task_submissions", map[string]any{"code": code, "state": "under_review"})
	if env["ok"] != true || env["total"].(float64) != 1 {
		t.Fatalf("submissions: %v", env)
	}
	rows := env["submissions"].([]any)
	row := rows[0].(map[string]any)
	if row["submission_id"].(string) != fmt.Sprint(subID) || row["agent_ref"] == "" || row["payload"] == nil {
		t.Fatalf("submission row: %v", row)
	}

	// accept → settled + paid
	_, env = ocCall(t, s, cookie, "task_verdict", map[string]any{
		"submission_id": subID, "verdict": map[string]any{"accepted": true}})
	if env["ok"] != true || env["state"] != "settled" || env["paid"].(float64) != 5 {
		t.Fatalf("verdict: %v", env)
	}

	// agent was paid
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, agentID).Scan(&balance)
	if want := int64(service.SignupGrant) + 5; balance != want {
		t.Fatalf("agent balance = %d, want signup grant %d + 5", balance, service.SignupGrant)
	}
	if err := task.CheckInvariants(ctx, pool, ocTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
	_ = botID
}

// ---- helpers ----

// ocSessionCookie seeds a direct owner session cookie (the standard
// test pattern: setOwnerCookie + parseSetCookie).
func ocSessionCookie(t *testing.T, s *Server, pool *pg.Pool, name string) *http.Cookie {
	t.Helper()
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID); err != nil {
		t.Fatalf("bot lookup: %v", err)
	}
	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	for _, c := range w.Result().Cookies() {
		return c
	}
	t.Fatal("no owner cookie set")
	return nil
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ocTaskID(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	tr, err := repository.FindTaskByCode(context.Background(), pool, code)
	if err != nil || tr == nil {
		t.Fatalf("task %s: %v", code, err)
	}
	return tr.ID
}

// ocAsyncContract: the work order's valid async sample (no receiver).
func ocAsyncContract() map[string]any {
	return map[string]any{
		"title":     "Summarize a page",
		"objective": "A 3-bullet summary of the given page, for a newsletter.",
		"inputs":    "A public URL fetched by the executor.",
		"output": map[string]any{
			"description": "One JSON object with the bullets.",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string"},
					"bullets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 3, "maxItems": 3},
				},
				"required": []string{"url", "bullets"},
			},
		},
		"acceptance": map[string]any{
			"mode": "async", "review_window": 3600,
			"criteria": []map[string]any{{"id": "C1", "kind": "rule", "description": "three sentences"}},
		},
		"examples": []map[string]any{
			{"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}}, "accepted": true},
		},
		"price": 5,
	}
}

// ocRegisterAgent registers via /api/v1 and returns the raw key + id.
func ocRegisterAgent(t *testing.T, s *Server) (string, int64) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name": fmt.Sprintf("ocagent%d", time.Now().UnixNano()), "password": "passpass123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/account_register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != 200 {
		t.Fatalf("register: %d %v (%v)", rec.Code, env, err)
	}
	key := env["api_key"].(string)
	bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), s.Pool, auth.HashAgentKey(key))
	if err != nil || bot == nil {
		t.Fatalf("agent lookup: %v", err)
	}
	return key, bot.ID
}
