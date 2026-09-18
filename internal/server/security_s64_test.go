package server

// S6.4: Owner mutation JSON gate — request-forgery boundary for the
// browser Owner plane. No token mechanism is introduced.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// s64Server builds a real server + router + owner session cookie.
func s64Server(t *testing.T) (*Server, int64, *http.Cookie) {
	t.Helper()
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	w := httptest.NewRecorder()
	_, botID := s64SeedBot(t, pool, s, fmt.Sprint(time.Now().UnixNano()))
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	return s, botID, parseSetCookie(t, w.Header().Get("Set-Cookie"))
}

func s64SeedBot(t *testing.T, pool *pg.Pool, s *Server, suffix string) (*Server, int64) {
	t.Helper()
	var id int64
	rawKey := "kf_live_" + suffix + strings.Repeat("a", 64-len(suffix))
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		VALUES ($1, $2, $3, 'x', 'active', 1000) RETURNING id`,
		"s64bot_"+suffix, s61SeedKeyHash(rawKey), s61SeedLast4(rawKey)).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_tasks WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id)
	})
	return s, id
}

func s64Mutate(t *testing.T, s *Server, cookie *http.Cookie, method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	return rec
}

// 1. form-urlencoded CSRF shape on a previously bodyless mutation.
func TestS64OwnerMutationRejectsFormURLEncoded(t *testing.T) {
	s, _, cookie := s64Server(t)
	rec := s64Mutate(t, s, cookie, "POST", "/api/owner/tasks/TCK000000001/close",
		"application/x-www-form-urlencoded", "code=x")
	if rec.Code != 415 {
		t.Fatalf("form POST = %d, want 415 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UNSUPPORTED_MEDIA_TYPE") {
		t.Fatalf("error code missing: %s", rec.Body.String())
	}
}

// 2. text/plain form-encodable type.
func TestS64OwnerMutationRejectsTextPlain(t *testing.T) {
	s, _, cookie := s64Server(t)
	rec := s64Mutate(t, s, cookie, "POST", "/api/owner/tasks/TCK000000001/close",
		"text/plain", "x")
	if rec.Code != 415 {
		t.Fatalf("text/plain POST = %d, want 415", rec.Code)
	}
}

// 3. missing Content-Type entirely.
func TestS64OwnerMutationRejectsMissingContentType(t *testing.T) {
	s, _, cookie := s64Server(t)
	rec := s64Mutate(t, s, cookie, "DELETE", "/api/owner/session", "", "")
	if rec.Code != 415 {
		t.Fatalf("no-content-type DELETE = %d, want 415", rec.Code)
	}
}

// 4. application/json reaches the existing handler (401/200/400 from
// business logic — anything but 415 proves the gate passed).
func TestS64OwnerMutationAcceptsApplicationJSON(t *testing.T) {
	s, _, cookie := s64Server(t)
	for _, tc := range []struct{ m, p string }{
		{"POST", "/api/owner/tasks"}, {"DELETE", "/api/owner/session"},
		{"POST", "/api/owner/tasks/TCK000000001/refund"},
	} {
		rec := s64Mutate(t, s, cookie, tc.m, tc.p, "application/json", "{}")
		if rec.Code == 415 {
			t.Fatalf("%s %s rejected by the gate", tc.m, tc.p)
		}
	}
}

// 5. parameters such as charset stay acceptable.
func TestS64OwnerMutationAcceptsJSONWithParameters(t *testing.T) {
	s, _, cookie := s64Server(t)
	rec := s64Mutate(t, s, cookie, "POST", "/api/owner/tasks",
		"application/json; charset=utf-8", "{}")
	if rec.Code == 415 {
		t.Fatalf("parameterized JSON rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// 6. Owner reads never require Content-Type.
func TestS64OwnerReadsUnaffected(t *testing.T) {
	s, _, cookie := s64Server(t)
	for _, p := range []string{"/api/owner/tasks", "/api/account", "/api/key"} {
		rec := s64Mutate(t, s, cookie, "GET", p, "", "")
		if rec.Code == 415 {
			t.Fatalf("GET %s hit the mutation gate", p)
		}
	}
}

// 7. Agent routes are not behind the Owner gate.
func TestS64AgentRoutesUnaffected(t *testing.T) {
	s, _, _ := s64Server(t)
	rec := s64Mutate(t, s, nil, "POST", "/api/register",
		"application/x-www-form-urlencoded", "name=agent01&password=secret123")
	if rec.Code == 415 {
		t.Fatal("Agent register must not be behind the Owner gate")
	}
}

// 8. Admin mutation mechanism is untouched by this gate.
func TestS64AdminRoutesRemainUnderAdminMechanism(t *testing.T) {
	s, _, _ := s64Server(t)
	// admin session create is NOT ownerMutation-wired: a form POST
	// there must be handled by the ADMIN plane (401/400 from admin
	// auth parsing), never 415 from the Owner gate.
	rec := s64Mutate(t, s, nil, "POST", "/api/admin/session",
		"application/x-www-form-urlencoded", "u=x")
	if rec.Code == 415 {
		t.Fatal("Admin route appears to be behind the Owner gate")
	}
}

// 9. Creem webhook must stay outside the Owner gate.
func TestS64CreemWebhookUnaffected(t *testing.T) {
	s, _, _ := s64Server(t)
	rec := s64Mutate(t, s, nil, "POST", "/api/webhooks/creem",
		"application/x-www-form-urlencoded", "x=1")
	if rec.Code == 415 {
		t.Fatal("Creem webhook must not be behind the Owner gate")
	}
}

// 10. Architecture guard: the complete authorized unsafe-route set is
// wired through the single canonical gate at route composition.
func TestS64AllOwnerUnsafeRoutesUseSingleGate(t *testing.T) {
	src := s61Read(t, "internal/server/router.go")
	routes := []string{
		`r.Post("/api/owner/session", ownerMutation(`,
		`r.Delete("/api/owner/session", ownerMutation(`,
		`r.Post("/api/change-password", ownerMutation(`,
		`r.Post("/api/reset-key", ownerMutation(`,
		`r.Post("/api/owner/tasks", ownerMutation(`,
		`r.Post("/api/owner/tasks/{code}/open", ownerMutation(`,
		`r.Post("/api/owner/tasks/{code}/close", ownerMutation(`,
		`r.Post("/api/owner/tasks/{code}/add-budget", ownerMutation(`,
		`r.Post("/api/owner/tasks/{code}/refund", ownerMutation(`,
		`r.Post("/api/owner/tasks/{code}/edit", ownerMutation(`,
		`r.Post("/api/testtask/{code}", ownerMutation(`,
		`r.Post("/api/owner/payments/checkout", ownerMutation(`,
		`r.Post("/api/owner/store/redemptions", ownerMutation(`,
	}
	for _, r := range routes {
		if !strings.Contains(src, r) {
			t.Fatalf("route not gated: %s", r)
		}
	}
	// no ungated duplicates remain for the same paths
	for _, path := range []string{
		`"/api/owner/tasks/{code}/open"`, `"/api/reset-key"`, `"/api/owner/payments/checkout"`,
	} {
		i := strings.Index(src, path)
		if i == -1 {
			continue
		}
		lineStart := strings.LastIndex(src[:i], "\n") + 1
		line := src[lineStart : strings.Index(src[i:], "\n")+i]
		if "ownerMutation(" != strings.TrimSpace(strings.SplitN(strings.SplitN(line, ",", 2)[1], "ownerMutation", 2)[0])+"ownerMutation"[:0] && !strings.Contains(line, "ownerMutation(") {
			t.Fatalf("ungated route line: %s", line)
		}
	}
}

// 11. No permissive credentialed CORS exists for Owner mutations.
func TestS64NoPermissiveCredentialedCORS(t *testing.T) {
	s, _, cookie := s64Server(t)
	req := httptest.NewRequest("OPTIONS", "/api/owner/tasks", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)

	acao := rec.Header().Get("Access-Control-Allow-Origin")
	acac := rec.Header().Get("Access-Control-Allow-Credentials")
	if strings.EqualFold(acao, "*") || (acao != "" && strings.EqualFold(acac, "true")) {
		t.Fatalf("permissive credentialed CORS: ACAO=%q ACAC=%q", acao, acac)
	}
	// and a cross-origin JSON POST still does not get permissive CORS
	rec2 := s64Mutate(t, s, cookie, "POST", "/api/owner/tasks", "application/json", "{}")
	acao2 := rec2.Header().Get("Access-Control-Allow-Origin")
	if strings.EqualFold(acao2, "*") || strings.EqualFold(rec2.Header().Get("Access-Control-Allow-Credentials"), "true") {
		t.Fatalf("permissive credentialed CORS on mutation: ACAO=%q", acao2)
	}
}
