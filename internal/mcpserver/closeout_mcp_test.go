package mcpserver

// Thread close-out: memory_get(code, assign) through the REAL tool
// entry — schema-carried param, both doors identical, the plain path
// unchanged, exclusive params rejected.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestCloseoutDeliveredMemoryBothDoors(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	defer srv.Close()
	defer pool.Close()
	ctx := context.Background()
	key, _, idLead := m2RegisterSeeded(t, srv, pool, 0)
	keyOther, _, idOther := m2RegisterSeeded(t, srv, pool, 0)

	if schema := registeredSchema(t, "memory_get"); !strings.Contains(schema, `"assign"`) {
		t.Fatal("memory_get schema must carry the assign param")
	}

	start, isErr, _ := mcpCall(t, srv, key, "thread_start", map[string]any{"subject": "co", "key": true, "idempotency_key": "co-s"})
	if isErr {
		t.Fatalf("start: %v", start)
	}
	code, rawKey := start["thread"].(string), start["key"].(string)
	if j, isErr, _ := mcpCall(t, srv, keyOther, "thread_join", map[string]any{"key": rawKey, "idempotency_key": "co-j"}); isErr {
		t.Fatalf("join: %v", j)
	}
	put, isErr, _ := mcpCall(t, srv, keyOther, "memory_put", map[string]any{
		"title": "report", "tags": []any{"r"}, "content": strings.Repeat("delivered-", 10)})
	if isErr {
		t.Fatalf("memory_put: %v", put)
	}
	mem := put["code"].(string)
	post, isErr, _ := mcpCall(t, srv, key, "thread_post", map[string]any{
		"thread": code, "content": "write it", "ask": []any{},
		"assign": map[string]any{"to": idOther, "requirements": "report"}, "idempotency_key": "co-p"})
	if isErr {
		t.Fatalf("post: %v", post)
	}
	assign := asFloat64(post["assign"])
	if tk, isErr, _ := mcpCall(t, srv, keyOther, "assign_take", map[string]any{
		"assign": assign, "memories": `[{"name":"report","code":"` + mem + `"}]`, "idempotency_key": "co-t"}); isErr {
		t.Fatalf("take+submit: %v", tk)
	}

	// the plain path is unchanged: private stays private to non-authors
	if _, isErr, _ := mcpCall(t, srv, key, "memory_get", map[string]any{"code": mem}); !isErr {
		t.Fatal("plain memory_get must still refuse a private memory")
	}
	// MCP door
	got, isErr, _ := mcpCall(t, srv, key, "memory_get", map[string]any{"code": mem, "assign": assign})
	if isErr || !strings.Contains(asString(got["content"]), "delivered-") || asFloat64(got["revision"]) != 1 {
		t.Fatalf("MCP delivered read: isErr=%v %v", isErr, got)
	}
	// HTTP door
	bot := wo7Bot(t, pool, idLead)
	raw, _ := json.Marshal(map[string]any{"code": mem, "assign": int64(assign)})
	env, status := CallTool(ctx, &deps, "memory_get", bot, raw)
	if status != 200 || env["ok"] != true || !strings.Contains(asString(env["content"]), "delivered-") {
		t.Fatalf("HTTP delivered read: %d %v", status, env)
	}
	// assign and revision are exclusive
	if _, isErr, _ := mcpCall(t, srv, key, "memory_get", map[string]any{"code": mem, "assign": assign, "revision": 1}); !isErr {
		t.Fatal("assign with revision must be rejected")
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// TestThreadToolDescriptionsMatchNextAction: every error code a thread
// tool description labels with a next action must carry the action the
// server actually returns for it (the NOT_YOURS "retry" drift).
func TestThreadToolDescriptionsMatchNextAction(t *testing.T) {
	group := regexp.MustCompile(`([A-Z_]+(?: / [A-Z_]+)*) \((stop|retry|revise|wait)\)`)
	checked := 0
	for _, td := range ToolDefs() {
		if !strings.HasPrefix(td.Name, "thread_") && !strings.HasPrefix(td.Name, "assign_") &&
			!strings.HasPrefix(td.Name, "todo_") {
			continue
		}
		for _, m := range group.FindAllStringSubmatch(td.Description, -1) {
			for _, code := range strings.Split(m[1], " / ") {
				got := threadNextActionFor(code)
				if got == nil {
					continue // shared table code, not thread-mapped
				}
				checked++
				if *got != m[2] {
					t.Errorf("%s: description labels %s as %s, server returns %s", td.Name, code, m[2], *got)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no labelled thread error codes found — the check proves nothing")
	}
}
