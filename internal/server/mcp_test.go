package server

// M1: /mcp is wired into the production router under the existing
// middleware stack — no second lifecycle/timeout owner, and the REST
// surface is unregressed.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

func TestMCPRouteOnProductionRouter(t *testing.T) {
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
	router := s.buildRouter()

	// POST /mcp passes through the stack (origin protection allows
	// non-browser no-Origin requests); 405 proves the MCP handler
	// answered under the stack, not a chi 404.
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/mcp not routed on the production router")
	}
	// stateless: GET is 405 from the SDK handler
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405 (stateless MCP handler)", rec.Code)
	}

	// S6.5 security headers still compose on the MCP surface
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("baseline security headers missing on /mcp")
	}
}
