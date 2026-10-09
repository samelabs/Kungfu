package mcpserver

// Task 1.2 (WO-32) at the MCP surface: the registry schemas and
// descriptions carry the audience and opportunity contract (the
// transport-visible JSON IS the contract), and the tools round-trip
// a restricted task end to end through CallTool.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTask12SchemaAndDescriptions: the declared surface names what
// it does — task_create's contract schema has the audience property,
// work_list's schema has offered_to_me, and the descriptions state
// the immutability and the opportunity semantics.
func TestTask12SchemaAndDescriptions(t *testing.T) {
	create, ok := Tool("task_create")
	if !ok {
		t.Fatal("task_create missing")
	}
	if !strings.Contains(create.InputSchema, `"audience"`) ||
		!strings.Contains(create.InputSchema, `"restricted"`) {
		t.Fatal("task_create contract schema does not declare the audience property")
	}
	if !strings.Contains(create.Description, "immutable") ||
		!strings.Contains(create.Description, "1–50 named agents") {
		t.Fatal("task_create description does not state audience immutability and the 50 cap")
	}

	update, _ := Tool("task_update")
	if !strings.Contains(update.Description, "audience is immutable") {
		t.Fatal("task_update description does not state audience immutability")
	}

	list, _ := Tool("work_list")
	if !strings.Contains(list.InputSchema, `"offered_to_me"`) {
		t.Fatal("work_list schema does not declare offered_to_me")
	}
	if !strings.Contains(list.Description, `audience ("open" or "restricted")`) {
		t.Fatal("work_list description does not document the audience marker")
	}

	todo, _ := Tool("todo_list")
	if !strings.Contains(todo.Description, "opportunities {tasks, assignments}") ||
		!strings.Contains(todo.Description, "never obligations") {
		t.Fatal("todo_list description does not document the opportunities projection")
	}
}

// TestTask12RestrictedTaskThroughTools: task_create with a restricted
// audience through the registry, then the named agent discovers it
// (work_list, offered_to_me, todo_list opportunities) while an
// outsider gets TASK_NOT_FOUND from work_get.
func TestTask12RestrictedTaskThroughTools(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	_, _, pubID := w11Register(t, pool)
	_, namedName, namedID := w11Register(t, pool)
	_, _, outID := w11Register(t, pool)
	pub := wo7Bot(t, pool, pubID)
	named := wo7Bot(t, pool, namedID)
	outsider := wo7Bot(t, pool, outID)
	ctx := context.Background()

	// strict decoding: an unknown field inside the audience still
	// names the field
	env, status := CallTool(ctx, &deps, "task_create", pub, []byte(`{
		"contract": {"title":"t","requirements":"r","receiver":{"url":"`+okReceiverURL+`"},"price":5,
			"audience":{"type":"restricted","agents":["`+namedName+`"],"extra":1}},
		"budget": 10, "open": true}`))
	if env["ok"] != false {
		t.Fatalf("unknown audience field: %#v, want a rejection", env["error"])
	}
	te0, _ := env["error"].(map[string]any)
	if te0["code"] != "VALIDATION_FAILED" || status != 422 {
		t.Fatalf("unknown audience field: %d %#v, want VALIDATION_FAILED", status, te0)
	}

	args, _ := json.Marshal(map[string]any{
		"contract": map[string]any{
			"title":        "Members only",
			"requirements": "For the named agent alone.",
			"receiver":     map[string]any{"url": okReceiverURL},
			"price":        5,
			"audience":     map[string]any{"type": "restricted", "agents": []string{namedName}},
		},
		"budget": 10,
		"open":   true,
	})
	env, status = CallTool(ctx, &deps, "task_create", pub, args)
	if status != 200 || env["ok"] != true {
		t.Fatalf("task_create: %d %v", status, env["error"])
	}
	aud, _ := env["audience"].(map[string]any)
	if aud["type"] != "restricted" {
		t.Fatalf("task_create audience = %#v", env["audience"])
	}
	code, _ := env["code"].(string)

	// named agent: work_list by code, with the restricted marker
	// (normalize through JSON: CallTool returns raw Go types)
	env, status = CallTool(ctx, &deps, "work_list", named, []byte(`{"code":"`+code+`"}`))
	env = jsonNormalize(t, env)
	if status != 200 || env["ok"] != true {
		t.Fatalf("work_list: %d %v", status, env["error"])
	}
	tasks, _ := env["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("work_list tasks = %#v", env["tasks"])
	}
	row, _ := tasks[0].(map[string]any)
	if row["audience"] != "restricted" {
		t.Fatalf("row audience = %#v", row["audience"])
	}

	// offered_to_me enumerates it
	env, status = CallTool(ctx, &deps, "work_list", named, []byte(`{"offered_to_me":true}`))
	env = jsonNormalize(t, env)
	if status != 200 || env["ok"] != true {
		t.Fatalf("offered work_list: %d %v", status, env["error"])
	}
	tasks, _ = env["tasks"].([]any)
	found := false
	for _, it := range tasks {
		if m, _ := it.(map[string]any); m["code"] == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("offered_to_me missing the restricted task: %#v", env["tasks"])
	}

	// todo_list reports the opportunity with the work_list hint
	env, status = CallTool(ctx, &deps, "todo_list", named, []byte(`{}`))
	env = jsonNormalize(t, env)
	if status != 200 || env["ok"] != true {
		t.Fatalf("todo_list: %d %v", status, env["error"])
	}
	opp, _ := env["opportunities"].(map[string]any)
	if opp == nil || opp["tasks"].(float64) < 1 || opp["assignments"].(float64) < 0 {
		t.Fatalf("todo_list opportunities = %#v", env["opportunities"])
	}
	next, _ := env["next"].([]any)
	if len(next) != 1 {
		t.Fatalf("todo_list next = %#v, want exactly one opportunity hint", env["next"])
	}
	hint, _ := next[0].(map[string]any)
	if hint["tool"] != "work_list" {
		t.Fatalf("hint = %#v, want work_list", hint)
	}

	// the outsider: TASK_NOT_FOUND on work_get, empty offered list
	env, status = CallTool(ctx, &deps, "work_get", outsider, []byte(`{"code":"`+code+`"}`))
	te, _ := env["error"].(map[string]any)
	if status != 404 || env["ok"] != false || te["code"] != "TASK_NOT_FOUND" {
		t.Fatalf("outsider work_get: %d %#v, want TASK_NOT_FOUND", status, env["error"])
	}
	env, status = CallTool(ctx, &deps, "work_list", outsider, []byte(`{"code":"`+code+`","offered_to_me":true}`))
	env = jsonNormalize(t, env)
	if status != 200 || env["ok"] != true {
		t.Fatalf("outsider offered work_list: %d %v", status, env["error"])
	}
	if n, _ := env["total"].(float64); n != 0 {
		t.Fatalf("outsider offered total = %v, want 0", env["total"])
	}
}

// jsonNormalize round-trips a CallTool envelope through JSON so the
// tests assert the wire types a client sees (float64 numbers,
// []any slices).
func jsonNormalize(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}
