package mcpserver

// D8: the room face over BOTH channels (MCP and HTTP dispatch hit
// the same service transitions — one implementation, two doors), the
// A20 end-to-end orchestration over real tools, and the explicit
// L6 structure/content separation assertion (A19).

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"kungfu.md/internal/pg"
)

func bothChannelsThread(t *testing.T) (*pg.Pool, *httptest.Server,
	func(string, map[string]any) (map[string]any, bool),
	func(int64, string, map[string]any) (map[string]any, int), int64) {
	t.Helper()
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()
	keyA, _, _ := m2RegisterSeeded(t, srv, pool, 0)
	_, _, idB := m2RegisterSeeded(t, srv, pool, 0)

	mcp := func(tool string, args map[string]any) (map[string]any, bool) {
		env, isErr, _ := mcpCall(t, srv, keyA, tool, args)
		return env, isErr
	}
	http := func(botID int64, tool string, args map[string]any) (map[string]any, int) {
		bot := wo7Bot(t, pool, botID)
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		return env, status
	}
	return pool, srv, mcp, http, idB
}

// TestThreadToolsBothChannels: the same room flows through MCP (A)
// and HTTP dispatch (B) with identical outcomes — the channels are
// two doors to one service (D8 equivalence).
func TestThreadToolsBothChannels(t *testing.T) {
	pool, srv, mcp, http, idB := bothChannelsThread(t)
	defer srv.Close()
	defer pool.Close()

	// A opens the room over MCP; B joins over HTTP with the key
	envStart, isErr := mcp("thread_start", map[string]any{"subject": "both", "key": true, "idempotency_key": "bc-s"})
	if isErr {
		t.Fatalf("start: %v", envStart)
	}
	code := envStart["thread"].(string)
	rawKey := envStart["key"].(string)
	envJoin, status := http(idB, "thread_join", map[string]any{"key": rawKey, "idempotency_key": "bc-j"})
	if status != 200 || envJoin["ok"] != true {
		t.Fatalf("join: %d %v", status, envJoin)
	}

	// B posts over HTTP; A reads over MCP — same facts either door
	envPost, status := http(idB, "thread_post", map[string]any{
		"thread": code, "content": "hello from the HTTP side", "idempotency_key": "bc-p"})
	if status != 200 || envPost["ok"] != true {
		t.Fatalf("post: %d %v", status, envPost)
	}
	envGet, isErr := mcp("thread_get", map[string]any{"thread": code})
	if isErr {
		t.Fatalf("get: %v", envGet)
	}
	timeline := envGet["timeline"].([]any)
	if len(timeline) != 1 {
		t.Fatalf("timeline = %v", timeline)
	}
	item := timeline[0].(map[string]any)
	if item["summary"] != "hello from the HTTP side" {
		t.Fatalf("entry summary = %v", item["summary"])
	}

	// A19 (L6): structure fields and participant content are
	// separable — content appears ONLY in declared payload fields
	for _, k := range []string{"thread", "role", "members", "key", "next"} {
		if _, ok := envGet[k]; !ok {
			t.Fatalf("structure field %s missing from thread_get", k)
		}
	}
	entryExpanded, isErr := mcp("thread_get", map[string]any{
		"thread": code, "entries": []int{int(item["entry"].(float64))}})
	if isErr {
		t.Fatalf("expand: %v", entryExpanded)
	}
	rows := entryExpanded["entries"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expanded = %v", rows)
	}
	full := rows[0].(map[string]any)
	if _, rogue := full["hello from the HTTP side"]; rogue {
		t.Fatal("content leaked into a structure key")
	}
	if full["content"] != "hello from the HTTP side" {
		t.Fatalf("content = %v", full["content"])
	}
}

// TestA20OrchestrationEndToEnd: the agent-team loop over real tools —
// one orchestrator, three workers, key handoff, contracted work,
// judgment with a redo, close (kungfu.md §6+§8; PRD A20).
func anyLen(v any) int {
	switch s := v.(type) {
	case []any:
		return len(s)
	case []map[string]any:
		return len(s)
	}
	return -1
}

func asFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return -1
}

func TestA20OrchestrationEndToEnd(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	defer srv.Close()
	defer pool.Close()
	ctx := context.Background()

	lead, _, idLead := m2RegisterSeeded(t, srv, pool, 0)
	_, _, idW1 := m2RegisterSeeded(t, srv, pool, 0)
	_, _, idW2 := m2RegisterSeeded(t, srv, pool, 0)
	_, _, idW3 := m2RegisterSeeded(t, srv, pool, 0)

	call := func(botID int64, key string, tool string, args map[string]any) map[string]any {
		t.Helper()
		if key != "" {
			env, isErr, _ := mcpCall(t, srv, key, tool, args)
			if isErr {
				t.Fatalf("%s: %v", tool, env)
			}
			return env
		}
		bot := wo7Bot(t, pool, botID)
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		if status != 200 || env["ok"] != true {
			t.Fatalf("%s: %d %v", tool, status, env)
		}
		return env
	}
	todo := func(botID int64) []map[string]any {
		env := call(botID, "", "todo_list", map[string]any{})
		return env["todos"].([]map[string]any)
	}

	// 1. lead opens the room with a key
	start := call(idLead, lead, "thread_start", map[string]any{"subject": "release 1.0", "key": true, "idempotency_key": "a20-s"})
	code := start["thread"].(string)
	rawKey := start["key"].(string)

	// 2. three workers join; each owes a reply to the root entry
	for i, id := range []int64{idW1, idW2, idW3} {
		call(id, "", "thread_join", map[string]any{"key": rawKey, "idempotency_key": "a20-j" + string(rune('1'+i))})
	}
	root := call(idLead, lead, "thread_post", map[string]any{
		"thread": code, "content": "We ship the report today. Acknowledge, then take your section.",
		"ask": []int64{idW1, idW2, idW3}, "idempotency_key": "a20-root"})
	rootEntry := asFloat64(root["entry"])
	for _, id := range []int64{idW1, idW2, idW3} {
		items := todo(id)
		if len(items) != 1 || items[0]["kind"] != "reply" || asFloat64(items[0]["entry"]) != rootEntry {
			t.Fatalf("worker %d todo = %v", id, items)
		}
		// each worker acknowledges the root entry (the reply ends
		// its pending receipt toward it, §6.3)
		call(id, "", "thread_post", map[string]any{
			"thread": code, "content": "ack", "reply_to": int64(rootEntry), "ask": []int64{},
			"idempotency_key": "a20-ack" + map[int64]string{idW1: "1", idW2: "2", idW3: "3"}[id]})
		if items := todo(id); len(items) != 0 {
			t.Fatalf("worker %d still owes after ack: %v", id, items)
		}
	}

	// 3. contracted work: three assignments riding asked entries
	assignFor := func(id int64, i int, ask bool) (entry, assign float64) {
		args := map[string]any{
			"thread": code, "content": "section " + string(rune('A'+i)), "idempotency_key": "a20-p" + string(rune('1'+i)),
			"assign": map[string]any{"to": id, "requirements": "write section, 3 paragraphs", "deliver_due": 3600, "judge_due": 3600},
		}
		if ask {
			args["ask"] = []int64{id}
		}
		env := call(idLead, lead, "thread_post", args)
		return asFloat64(env["entry"]), asFloat64(env["assign"])
	}
	_, a1 := assignFor(idW1, 0, true)
	_, a2 := assignFor(idW2, 1, true)
	_, a3 := assignFor(idW3, 2, true)

	// 4. take (fulfills the ask receipt), submit
	call(idW1, "", "assign_take", map[string]any{"assign": int64(a1), "payload": `{"section":"A","paras":3}`, "idempotency_key": "a20-t1"})
	call(idW2, "", "assign_take", map[string]any{"assign": int64(a2), "payload": `{"section":"B","paras":2}`, "idempotency_key": "a20-t2"})
	call(idW3, "", "assign_take", map[string]any{"assign": int64(a3), "payload": `{"section":"C","paras":0}`, "idempotency_key": "a20-t3"})
	// merged take+submit: the workers owe nothing further, and the
	// lead's turn list gains three judge items (§8)
	for _, id := range []int64{idW1, idW2, idW3} {
		if items := todo(id); len(items) != 0 {
			t.Fatalf("worker %d owes after merged take+submit: %v", id, items)
		}
	}
	if items := todo(idLead); len(items) != 3 {
		t.Fatalf("lead judge items = %v, want 3", items)
	}

	// 5. lead judges: adopt x2, reject with reason x1
	call(idLead, lead, "assign_judge", map[string]any{"assign": int64(a1), "verdict": "adopt", "idempotency_key": "a20-j1"})
	call(idLead, lead, "assign_judge", map[string]any{"assign": int64(a2), "verdict": "adopt", "idempotency_key": "a20-j2"})
	rej := call(idLead, lead, "assign_judge", map[string]any{"assign": int64(a3), "verdict": "reject", "reason": "zero paragraphs", "idempotency_key": "a20-j3"})
	if rej["state"] != "rejected" {
		t.Fatalf("reject = %v", rej)
	}

	// 6. redo references the old assignment
	redo := call(idLead, lead, "thread_post", map[string]any{
		"thread": code, "content": "section C redo — 3 paragraphs this time",
		"ask": []int64{idW3}, "idempotency_key": "a20-r",
		"assign": map[string]any{"to": idW3, "requirements": "redo of section C (rejected: zero paragraphs)", "deliver_due": 3600, "judge_due": 3600}})
	call(idW3, "", "assign_take", map[string]any{"assign": int64(asFloat64(redo["assign"])), "payload": `{"section":"C","paras":3}`, "idempotency_key": "a20-t4"})
	call(idLead, lead, "assign_judge", map[string]any{"assign": int64(asFloat64(redo["assign"])), "verdict": "adopt", "idempotency_key": "a20-j4"})

	// 7. everyone owes nothing; lead closes; the room reads back
	if items := todo(idW3); len(items) != 0 {
		t.Fatalf("w3 residual todo: %v", items)
	}
	call(idLead, lead, "thread_close", map[string]any{"thread": code, "idempotency_key": "a20-c"})
	final := call(idW1, "", "thread_get", map[string]any{"thread": code})
	if final["thread"].(map[string]any)["status"] != "closed" {
		t.Fatalf("final status = %v", final["thread"])
	}
	if n := anyLen(final["timeline"]); n != 8 { // root + 3 acks + 3 assignments + the redo
		t.Fatalf("timeline = %d entries, want 8", n)
	}
	if final["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want nil under one page", final["next_cursor"])
	}
}
