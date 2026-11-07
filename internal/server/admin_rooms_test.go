package server

// Admin room disposal (migration 035): the platform door — CSRF gate,
// permission gate, and the same settle-then-delete core the governor
// tool uses. The room must be gone afterwards, with the audit row
// naming the actor, and a second call answers THREAD_NOT_FOUND.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/service"
)

// roomBot seeds a bot row that can own rooms (mirrors govBot).
func roomBot(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	name := fmt.Sprintf("room_%d", time.Now().UnixNano())
	h := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		 VALUES ($1, $2, 'rm01', 'x', 0) RETURNING id`, name, h[:]).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id) })
	return id
}

// roomWithPayload creates a room holding one payload-carrying entry.
func roomWithPayload(t *testing.T, pool *pg.Pool, owner int64) string {
	t.Helper()
	ctx := context.Background()
	res, err := service.ThreadStart(ctx, pool, owner, "admin-dispose", true, "")
	if err != nil {
		t.Fatalf("thread_start: %v", err)
	}
	code, _ := res["thread"].(string)
	if _, err := service.ThreadPost(ctx, pool, owner, code,
		"payload for the admin door", "", "", nil, nil, nil, "ad1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	return code
}

func TestAdminRoomDispose(t *testing.T) {
	e := newAdminEnv(t)
	pool := e.s.Pool
	code := roomWithPayload(t, pool, roomBot(t, pool))

	// without CSRF the door stays shut
	if rec := e.do(t, "POST", "/api/samelabs/rooms/"+code+"/dispose", "", false); rec.Code == 200 {
		t.Fatalf("dispose without CSRF must not succeed")
	}

	// with CSRF: disposed, audited as the admin actor
	rec := e.do(t, "POST", "/api/samelabs/rooms/"+code+"/dispose", "", true)
	if rec.Code != 200 {
		t.Fatalf("dispose = %d %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["disposed"] != true {
		t.Fatalf("result = %v", res)
	}

	var actor string
	if err := pool.QueryRow(context.Background(),
		`SELECT actor_kind FROM thread_disposals WHERE code=$1`, code).Scan(&actor); err != nil || actor != "admin" {
		t.Fatalf("audit row = %q err=%v, want admin", actor, err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM threads WHERE code=$1`, code).Scan(&n); err != nil || n != 0 {
		t.Fatalf("room survived: n=%d err=%v", n, err)
	}

	// second call: THREAD_NOT_FOUND, not 500
	rec2 := e.do(t, "POST", "/api/samelabs/rooms/"+code+"/dispose", "", true)
	if rec2.Code != http.StatusNotFound && !strings.Contains(rec2.Body.String(), "THREAD_NOT_FOUND") {
		t.Fatalf("second dispose = %d %s, want THREAD_NOT_FOUND", rec2.Code, rec2.Body.String())
	}
}

func TestAdminRoomDisposePermissionDenied(t *testing.T) {
	// a limited admin (no roles → no rooms.manage) must not dispose
	s := newAdminTestServer(t)
	s.TrustedProxies = nil
	username := "noroom_" + time.Now().Format("150405.000000000")
	password := "noroom-pass-1"
	hash, err := hashPasswordForAdminTest(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1,'A',$2) RETURNING id`, username, hash).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admin_sessions WHERE admin_id=$1`, id)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_admins WHERE id=$1`, id)
	})

	code := roomWithPayload(t, s.Pool, roomBot(t, s.Pool))
	router := s.buildRouter()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest("POST", "/api/samelabs/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.AdminCookieName {
			cookie = c
		}
	}
	var resp struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	req2 := httptest.NewRequest("POST", "/api/samelabs/rooms/"+code+"/dispose", nil)
	req2.AddCookie(cookie)
	req2.Header.Set("X-CSRF-Token", resp.Data.CSRFToken)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code == 200 {
		t.Fatal("admin without rooms.manage must not dispose")
	}
	var n int
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM threads WHERE code=$1`, code).Scan(&n); err != nil || n != 1 {
		t.Fatalf("room must survive a denied dispose: n=%d err=%v", n, err)
	}
}
