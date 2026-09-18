package server

// S6.5: baseline security headers — one global authority.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

func s65Headers() map[string]string {
	return map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}
}

func s65Router() http.Handler {
	s := &Server{
		Config:      testConfig(),
		Pool:        nil,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	return s.buildRouter()
}

func s65Assert(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k, want := range s65Headers() {
		if got := rec.Header().Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	// CSP and HSTS must NOT be present.
	for _, banned := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only", "Strict-Transport-Security"} {
		if rec.Header().Get(banned) != "" {
			t.Fatalf("%s must not be set", banned)
		}
	}
}

func s65Get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s65Router().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// 1. HTML route (needs a real server: templates render against i18n).
func TestS65HTMLGetsBaselineSecurityHeaders(t *testing.T) {
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
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, httptest.NewRequest("GET", "/owner/login", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String()[:200])
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	s65Assert(t, rec)
}

// 2. Representative JSON API (unauthenticated error is still JSON).
func TestS65APIGetsBaselineSecurityHeaders(t *testing.T) {
	rec := s65Get(t, "/api/ping")
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	s65Assert(t, rec)
}

// 3. Embedded static asset.
func TestS65StaticAssetGetsBaselineSecurityHeaders(t *testing.T) {
	rec := s65Get(t, "/robots.txt")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	s65Assert(t, rec)
}

// 4. 404 error path.
func TestS65ErrorGetsBaselineSecurityHeaders(t *testing.T) {
	rec := s65Get(t, "/no/such/route/xyz")
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	s65Assert(t, rec)
}

// 5. Health/readiness.
func TestS65HealthGetsBaselineSecurityHeaders(t *testing.T) {
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := s65Get(t, p)
		// /readyz 503s with Pool=nil — headers still required.
		if rec.Code != 200 && !(p == "/readyz" && (rec.Code == 503 || rec.Code == 500)) {
			t.Fatalf("%s status = %d", p, rec.Code)
		}
		s65Assert(t, rec)
	}
}

// 6. Single global authority: registered exactly once, no
// handler-local copies of the four approved headers.
func TestS65SingleGlobalHeaderAuthority(t *testing.T) {
	routerSrc := s61Read(t, "internal/server/router.go")
	n := strings.Count(routerSrc, "r.Use(securityHeadersMiddleware)")
	if n != 1 {
		t.Fatalf("securityHeadersMiddleware registered %d times, want 1", n)
	}

	// No production server file other than security_headers.go writes
	// the four approved headers.
	prod := []string{
		"internal/server/router.go", "internal/server/response.go",
		"internal/server/static.go", "internal/server/templates.go",
		"internal/server/handlers.go", "internal/server/handler_agent.go",
		"internal/server/handler_admin.go", "internal/server/handler_admin_b12.go",
		"internal/server/handler_creem.go", "internal/server/handler_owner_store.go",
		"internal/server/handler_admin_store.go", "internal/server/health.go",
		"internal/server/owner_mutation_guard.go",
	}
	for _, rel := range prod {
		src := s61Read(t, rel)
		for h := range s65Headers() {
			if strings.Contains(src, `"`+h+`"`) {
				t.Fatalf("%s writes %s locally — header authority must stay in security_headers.go", rel, h)
			}
		}
	}
}

// 7. Content-Type ownership unchanged.
func TestS65ExistingContentTypesRemainIntact(t *testing.T) {
	if ct := s65Get(t, "/api/ping").Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("json content-type = %q", ct)
	}
	if ct := s65Get(t, "/llms.txt").Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("txt content-type = %q", ct)
	}
	if ct := s65Get(t, "/openai.json").Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("json asset content-type = %q", ct)
	}
}

// 8. Cache-Control ownership unchanged (compose, not replace).
func TestS65ExistingCacheControlRemainIntact(t *testing.T) {
	rec := s65Get(t, "/openai.json") // registered with public, max-age=300
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("cache-control = %q, want the existing static contract", cc)
	}
	s65Assert(t, rec) // security headers compose alongside
}
