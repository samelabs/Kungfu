package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/ratelimit"
)

func s51Router() http.Handler {
	s := &Server{
		Config:      testConfig(),
		Pool:        nil,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	return s.buildRouter()
}

func s51GET(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// TestS51OpenAIDescriptorRoutesUseSingleCanonicalSource: both public
// descriptor URLs return the same embedded openai.json bytes.
func TestS51OpenAIDescriptorRoutesUseSingleCanonicalSource(t *testing.T) {
	router := s51Router()
	a := s51GET(t, router, "/openai.json")
	b := s51GET(t, router, "/.well-known/openai.json")

	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("status /openai.json=%d /.well-known/openai.json=%d, want 200/200", a.Code, b.Code)
	}
	if ct := a.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("/openai.json Content-Type = %q", ct)
	}
	if ct := b.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("/.well-known/openai.json Content-Type = %q", ct)
	}
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatal("descriptor route bodies are not exactly equal")
	}
	if !json.Valid(a.Body.Bytes()) {
		t.Fatal("canonical descriptor is not JSON")
	}
}

// TestS51OpenAIDescriptorIdentity locks identity fields, not copy.
func TestS51OpenAIDescriptorIdentity(t *testing.T) {
	rec := s51GET(t, s51Router(), "/openai.json")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var d struct {
		SchemaVersion string `json:"schema_version"`
		Name          string `json:"name"`
		Homepage      string `json:"homepage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("json: %v", err)
	}
	if d.SchemaVersion != "1.0" {
		t.Fatalf("schema_version = %q", d.SchemaVersion)
	}
	if d.Name != "Kungfu.md" {
		t.Fatalf("name = %q, want Kungfu.md", d.Name)
	}
	if d.Homepage != "https://kungfu.md/" {
		t.Fatalf("homepage = %q", d.Homepage)
	}
}

// TestS51BusinessPingStillNotHealthAlias: /api/ping stays authenticated.
func TestS51BusinessPingStillNotHealthAlias(t *testing.T) {
	rec := s51GET(t, s51Router(), "/api/ping")
	if rec.Code != 401 {
		t.Fatalf("/api/ping without X-Bot-Key = %d, want 401 body=%s", rec.Code, rec.Body.String())
	}
}
