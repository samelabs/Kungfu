package mcpserver

// WO-7a integration: every executor tool called through BOTH channels
// (MCP tools/call and the registry dispatch behind POST /api/v1),
// envelopes compared field-by-field after stripping time-class and
// per-call identity fields; work_submit's accepted (via publisher
// verdict) and SCHEMA_MISMATCH outcomes verified on both channels.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func registryEnv(t *testing.T) (*pg.Pool, Deps, *httptest.Server) {
	t.Helper()
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	deps.AgentRefKey = []byte("wo7a-agent-ref-key")
	srv := httptest.NewServer(Handler(deps))
	t.Cleanup(srv.Close)
	return pool, deps, srv
}

// mcpCall runs one tools/call through the shared harness; returns the
// §8.2 structuredContent envelope and isError.
func mcpCall(t *testing.T, srv *httptest.Server, key, tool string, args map[string]any) (map[string]any, bool, int) {
	t.Helper()
	ts := mcpTestServer{srv: srv, client: srv.Client()}
	sc, body := m2CallTool(t, ts, key, tool, args)
	if sc != http.StatusOK {
		t.Fatalf("MCP %s = HTTP %d: %s", tool, sc, body)
	}
	var rpc struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &rpc); err != nil {
		t.Fatalf("MCP %s parse: %v (%s)", tool, err, body)
	}
	if rpc.Result.StructuredContent == nil {
		t.Fatalf("MCP %s: no structuredContent (%s)", tool, body)
	}
	return rpc.Result.StructuredContent, rpc.Result.IsError, sc
}

// httpDispatch runs the same call through the registry dispatch that
// POST /api/v1/<tool> uses (bot re-read by id).
func httpDispatch(t *testing.T, pool *pg.Pool, deps *Deps, botID int64, tool string, args map[string]any) (map[string]any, int) {
	t.Helper()
	var bot = wo7Bot(t, pool, botID)
	raw, _ := json.Marshal(args)
	env, status := CallTool(context.Background(), deps, tool, bot, raw)
	return env, status
}

// volatile or per-channel identity fields, stripped before comparing.
var volatileKeys = map[string]bool{
	"expires_at": true, "deadline": true, "created_at": true,
	"at": true, "review_deadline": true, "submission_id": true,
	"claim_id": true, "report_id": true,
}

func stripVolatile(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			if volatileKeys[k] {
				continue
			}
			out[k] = stripVolatile(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, stripVolatile(item))
		}
		return out
	default:
		return v
	}
}

// numOff normalizes numeric values across in-memory (int) and
// JSON-round-tripped (float64) envelopes.
// mcpCallRelease releases other-agent's claim with that agent's key.
func mcpCallRelease(t *testing.T, srv *httptest.Server, key string, claimID int64) (map[string]any, bool) {
	t.Helper()
	env, isErr, _ := mcpCall(t, srv, key, "work_release", map[string]any{"claim_id": claimID})
	return env, isErr
}

func numOff(v any) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return -1
}

func assertJSONEqual(t *testing.T, what string, a, b map[string]any) {
	t.Helper()
	// one round-trip normalizes struct-field order into map key order
	// so both channels compare canonically
	norm := func(v map[string]any) []byte {
		raw, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out, _ := json.Marshal(stripVolatile(m))
		return out
	}
	ra, rb := norm(a), norm(b)
	if string(ra) != string(rb) {
		t.Fatalf("%s envelopes differ:\nMCP: %s\nHTTP: %s", what, ra, rb)
	}
}

func TestRegistryEveryToolBothChannels(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()

	// publisher opens an async task WITHOUT a receiver: submissions go
	// straight to under_review; the publisher then accepts them via
	// SubmitVerdict (deterministic settled states, no outbound HTTP).
	_, _, pubID := m2RegisterSeeded(t, srv, pool, 10_000)
	code := wo7OpenTask(t, pool, pubID)

	agentKey, _, agentID := m2RegisterSeeded(t, srv, pool, 0)
	mcp := func(tool string, args map[string]any) (map[string]any, bool) {
		env, isErr, _ := mcpCall(t, srv, agentKey, tool, args)
		return env, isErr
	}
	httpCall := func(tool string, args map[string]any) (map[string]any, int) {
		return httpDispatch(t, pool, &deps, agentID, tool, args)
	}

	// 1. work_list
	e1m, i1 := mcp("work_list", map[string]any{})
	e1h, s1 := httpCall("work_list", map[string]any{})
	if i1 || s1 != 200 || e1m["ok"] != true || e1h["ok"] != true {
		t.Fatalf("work_list: mcp isError=%v http=%d", i1, s1)
	}
	assertJSONEqual(t, "work_list", e1m, e1h)

	// 2. work_get
	e2m, i2 := mcp("work_get", map[string]any{"code": code})
	e2h, s2 := httpCall("work_get", map[string]any{"code": code})
	if i2 || s2 != 200 || e2m["status"] != task.TaskOpen {
		t.Fatalf("work_get: %v %d", i2, s2)
	}
	assertJSONEqual(t, "work_get", e2m, e2h)

	// 3. work_harness — missing ref → HARNESS_REF_NOT_FOUND both ways
	e3m, i3 := mcp("work_harness", map[string]any{"code": code, "ref_id": "nope00000000"})
	e3h, s3 := httpCall("work_harness", map[string]any{"code": code, "ref_id": "nope00000000"})
	if !i3 || s3 != 404 {
		t.Fatalf("work_harness: mcp isError=%v http=%d", i3, s3)
	}
	assertJSONEqual(t, "work_harness", e3m, e3h)

	// 4. work_claim — idempotent: same claim on both channels
	e4m, i4 := mcp("work_claim", map[string]any{"code": code})
	e4h, s4 := httpCall("work_claim", map[string]any{"code": code})
	if i4 || s4 != 200 || e4m["next_action"] != "submit" || e4h["next_action"] != "submit" {
		t.Fatalf("work_claim: %+v / %+v", e4m, e4h)
	}
	assertJSONEqual(t, "work_claim", e4m, e4h)
	claimID, _ := e4m["claim_id"].(string) // ids are strings on the wire

	// 5. work_claim_renew (the string id exercises the dual input)
	e5m, i5 := mcp("work_claim_renew", map[string]any{"claim_id": claimID})
	e5h, s5 := httpCall("work_claim_renew", map[string]any{"claim_id": claimID})
	if i5 || s5 != 200 || e5m["next_action"] != "submit" {
		t.Fatalf("work_claim_renew: %+v", e5m)
	}
	assertJSONEqual(t, "work_claim_renew", e5m, e5h)

	// 6. work_submit — both channels land under_review (async, no
	// receiver); the publisher then accepts BOTH via verdicts
	subKey := fmt.Sprintf("par-%d", time.Now().UnixNano())
	e6m, i6 := mcp("work_submit", map[string]any{
		"code": code, "request_key": subKey + "-m",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
	})
	e6h, s6 := httpCall("work_submit", map[string]any{
		"code": code, "request_key": subKey + "-h",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
	})
	if i6 || s6 != 200 || e6m["state"] != task.SubUnderReview || e6m["ok"] != true {
		t.Fatalf("work_submit mcp: %+v isError=%v", e6m, i6)
	}
	if e6h["state"] != task.SubUnderReview || e6h["ok"] != true ||
		e6h["next_action"] != "poll" || numOff(e6h["retry_after"]) != 60 {
		t.Fatalf("work_submit http: %+v (%d)", e6h, s6)
	}
	if e6m["next_action"] != "poll" || numOff(e6m["retry_after"]) != 60 {
		t.Fatalf("work_submit mcp next: %+v", e6m)
	}
	wo7AcceptAll(t, pool, pubID, code)

	// 7. work_status — by code + request_key; now settled
	e7m, i7 := mcp("work_status", map[string]any{"code": code, "request_key": subKey + "-m"})
	e7h, s7 := httpCall("work_status", map[string]any{"code": code, "request_key": subKey + "-m"})
	if i7 || s7 != 200 || e7m["state"] != task.SubSettled || e7m["next_action"] != "done" {
		t.Fatalf("work_status: %+v", e7m)
	}
	if _, has := e7m["events"]; !has {
		t.Fatal("work_status missing events[]")
	}
	if numOff(e7m["paid"]) != 5 {
		t.Fatalf("work_status paid: %+v", e7m)
	}
	assertJSONEqual(t, "work_status", e7m, e7h)

	// 8. work_history
	e8m, i8 := mcp("work_history", map[string]any{"code": code})
	e8h, s8 := httpCall("work_history", map[string]any{"code": code})
	if i8 || s8 != 200 || numOff(e8m["total"]) != 2 {
		t.Fatalf("work_history: %+v", e8m)
	}
	assertJSONEqual(t, "work_history", e8m, e8h)

	// 9. work_report — idempotent across channels
	e9m, i9 := mcp("work_report", map[string]any{"code": code, "reason": "parity probe"})
	e9h, s9 := httpCall("work_report", map[string]any{"code": code, "reason": "parity probe"})
	if i9 || s9 != 200 || e9m["status"] != "open" {
		t.Fatalf("work_report: %+v", e9m)
	}
	assertJSONEqual(t, "work_report", e9m, e9h)

	// 10. work_release — one claim released per channel
	otherKey, _, otherID := m2RegisterSeeded(t, srv, pool, 0)
	c1, err := service.ClaimTask(ctx, pool, otherID, code, time.Now())
	if err != nil {
		t.Fatalf("claim c1: %v", err)
	}
	e10m, i10 := mcpCallRelease(t, srv, otherKey, c1.ClaimID.Int64())
	if i10 || e10m["status"] != task.ClaimReleased {
		t.Fatalf("work_release mcp: %+v", e10m)
	}
	c2, err := service.ClaimTask(ctx, pool, otherID, code, time.Now())
	if err != nil {
		t.Fatalf("claim c2: %v", err)
	}
	e10h, s10 := httpDispatch(t, pool, &deps, otherID, "work_release", map[string]any{"claim_id": c2.ClaimID})
	if s10 != 200 || e10h["status"] != task.ClaimReleased {
		t.Fatalf("work_release http: %+v (%d)", e10h, s10)
	}
	assertJSONEqual(t, "work_release", e10m, e10h)
	_ = otherKey
}

// TestWorkSubmitSchemaMismatchBothChannels: not-accepted is
// isError=true on MCP, 422 on HTTP, same envelope otherwise.
func TestWorkSubmitSchemaMismatchBothChannels(t *testing.T) {
	pool, deps, srv := registryEnv(t)

	_, _, pubID := m2RegisterSeeded(t, srv, pool, 10_000)
	code := wo7OpenTask(t, pool, pubID)
	agentKey, _, agentID := m2RegisterSeeded(t, srv, pool, 0)

	env, isErr, _ := mcpCall(t, srv, agentKey, "work_submit", map[string]any{
		"code": code, "request_key": "bad-schema",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"only-one"}},
	})
	if !isErr || env["ok"] != false {
		t.Fatalf("MCP mismatch not isError: %+v", env)
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "SCHEMA_MISMATCH" || env["next_action"] != "revise" {
		t.Fatalf("MCP mismatch envelope: %+v", env)
	}
	if _, has := errObj["details"].(map[string]any)["errors"]; !has {
		t.Fatalf("details missing errors: %+v", errObj)
	}

	henv, status := httpDispatch(t, pool, &deps, agentID, "work_submit", map[string]any{
		"code": code, "request_key": "bad-schema-h",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"only-one"}},
	})
	if status != 422 || henv["ok"] != false || henv["next_action"] != "revise" {
		t.Fatalf("HTTP mismatch: %d %+v", status, henv)
	}
	if henv["error"].(map[string]any)["code"] != "SCHEMA_MISMATCH" {
		t.Fatalf("HTTP mismatch code: %+v", henv["error"])
	}
}

// TestRegistryMCPAuthAndUnknownTool: the MCP channel authenticates
// before dispatch (registry tools are never public) and unknown tools
// never reach the registry through /api/v1 (404 UNKNOWN_TOOL envelope,
// covered end-to-end in internal/server).
func TestRegistryMCPAuth(t *testing.T) {
	_, _, srv := registryEnv(t)

	// no bearer on a registry tool → 401 from the middleware
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"work_list","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "work_list")
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous registry call = %d, want 401", resp.StatusCode)
	}
}

// -- helpers --

func m2RegisterSeeded(t *testing.T, srv *httptest.Server, pool *pg.Pool, balance int64) (string, string, int64) {
	t.Helper()
	name, key, id := m2Bot(t, pool, srv, fmt.Sprintf("wo7a%d", balance))
	if balance > 0 {
		if _, err := pool.Exec(context.Background(),
			`UPDATE tb_bots SET balance = $1 WHERE id = $2`, balance, id); err != nil {
			t.Fatalf("seed balance: %v", err)
		}
	}
	return key, name, id
}

// wo7Bot loads the bot row for the dispatch path.
func wo7Bot(t *testing.T, pool *pg.Pool, id int64) *model.Bot {
	t.Helper()
	bot, err := repository.FindActiveBotAccountByID(context.Background(), pool, id)
	if err != nil || bot == nil {
		t.Fatalf("bot %d: %v", id, err)
	}
	return bot
}

// wo7OpenTask creates and opens an async no-receiver task (claim not
// required) — submissions go straight to under_review.
func wo7OpenTask(t *testing.T, pool *pg.Pool, publisher int64) string {
	t.Helper()
	c := task.Contract{
		Title: "Parity task", Objective: "o", Inputs: "i",
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
	view, err := service.CreateTask(context.Background(), pool, publisher, c, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if _, err := service.OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

// wo7AcceptAll settles the task's under_review submissions with a
// publisher verdict.
func wo7AcceptAll(t *testing.T, pool *pg.Pool, publisher int64, code string) {
	t.Helper()
	ctx := context.Background()
	rows, _, err := service.ListSubmissionsForPublisher(ctx, pool, publisher, code, task.SubUnderReview, 1, 50, []byte("k"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if _, err := service.SubmitVerdict(ctx, pool, publisher, r.SubmissionID.Int64(),
			[]byte(`{"accepted":true}`), time.Now()); err != nil {
			t.Fatalf("verdict: %v", err)
		}
	}
}
