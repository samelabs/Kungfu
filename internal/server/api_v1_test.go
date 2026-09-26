package server

// POST /api/v1/<tool> (WO-7a): Bearer authentication, unknown-tool
// envelope, and one end-to-end happy path against the same registry as
// /mcp.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func apiV1Env(t *testing.T) (*Server, string, int64, string) {
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
	// publisher task (async, no receiver → under_review submissions)
	view, err := service.CreateTask(context.Background(), pool, apiV1Publisher(t, pool), apiV1Contract(), 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if _, err := service.OpenTask(context.Background(), pool, apiV1PublisherID(t, pool), code); err != nil {
		t.Fatalf("open: %v", err)
	}

	// agent with a raw key for the Bearer header
	name := "apiv1agent" + time.Now().Format("150405.000000000")
	reg, err := service.Register(context.Background(), pool, name, "passpass123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var agentID int64
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM tb_bots WHERE bot_name = $1`, reg.BotName).Scan(&agentID)
	return s, reg.Key, agentID, code
}

func apiV1Publisher(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	return apiV1SeedBalance(t, pool)
}

var apiV1PubCache struct {
	id   int64
	done bool
}

func apiV1SeedBalance(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	if !apiV1PubCache.done {
		reg, err := service.Register(context.Background(), pool,
			"apiv1pub"+time.Now().Format("150405.000000000"), "passpass123", "127.0.0.1")
		if err != nil {
			t.Fatalf("register publisher: %v", err)
		}
		var id int64
		_ = pool.QueryRow(context.Background(),
			`SELECT id FROM tb_bots WHERE bot_name = $1`, reg.BotName).Scan(&id)
		if _, err := pool.Exec(context.Background(),
			`UPDATE tb_bots SET balance = 10000 WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		apiV1PubCache.id, apiV1PubCache.done = id, true
	}
	return apiV1PubCache.id
}

func apiV1PublisherID(t *testing.T, pool *pg.Pool) int64 { return apiV1SeedBalance(t, pool) }

func apiV1Contract() task.Contract {
	return task.Contract{
		Title: "API v1 task", Objective: "o", Inputs: "i",
		Output: task.Output{Description: "d", Schema: []byte(`{
			"type":"object","properties":{
				"url":{"type":"string"},
				"bullets":{"type":"array","items":{"type":"string"},"minItems":3,"maxItems":3}
			},"required":["url","bullets"]}`)},
		Acceptance: task.Acceptance{Mode: task.ModeAsync,
			ReviewWindow: &[]int64{3600}[0],
			Criteria:     []task.Criterion{{ID: "C1", Kind: task.KindRule, Description: "r"}}},
		Examples: []task.Example{
			{Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), Accepted: true},
		},
		Price: 5,
	}
}

func apiV1Call(t *testing.T, s *Server, key, tool string, args map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(args)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/"+tool, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: non-JSON body (%d): %s", tool, rec.Code, rec.Body.String())
	}
	return rec, env
}

func TestAPIV1Auth(t *testing.T) {
	s, key, _, code := apiV1Env(t)

	// no Bearer → 401 UNAUTHORIZED
	rec, env := apiV1Call(t, s, "", "work_list", map[string]any{})
	if rec.Code != 401 || env["error"].(map[string]any)["code"] != "UNAUTHORIZED" {
		t.Fatalf("no bearer: %d %v", rec.Code, env)
	}
	// wrong Bearer → 401
	rec, env = apiV1Call(t, s, "kf_live_"+strings.Repeat("0", 64), "work_list", map[string]any{})
	if rec.Code != 401 || env["error"].(map[string]any)["code"] != "UNAUTHORIZED" {
		t.Fatalf("bad bearer: %d %v", rec.Code, env)
	}
	// unknown tool → 404 UNKNOWN_TOOL (authenticated)
	rec, env = apiV1Call(t, s, key, "no_such_tool", map[string]any{})
	if rec.Code != 404 || env["error"].(map[string]any)["code"] != "UNKNOWN_TOOL" || env["ok"] != false {
		t.Fatalf("unknown tool: %d %v", rec.Code, env)
	}
	_ = code
}

func TestAPIV1WorkListAndSubmit(t *testing.T) {
	s, key, _, code := apiV1Env(t)

	rec, env := apiV1Call(t, s, key, "work_list", map[string]any{})
	if rec.Code != 200 || env["ok"] != true || env["error"] != nil {
		t.Fatalf("work_list: %d %v", rec.Code, env)
	}
	tasks := env["tasks"].([]any)
	found := false
	for _, it := range tasks {
		if it.(map[string]any)["code"] == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("work_list missing this test's task %s (got %d rows)", code, len(tasks))
	}

	// happy-path submit: async no receiver → under_review + poll/60
	rec, env = apiV1Call(t, s, key, "work_submit", map[string]any{
		"code": code, "request_key": "apiv1-ok",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
	})
	if rec.Code != 200 || env["ok"] != true || env["state"] != task.SubUnderReview ||
		env["next_action"] != "poll" || env["retry_after"].(float64) != 60 {
		t.Fatalf("work_submit: %d %v", rec.Code, env)
	}

	// schema mismatch → 422 + revise
	rec, env = apiV1Call(t, s, key, "work_submit", map[string]any{
		"code": code, "request_key": "apiv1-bad",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"one"}},
	})
	if rec.Code != 422 || env["ok"] != false ||
		env["error"].(map[string]any)["code"] != "SCHEMA_MISMATCH" || env["next_action"] != "revise" {
		t.Fatalf("work_submit bad: %d %v", rec.Code, env)
	}

	// the invariants hold on the underlying task
	tr, _ := repository.FindTaskByCode(context.Background(), s.Pool, code)
	if err := task.CheckInvariants(context.Background(), s.Pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestAPIV1BodyTooLarge(t *testing.T) {
	s, key, _, code := apiV1Env(t)

	// a body one byte over the cap: 413 PAYLOAD_TOO_LARGE, revise
	huge := map[string]any{
		"code": code, "request_key": "toolarge",
		"payload": map[string]any{"url": "https://example.com/a",
			"bullets": []string{strings.Repeat("x", apiV1BodyLimit)}},
	}
	body, _ := json.Marshal(huge)
	if len(body) <= apiV1BodyLimit {
		t.Fatalf("setup: body %d must exceed %d", len(body), apiV1BodyLimit)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/work_submit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("non-JSON: %s", rec.Body.String())
	}
	if env["ok"] != false || env["error"].(map[string]any)["code"] != "PAYLOAD_TOO_LARGE" ||
		env["next_action"] != "revise" {
		t.Fatalf("envelope: %v", env)
	}

	// one byte under the cap reaches the tool (fails later as
	// SCHEMA_MISMATCH, not 413)
	ok := map[string]any{
		"code": code, "request_key": "under",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"one"}},
	}
	b2, _ := json.Marshal(ok)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/work_submit", bytes.NewReader(b2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+key)
	rec2 := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("in-cap body rejected as 413")
	}
}
