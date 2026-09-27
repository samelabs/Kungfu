package server

// WO-11: the owner console's new-task page ships a simple mode, and
// the console bridge carries task_create's open flag end to end.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// simpleGet fetches a path with the session cookie.
func simpleGet(t *testing.T, s *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	return rec
}

// assetURL extracts the fingerprinted URL of one asset referenced by
// the page (assets are served with a content-hash query).
func assetURL(t *testing.T, s *Server, page, file string, cookie *http.Cookie) string {
	t.Helper()
	rec := simpleGet(t, s, page, cookie)
	body := rec.Body.String()
	i := strings.Index(body, file)
	if i < 0 {
		t.Fatalf("%s does not reference %s", page, file)
	}
	start := strings.LastIndex(body[:i], "/assets/")
	if start < 0 {
		t.Fatalf("no asset path before %s", file)
	}
	end := strings.IndexAny(body[i:], "\"'")
	if end < 0 {
		t.Fatal("asset path not terminated")
	}
	return body[start : i+end]
}

func TestOwnerTaskNewPageSimpleMode(t *testing.T) {
	s, pool, name, _ := ownerConsoleEnv(t)
	_ = pool
	cookie := ocSessionCookie(t, s, pool, name)

	// the page shell routes and loads the console driver
	req := simpleGet(t, s, "/owner/tasks/new", cookie)
	if req.Code != 200 {
		t.Fatalf("/owner/tasks/new = %d", req.Code)
	}
	if !strings.Contains(req.Body.String(), "taskEditorRoot") {
		t.Fatal("new-task page lacks the editor root")
	}

	// the console asset carries the simple-mode form (title/
	// requirements/receiver/sample/price/units + open now) and falls
	// back to the JSON editor
	asset := simpleGet(t, s, assetURL(t, s, "/owner/tasks/new", "owner/tasks-console.js", cookie), cookie)
	if asset.Code != 200 {
		t.Fatalf("tasks-console.js = %d", asset.Code)
	}
	js := asset.Body.String()
	for _, marker := range []string{"tcvRenderSimpleForm", "tcvSPublish", "tcvSUnits", "tcvSOpen", "tcvSReceiver", "tcvSSample", "tasks.advanced"} {
		if !strings.Contains(js, marker) {
			t.Fatalf("tasks-console.js missing simple-mode marker %s", marker)
		}
	}
}

func TestOwnerToolTaskCreateOpen(t *testing.T) {
	s, pool, name, _ := ownerConsoleEnv(t)
	cookie := ocSessionCookie(t, s, pool, name)

	rec, env := ocCall(t, s, cookie, "task_create", map[string]any{
		"contract": map[string]any{
			"title":        "Console minimal",
			"requirements": "Three bullets.",
			"receiver":     map[string]any{"url": okReceiverURL},
			"sample":       map[string]any{"result": "three bullets"},
			"price":        5,
		},
		"budget": 10,
		"open":   true,
	})
	if rec.Code != 200 || env["ok"] != true || env["status"] != "open" {
		t.Fatalf("console task_create open=true: %d %v", rec.Code, env)
	}
	// the required-fields contract went through and opened (its
	// sample test-delivered to the receiver)
	if env["status"] != "open" {
		t.Fatalf("status: %v", env["status"])
	}
	_ = json.Marshal
}
