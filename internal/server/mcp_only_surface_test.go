package server

import (
	"encoding/json"

	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- MCP-only Agent public surface contract ----
//
// The Agent execution protocol is MCP-only: the former Agent REST
// surface is removed; Owner/Admin/webhook/infra HTTP routes stay.

// removedAgentRESTRoutes is the authoritative list of removed
// Agent-facing REST routes (11 routes).
var removedAgentRESTRoutes = []struct {
	method string
	path   string
}{
	{"POST", "/api/register"},
	{"GET", "/api/ping"},
	{"GET", "/api/kungfus"},
	{"POST", "/api/kungfus"},
	{"GET", "/api/kungfus/somecode"},
	{"DELETE", "/api/kungfus/somecode"},
	{"POST", "/api/kungfus/somecode/share"},
	{"POST", "/api/kungfus/somecode/unshare"},
	{"GET", "/api/tasks"},
	{"GET", "/api/tasks/somecode"},
	{"POST", "/api/tasks/somecode/submissions"},
}

// TestMCPOfflyRemovedAgentRESTSurface: none of the removed Agent REST
// routes answers with a business surface anymore (404 for every method
// and path in the removal list).
func TestMCPOOnlyRemovedAgentRESTSurface(t *testing.T) {
	router := s51Router()
	for _, r := range removedAgentRESTRoutes {
		var body io.Reader
		if r.method == "POST" {
			body = strings.NewReader("{}")
		}
		req := httptest.NewRequest(r.method, r.path, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Bot-Key", "kf_live_anything")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s %s = %d, want 404 (removed)", r.method, r.path, rec.Code)
		}
	}
}

// TestMCPRouteRegistered: /mcp stays mounted on the public router.
func TestMCPRouteRegistered(t *testing.T) {
	// OPTIONS/GET on /mcp must not be a plain 404 (the endpoint exists;
	// protocol-level negotiation is covered by the mcpserver package).
	rec := s51GET(t, s51Router(), "/mcp")
	if rec.Code == 404 {
		t.Fatal("/mcp is not registered")
	}
}

// TestRetainedSurfacesStay: infra probes, Creem webhook, Owner browser
// surface and Admin surface remain.
func TestRetainedSurfacesStay(t *testing.T) {
	router := s51Router()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", 200},
		// /readyz with a nil pool cannot be executed here; its
		// liveness is asserted by the health test package. Here we
		// only require it to be a live route, so exclude it from the
		// exec list below and check registration separately.
		{"POST", "/api/webhooks/creem", 400}, // bad signature, not 404/405
		{"GET", "/api/owner/session", 401},   // unauthenticated JSON, not 404
		{"GET", "/api/samelabs/session", 401},
		{"GET", "/api/account", 401},
		{"POST", "/api/owner/register", 400},    // live owner surface (bad body, not 404/405)
		{"POST", "/api/testtask/somecode", 401}, // owner bot-auth surface stays
	} {
		var req *http.Request
		if tc.method == "POST" {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == 404 || rec.Code == 405 {
			t.Errorf("%s %s = %d, want a live surface", tc.method, tc.path, rec.Code)
		}
	}
	// /readyz is registered (a nil pool would panic on execution, so
	// route presence is asserted via the router's route table).
	router2 := s51Router().(*chi.Mux)
	found := false
	walkErr := chi.Walk(router2, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == "GET" && route == "/readyz" {
			found = true
		}
		return nil
	})
	if walkErr != nil || !found {
		t.Error("/readyz is not registered")
	}
}

// TestDiscoveryAssetsDoNotAdvertiseAgentREST: public discovery files
// carry no Agent REST access path.
func TestDiscoveryAssetsDoNotAdvertiseAgentREST(t *testing.T) {
	router := s51Router()
	for _, path := range []string{"/llms.txt", "/kungfu_skill.md"} {
		rec := s51GET(t, router, path)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := strings.ToLower(rec.Body.String())
		for _, banned := range []string{"x-bot-key", "/api/register", "/api/ping", "/api/kungfus", "/api/tasks"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s advertises removed Agent REST surface %q", path, banned)
			}
		}
	}
	rec := s51GET(t, router, "/openai.json")
	if rec.Code != 200 {
		t.Fatalf("/openai.json = %d", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if _, ok := d["rest_api"]; ok {
		t.Error("openai.json still carries a rest_api block")
	}
	blob := strings.ToLower(rec.Body.String())
	for _, banned := range []string{"x-bot-key", "/api/register", "/api/ping"} {
		if strings.Contains(blob, banned) {
			t.Errorf("openai.json advertises %q", banned)
		}
	}
}

// TestWorkerFacingDiscoveryHidesOwnerReceiver: worker-facing guidance
// never names the owner's PostAPI as the submit target.
func TestWorkerFacingDiscoveryHidesOwnerReceiver(t *testing.T) {
	router := s51Router()
	for _, path := range []string{"/llms.txt", "/kungfu_skill.md"} {
		rec := s51GET(t, router, path)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		low := strings.ToLower(rec.Body.String())
		for _, banned := range []string{"owner's postapi", "owner's configured postapi"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s exposes owner receiver wording %q", path, banned)
			}
		}
	}
}
