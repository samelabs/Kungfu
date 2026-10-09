package server

// WO-30 §3/§5: the three read-only workbench views — Turn (/owner),
// Threads (/owner/threads) and Memory (/owner/memories). Unauthenticated
// visitors get the guest shell (the login landing; the JS lifecycle
// takes them to sign-in — the API under the pages is the auth
// boundary). Authenticated owners read only their own agent's facts
// through the bridge, and the bridge exposes no write path into
// threads or memory.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/mcpserver"
	"kungfu.md/internal/repository"
)

// TestOwnerWorkbenchViewsGuestShowLogin: the three views (and their
// detail routes) render the owner guest shell for signed-out
// visitors — the auth landing with the login link — and stay noindex.
func TestOwnerWorkbenchViewsGuestShowLogin(t *testing.T) {
	s, pool, _, _ := ownerConsoleEnv(t)
	_ = pool

	for _, path := range []string{
		"/owner", "/owner/threads", "/owner/threads/abc123def456",
		"/owner/memories", "/owner/memories/abc123def456",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.buildRouter().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		// the guest shell: login landing with the sign-in affordance
		// (LocaleURL carries ?lang= even for en)
		if !strings.Contains(body, `href="/owner/login?lang=en"`) {
			t.Fatalf("%s: guest shell missing the login link", path)
		}
		if !strings.Contains(body, `<meta name="robots" content="noindex,nofollow">`) {
			t.Fatalf("%s: owner pages must stay noindex", path)
		}
		// the section container is present but empty (data arrives
		// only through the authenticated bridge); detail routes carry
		// their own section names
		section := "overview"
		switch path {
		case "/owner/threads":
			section = "threads"
		case "/owner/threads/abc123def456":
			section = "thread_detail"
		case "/owner/memories":
			section = "memories"
		case "/owner/memories/abc123def456":
			section = "memory_detail"
		}
		if !strings.Contains(body, `data-section="`+section+`"`) {
			t.Fatalf("%s: missing data-section=%q", path, section)
		}
	}
}

// TestOwnerWorkbenchBridgeScopedReadOnly: with two agents in a room,
// the bridge answers each session with ONLY that agent's facts
// (todo_list, thread_list, thread_get, memory_list, memory_get), and
// every write path stays 404 UNKNOWN_TOOL.
func TestOwnerWorkbenchBridgeScopedReadOnly(t *testing.T) {
	s, pool, nameA, botA := ownerConsoleEnv(t)
	ctx := context.Background()
	_ = s.buildRouter() // wires mcpDepsValue
	deps := s.mcpDeps()

	// agent B (the asked member / assignee) with a private memory
	nameB := "wbghost" + time.Now().Format("150405.000000000")
	secretCode := fmt.Sprintf("wb%09d", time.Now().UnixNano()%1e9)
	digestB := sha256.Sum256([]byte(nameB))
	var botB int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'wb99', 'x', 0) RETURNING id`, nameB, digestB[:]).Scan(&botB); err != nil {
		t.Fatalf("seed bot B: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status)
		VALUES ($2, $1, 'B private memory', '["t"]', 'private body of B, long enough', 'c0wb01', 'private', 'active')`,
		botB, secretCode); err != nil {
		t.Fatalf("seed B memory: %v", err)
	}

	// a third agent with no membership
	nameC := "wbouts" + time.Now().Format("150405.000000000")
	digestC := sha256.Sum256([]byte(nameC))
	var botC int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'wb98', 'x', 0) RETURNING id`, nameC, digestC[:]).Scan(&botC); err != nil {
		t.Fatalf("seed bot C: %v", err)
	}

	rowA, err := repository.FindActiveBotAccountByID(ctx, pool, botA)
	if err != nil || rowA == nil {
		t.Fatalf("bot A lookup: %v", err)
	}
	rowB, err := repository.FindActiveBotAccountByID(ctx, pool, botB)
	if err != nil || rowB == nil {
		t.Fatalf("bot B lookup: %v", err)
	}

	// A opens a room with a signed key, B joins with it, then A asks B
	// to respond and assigns B a work unit
	startEnv, status := mcpserver.CallTool(ctx, deps, "thread_start", rowA,
		mustJSON(t, map[string]any{"subject": "workbench scope room", "key": true}))
	if status != 200 || startEnv["ok"] != true {
		t.Fatalf("thread_start: %d %v", status, startEnv)
	}
	code, _ := startEnv["thread"].(string)
	if code == "" {
		t.Fatalf("thread_start returned no code: %v", startEnv)
	}
	roomKey, _ := startEnv["key"].(string) // the raw key, disclosed once
	if roomKey == "" {
		t.Fatalf("thread_start(key=true) returned no raw key: %v", startEnv)
	}
	joinEnv, status := mcpserver.CallTool(ctx, deps, "thread_join", rowB,
		mustJSON(t, map[string]any{"key": roomKey}))
	if status != 200 || joinEnv["ok"] != true {
		t.Fatalf("B thread_join: %d %v", status, joinEnv)
	}
	postEnv, status := mcpserver.CallTool(ctx, deps, "thread_post", rowA, mustJSON(t, map[string]any{
		"thread":  code,
		"content": "Please respond and take the assignment.",
		"ask":     []any{botB},
		"assign":  map[string]any{"to": botB, "requirements": "do the scoped thing"},
	}))
	if status != 200 || postEnv["ok"] != true {
		t.Fatalf("thread_post: %d %v", status, postEnv)
	}

	cookieA := ocSessionCookie(t, s, pool, nameA)
	cookieB := ocSessionCookie(t, s, pool, nameB)
	cookieC := ocSessionCookie(t, s, pool, nameC)

	// --- Turn: B owes the reply; A does not ---
	recB, envB := ocCall(t, s, cookieB, "todo_list", map[string]any{})
	if recB.Code != 200 || envB["ok"] != true {
		t.Fatalf("B todo_list: %d %v", recB.Code, envB)
	}
	todosB, _ := envB["todos"].([]any)
	foundReply := false
	for _, it := range todosB {
		row, _ := it.(map[string]any)
		if row["kind"] == "reply" && row["thread"] == code {
			foundReply = true
		}
	}
	if !foundReply {
		t.Fatalf("B todo_list misses the reply toward A's entry: %v", envB["todos"])
	}
	_, envA := ocCall(t, s, cookieA, "todo_list", map[string]any{})
	todosA, _ := envA["todos"].([]any)
	if len(todosA) != 0 {
		t.Fatalf("A (the asker, nothing delivered yet) owes nothing: %v", todosA)
	}

	// --- Threads: B and A see the room; C does not ---
	_, envBL := ocCall(t, s, cookieB, "thread_list", map[string]any{})
	threadsB, _ := envBL["threads"].([]any)
	seenB := false
	for _, it := range threadsB {
		row, _ := it.(map[string]any)
		if room, _ := row["thread"].(map[string]any); room != nil && room["code"] == code {
			seenB = true
			if row["role"] != "speaker" {
				t.Fatalf("B role = %v, want speaker", row["role"])
			}
			if row["open_items"] == nil {
				t.Fatal("thread_list rows must carry open_items")
			}
			if row["open_invites"] == nil {
				t.Fatal("thread_list rows must carry open_invites")
			}
		}
	}
	if !seenB {
		t.Fatalf("B thread_list misses the room: %v", envBL["threads"])
	}
	_, envCL := ocCall(t, s, cookieC, "thread_list", map[string]any{})
	threadsC, _ := envCL["threads"].([]any)
	for _, it := range threadsC {
		row, _ := it.(map[string]any)
		if room, _ := row["thread"].(map[string]any); room != nil && room["code"] == code {
			t.Fatal("C thread_list leaked a room C is not in")
		}
	}
	recCG, envCG := ocCall(t, s, cookieC, "thread_get", map[string]any{"thread": code})
	if recCG.Code == 200 || envCG["ok"] == true {
		t.Fatalf("C thread_get on a foreign room must fail: %d %v", recCG.Code, envCG)
	}

	// --- thread_get: B's work set carries timeline, members, todos ---
	_, envBG := ocCall(t, s, cookieB, "thread_get", map[string]any{"thread": code})
	if envBG["ok"] != true {
		t.Fatalf("B thread_get: %v", envBG)
	}
	for _, key := range []string{"timeline", "members", "assignments", "todos", "role"} {
		if _, present := envBG[key]; !present {
			t.Fatalf("B thread_get missing %q", key)
		}
	}

	// --- Memory: B reads its own; A cannot read B's private memory ---
	_, envBM := ocCall(t, s, cookieB, "memory_get", map[string]any{"code": secretCode})
	if envBM["ok"] != true || envBM["content"] != "private body of B, long enough" {
		t.Fatalf("B memory_get own: %v", envBM)
	}
	recAM, envAM := ocCall(t, s, cookieA, "memory_get", map[string]any{"code": secretCode})
	if recAM.Code == 200 || envAM["ok"] == true {
		t.Fatalf("A memory_get on B's private memory must fail: %d %v", recAM.Code, envAM)
	}
	_, envBMList := ocCall(t, s, cookieB, "memory_list", map[string]any{})
	if envBMList["ok"] != true {
		t.Fatalf("B memory_list: %v", envBMList)
	}

	// --- read-only boundary: no write path into threads or memory ---
	for _, banned := range []string{
		"thread_post", "thread_handle", "thread_retract", "thread_start",
		"thread_close", "thread_key", "assign_take", "assign_submit",
		"assign_judge", "assign_drop", "assign_void",
		"memory_put", "memory_share", "memory_unshare", "memory_delete",
		"work_submit", "account_register", "no_such_tool",
	} {
		rec, env := ocCall(t, s, cookieA, banned, map[string]any{})
		if rec.Code != 404 || env["error"].(map[string]any)["code"] != "UNKNOWN_TOOL" {
			t.Fatalf("%s: %d %v, want 404 UNKNOWN_TOOL", banned, rec.Code, env)
		}
	}

	// --- anonymous: every workbench read is 401 ---
	for _, tool := range []string{"todo_list", "thread_list", "thread_get", "memory_list", "memory_get"} {
		rec, env := ocCall(t, s, nil, tool, map[string]any{})
		if rec.Code != 401 || env["error"].(map[string]any)["code"] != "UNAUTHORIZED" {
			t.Fatalf("anonymous %s: %d %v, want 401", tool, rec.Code, env)
		}
	}
}
