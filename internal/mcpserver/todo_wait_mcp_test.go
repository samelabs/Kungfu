package mcpserver

// todo_wait through the tool surface: envelope shape on the normal
// timeout answer (changed=false + wait + retry_after) — the blocking
// behaviour itself is covered by the service tests.

import (
	"testing"
)

func TestTodoWaitMCPEnvelope(t *testing.T) {
	srv, pool := m1SetupServer(t)
	key, _, _ := m2RegisterSeeded(t, srv, pool, 0)
	res, isErr, _ := mcpCall(t, srv, key, "todo_wait", map[string]any{"timeout": 1})
	if isErr {
		t.Fatalf("todo_wait errored: %v", res)
	}
	if res["changed"] != false {
		t.Fatalf("idle todo_wait.changed = %v, want false", res["changed"])
	}
	if res["next_action"] != "wait" {
		t.Fatalf("idle todo_wait.next_action = %v, want wait", res["next_action"])
	}
	if ra, _ := res["retry_after"].(float64); ra < 1 {
		t.Fatalf("idle todo_wait.retry_after = %v, want >=1", res["retry_after"])
	}
}
