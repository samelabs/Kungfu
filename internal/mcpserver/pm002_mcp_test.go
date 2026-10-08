package mcpserver

// PM-002 §3: the thread_get assignment surface through the REAL
// tool entry — schema-carried params, digest paging, id expansion,
// invalid params rejected, response shape stable across channels.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPM002ThreadGetAssignmentsSurface(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	defer srv.Close()
	defer pool.Close()
	ctx := context.Background()
	key, _, idLead := m2RegisterSeeded(t, srv, pool, 0)

	// schema discoverability: the registered InputSchema names the params
	schema := registeredSchema(t, "thread_get")
	if !json.Valid([]byte(schema)) {
		t.Fatalf("thread_get schema is not valid JSON")
	}
	for _, param := range []string{"assignments_cursor", "assignments", "entries", "cursor"} {
		if !strings.Contains(schema, param) {
			t.Fatalf("thread_get schema missing param %s", param)
		}
	}

	// create 60 self-assignments through the real MCP path
	startEnv, isErr, _ := mcpCall(t, srv, key, "thread_start", map[string]any{"subject": "pm2", "idempotency_key": "p2-s"})
	if isErr {
		t.Fatalf("start: %v", startEnv)
	}
	code := startEnv["thread"].(string)
	var lastID float64
	for i := 0; i < 60; i++ {
		res, isErr, _ := mcpCall(t, srv, key, "thread_post", map[string]any{
			"thread": code, "content": "bulk", "ask": []any{},
			"assign":          map[string]any{"to": idLead, "requirements": "bulk"},
			"idempotency_key": "p2-b" + string(rune('a'+i%26)) + string(rune('0'+i/26))})
		if isErr {
			t.Fatalf("post %d: %v", i, res)
		}
		lastID = asFloat64(res["assign"])
	}

	// digest page 1 over MCP: 50 light rows, no requirements
	p1, isErr, _ := mcpCall(t, srv, key, "thread_get", map[string]any{"thread": code})
	if isErr {
		t.Fatalf("page1: %v", p1)
	}
	rows := p1["assignments"].([]any)
	if len(rows) != 50 {
		t.Fatalf("page1 = %d", len(rows))
	}
	for _, r := range rows {
		if _, has := r.(map[string]any)["requirements"]; has {
			t.Fatal("digest carries requirements over MCP")
		}
	}
	nc, _ := p1["assignments_next_cursor"].(string)
	if nc == "" {
		t.Fatal("cursor missing")
	}
	// page 2 via the schema param
	p2, isErr, _ := mcpCall(t, srv, key, "thread_get", map[string]any{"thread": code, "assignments_cursor": nc})
	if isErr {
		t.Fatalf("page2: %v", p2)
	}
	if n := anyLen(p2["assignments"]); n != 10 {
		t.Fatalf("page2 = %d, want 10", n)
	}
	// expansion by id carries requirements
	ex, isErr, _ := mcpCall(t, srv, key, "thread_get", map[string]any{"thread": code, "assignments": []any{lastID}})
	if isErr {
		t.Fatalf("expand: %v", ex)
	}
	rows = ex["assignments_expanded"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["requirements"] != "bulk" {
		t.Fatalf("expand = %v", rows)
	}
	// invalid params rejected
	if _, isErr, _ := mcpCall(t, srv, key, "thread_get", map[string]any{"thread": code, "assignments_cursor": "not-a-cursor"}); !isErr {
		t.Fatal("invalid assignments_cursor must be rejected")
	}
	if _, isErr, _ := mcpCall(t, srv, key, "thread_get", map[string]any{"thread": code, "assignments": makeIds(51)}); !isErr {
		t.Fatal("51 expansion ids must be rejected")
	}
	// same surface over the HTTP dispatch door
	bot := wo7Bot(t, pool, idLead)
	raw, _ := json.Marshal(map[string]any{"thread": code, "assignments": []int64{int64(lastID)}})
	env, status := CallTool(ctx, &deps, "thread_get", bot, raw)
	if status != 200 || env["ok"] != true {
		t.Fatalf("http door: %d %v", status, env)
	}
	hrowsAny := env["assignments_expanded"].([]any)
	hrows := make([]map[string]any, 0, len(hrowsAny))
	for _, r := range hrowsAny {
		hrows = append(hrows, r.(map[string]any))
	}
	if len(hrows) != 1 || hrows[0]["requirements"] != "bulk" {
		t.Fatalf("http expand = %v", hrows)
	}
}

func makeIds(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, i+1)
	}
	return out
}

func registeredSchema(t *testing.T, name string) string {
	t.Helper()
	for _, td := range ToolDefs() {
		if td.Name == name {
			return td.InputSchema
		}
	}
	t.Fatalf("tool %s not registered", name)
	return ""
}

// PM-003: the mine-open filter through both real doors, with the
// invalid-type rejection the schema promises.
func TestPM003MineOpenBothDoors(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	defer srv.Close()
	defer pool.Close()
	ctx := context.Background()
	key, _, idLead := m2RegisterSeeded(t, srv, pool, 0)
	keyOther, _, idOther := m2RegisterSeeded(t, srv, pool, 0)

	start, isErr, _ := mcpCall(t, srv, key, "thread_start", map[string]any{"subject": "pm3", "key": true, "idempotency_key": "p3-s"})
	if isErr {
		t.Fatalf("start: %v", start)
	}
	code, rawKey := start["thread"].(string), start["key"].(string)
	// the other member joins with its own key (a speech member, so it
	// can be the assignee of noise); the lead keeps one self-invite
	ojoin, isErr, _ := mcpCall(t, srv, keyOther, "thread_join", map[string]any{"key": rawKey, "idempotency_key": "p3-oj"})
	if isErr {
		t.Fatalf("join: %v", ojoin)
	}
	var mine float64
	for i := 0; i <= 55; i++ {
		to := idLead
		if i < 55 {
			to = idOther
		}
		res, isErr, _ := mcpCall(t, srv, key, "thread_post", map[string]any{
			"thread": code, "content": "x", "ask": []any{},
			"assign":          map[string]any{"to": to, "requirements": "x"},
			"idempotency_key": fmt.Sprintf("p3-m%02d", i)})
		if isErr {
			t.Fatalf("post %d: %v", i, res)
		}
		if i == 55 {
			mine = asFloat64(res["assign"])
		}
	}
	// MCP door: filtered page = exactly the self-invite
	v, isErr, _ := mcpCall(t, srv, key, "thread_get",
		map[string]any{"thread": code, "assignments_mine_open": true})
	if isErr {
		t.Fatalf("filtered: %v", v)
	}
	rows := v["assignments"].([]any)
	if len(rows) != 1 || asFloat64(rows[0].(map[string]any)["assign"]) != mine {
		t.Fatalf("MCP filtered = %v, want exactly %v", rows, mine)
	}
	// HTTP door: identical result
	bot := wo7Bot(t, pool, idLead)
	raw, _ := json.Marshal(map[string]any{"thread": code, "assignments_mine_open": true})
	env, status := CallTool(ctx, &deps, "thread_get", bot, raw)
	if status != 200 || env["ok"] != true {
		t.Fatalf("http door: %d %v", status, env)
	}
	hrowsAny, _ := env["assignments"].([]any)
	if hrowsAny == nil {
		if typed, ok := env["assignments"].([]map[string]any); ok {
			for _, r := range typed {
				hrowsAny = append(hrowsAny, r)
			}
		}
	}
	if len(hrowsAny) != 1 || asFloat64(hrowsAny[0].(map[string]any)["assign"]) != mine {
		t.Fatalf("HTTP filtered = %v", hrowsAny)
	}
	// schema-typed param: a string where a boolean belongs is rejected
	if _, isErr, _ := mcpCall(t, srv, key, "thread_get",
		map[string]any{"thread": code, "assignments_mine_open": "yes"}); !isErr {
		t.Fatal("non-boolean assignments_mine_open must be rejected")
	}
}
