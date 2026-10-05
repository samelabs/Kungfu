package mcpserver

import (
	"encoding/json"
	"testing"
)

func threadStructured(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var rpc map[string]interface{}
	if err := json.Unmarshal([]byte(extractJSON(body)), &rpc); err != nil {
		t.Fatalf("parse MCP body: %v: %.400s", err, body)
	}
	result, _ := rpc["result"].(map[string]interface{})
	if result == nil {
		t.Fatalf("missing result: %.400s", body)
	}
	sc, _ := result["structuredContent"].(map[string]interface{})
	if sc == nil {
		t.Fatalf("missing structuredContent: %.400s", body)
	}
	return sc
}

func TestThreadToolsRequireAuth(t *testing.T) {
	_, ts := toolsSetup(t)
	for _, tool := range []string{
		"thread_create", "thread_list", "thread_get", "thread_updates",
		"thread_invite", "thread_invite_revoke", "thread_join",
		"thread_remove_member", "thread_message", "thread_deliver",
		"thread_review_delivery", "thread_handoff", "thread_close",
	} {
		sc, _ := m2CallTool(t, ts, "", tool, map[string]interface{}{})
		if sc != 401 {
			t.Fatalf("%s without Authorization = %d, want 401", tool, sc)
		}
	}
}

func TestMCPThreadCollaborationLifecycle(t *testing.T) {
	pool, ts := toolsSetup(t)
	ownerName, ownerKey, _ := m2Bot(t, pool, ts.srv, "threadowner")
	memberName, memberKey, _ := m2Bot(t, pool, ts.srv, "threadmember")
	_, outsiderKey, _ := m2Bot(t, pool, ts.srv, "threadoutside")

	sc, body := m2CallTool(t, ts, ownerKey, "thread_create", map[string]interface{}{
		"title": "AIchem launch collaboration",
		"objective": "Coordinate media launch work with persistent state.",
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_create: %d %.500s", sc, body)
	}
	created := threadStructured(t, body)
	code, _ := created["code"].(string)
	if code == "" {
		t.Fatalf("thread_create missing code: %#v", created)
	}
	startCursor, _ := created["cursor"].(float64)

	// Isolation is enforced by the service, not by agent behavior.
	sc, body = m2CallTool(t, ts, outsiderKey, "thread_get", map[string]interface{}{"code": code})
	if sc != 200 || !toolFailed(body) || !containsToolError(body, "THREAD_NOT_FOUND") {
		t.Fatalf("outsider thread_get: %d %.500s", sc, body)
	}

	sc, body = m2CallTool(t, ts, ownerKey, "thread_invite", map[string]interface{}{
		"code": code, "participant": memberName, "expires_in_hours": 24,
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_invite: %d %.500s", sc, body)
	}
	invite := threadStructured(t, body)
	token, _ := invite["invite_token"].(string)
	if token == "" {
		t.Fatalf("thread_invite missing token: %#v", invite)
	}

	sc, body = m2CallTool(t, ts, memberKey, "thread_join", map[string]interface{}{"invite_token": token})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_join: %d %.500s", sc, body)
	}

	sc, body = m2CallTool(t, ts, memberKey, "thread_message", map[string]interface{}{
		"code": code, "body": "I am checking the first media contacts now.",
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_message: %d %.500s", sc, body)
	}

	sc, body = m2CallTool(t, ts, ownerKey, "thread_handoff", map[string]interface{}{
		"code": code, "participant": memberName, "next_action": "Verify the first media batch.",
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_handoff: %d %.500s", sc, body)
	}

	sc, body = m2CallTool(t, ts, memberKey, "thread_deliver", map[string]interface{}{
		"code": code, "title": "Verified media batch", "body": "Twenty media contacts checked; source URLs retained.",
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_deliver: %d %.500s", sc, body)
	}
	delivery := threadStructured(t, body)
	deliveryID, ok := delivery["delivery_id"].(float64)
	if !ok || deliveryID <= 0 {
		t.Fatalf("delivery_id: %#v", delivery)
	}

	sc, body = m2CallTool(t, ts, ownerKey, "thread_review_delivery", map[string]interface{}{
		"code": code, "delivery_id": int64(deliveryID), "decision": "accepted", "note": "Use this batch.",
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_review_delivery: %d %.500s", sc, body)
	}

	sc, body = m2CallTool(t, ts, ownerKey, "thread_updates", map[string]interface{}{
		"code": code, "cursor": int64(startCursor), "limit": 50,
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_updates: %d %.500s", sc, body)
	}
	updates := threadStructured(t, body)
	events, _ := updates["events"].([]interface{})
	if len(events) < 5 {
		t.Fatalf("updates events = %d, want invite/join/message/handoff/delivery/review activity: %#v", len(events), updates)
	}

	sc, body = m2CallTool(t, ts, ownerKey, "thread_get", map[string]interface{}{"code": code})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("thread_get: %d %.500s", sc, body)
	}
	got := threadStructured(t, body)
	owner, _ := got["owner"].(map[string]interface{})
	if owner["bot_name"] != ownerName {
		t.Fatalf("owner mismatch: %#v", got)
	}
	next, _ := got["next_actor"].(map[string]interface{})
	if next["bot_name"] != memberName || got["next_action"] != "Verify the first media batch." {
		t.Fatalf("handoff not persisted: %#v", got)
	}
}

func containsToolError(body, code string) bool {
	var rpc map[string]interface{}
	if json.Unmarshal([]byte(extractJSON(body)), &rpc) != nil {
		return false
	}
	result, _ := rpc["result"].(map[string]interface{})
	sc, _ := result["structuredContent"].(map[string]interface{})
	errObj, _ := sc["error"].(map[string]interface{})
	return errObj != nil && errObj["code"] == code
}
