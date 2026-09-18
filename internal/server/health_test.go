package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

func r4Router(s *Server) http.Handler {
	return s.buildRouter()
}

func r4GET(router http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

type r4Envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

func r4Decode(t *testing.T, rec *httptest.ResponseRecorder) r4Envelope {
	t.Helper()
	var env r4Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("json: %v body=%s", err, rec.Body.String())
	}
	return env
}

func r4DataStatus(t *testing.T, data json.RawMessage) string {
	t.Helper()
	var d struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("data: %v", err)
	}
	return d.Status
}

// TestR4HealthDoesNotDependOnDatabase: liveness is independent of PG.
func TestR4HealthDoesNotDependOnDatabase(t *testing.T) {
	s := &Server{
		Config:      testConfig(),
		Pool:        nil, // unavailable — healthz must not touch it
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	rec := r4GET(r4Router(s), "/healthz")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	env := r4Decode(t, rec)
	if !env.Success {
		t.Fatalf("success=false: %s", rec.Body.String())
	}
	if got := r4DataStatus(t, env.Data); got != "ok" {
		t.Fatalf("status = %q, want ok", got)
	}
}

// TestR4ReadyWithRealPostgres: readiness uses the production Ping path.
func TestR4ReadyWithRealPostgres(t *testing.T) {
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
	rec := r4GET(r4Router(s), "/readyz")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	env := r4Decode(t, rec)
	if !env.Success {
		t.Fatalf("success=false: %s", rec.Body.String())
	}
	if got := r4DataStatus(t, env.Data); got != "ready" {
		t.Fatalf("status = %q, want ready", got)
	}
}

// TestR4ReadyFailsClosedWhenPostgresUnavailable: closed pool → 503
// NOT_READY, no DB error / DSN / credential leakage.
func TestR4ReadyFailsClosedWhenPostgresUnavailable(t *testing.T) {
	url := testDatabaseURL(t)
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	pool.Close()

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	rec := r4GET(r4Router(s), "/readyz")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 body=%s", rec.Code, rec.Body.String())
	}
	env := r4Decode(t, rec)
	if env.Success {
		t.Fatal("success=true on unreadiness")
	}
	if env.Error == nil || env.Error.Code != "NOT_READY" {
		t.Fatalf("error.code = %+v, want NOT_READY", env.Error)
	}
	if env.Error.Message != "Service not ready" {
		t.Fatalf("message = %q", env.Error.Message)
	}
	if len(env.Error.Details) > 0 && string(env.Error.Details) != "null" {
		t.Fatalf("details leaked: %s", env.Error.Details)
	}

	body := rec.Body.String()
	lower := strings.ToLower(body)
	leaks := []string{
		url,
		"postgres://",
		"password",
		"closed pool",
		"ci_pw",
		"dbhost",
		"5432",
	}
	for _, needle := range leaks {
		if needle != "" && strings.Contains(lower, strings.ToLower(needle)) {
			t.Fatalf("response leaked %q: %s", needle, body)
		}
	}
}

// TestR4BusinessPingRemainsAuthenticated: /api/ping is not a public
// health alias after adding /healthz+/readyz.
func TestR4BusinessPingRemainsAuthenticated(t *testing.T) {
	s := &Server{
		Config:      testConfig(),
		Pool:        nil,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	rec := r4GET(r4Router(s), "/api/ping")
	if rec.Code == 200 {
		t.Fatal("/api/ping succeeded without X-Bot-Key")
	}
	env := r4Decode(t, rec)
	if env.Success {
		t.Fatal("/api/ping success without auth")
	}
	if rec.Code != 401 {
		t.Fatalf("unauthenticated ping status = %d, want 401 body=%s", rec.Code, rec.Body.String())
	}
}
