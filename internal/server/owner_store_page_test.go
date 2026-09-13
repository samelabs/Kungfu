package server

// Owner Store page wiring tests: the /owner/store route renders the Owner
// Workspace store section inside the existing shell (nav, scripts, auth),
// with active i18n keys for every supported locale.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerStorePageRendersSection(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner/store", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// Section renders inside the existing Owner Workspace shell, not a
	// second HTML app.
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %s", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, `data-section="store"`) {
		t.Fatal("store section marker missing")
	}
	if !strings.Contains(body, "id=\"storeProducts\"") {
		t.Fatal("store products container missing")
	}
	if !strings.Contains(body, "id=\"storeResult\"") {
		t.Fatal("store result container missing")
	}
	if !strings.Contains(body, "id=\"storeBalance\"") {
		t.Fatal("store balance element missing")
	}

	// Store nav link present.
	if !strings.Contains(body, "/owner/store") {
		t.Fatal("store nav link missing")
	}

	// Store JS modules referenced.
	if !strings.Contains(body, "/assets/owner/store.js") {
		t.Fatal("store.js reference missing")
	}
	if !strings.Contains(body, "/assets/owner/render-store.js") {
		t.Fatal("render-store.js reference missing")
	}
}

func TestOwnerStorePageDoesNotRegressOverviewSection(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	// The store section renders its own view; the overview page keeps
	// rendering the overview view (section switch does not regress).
	for path, section := range map[string]string{
		"/owner":       "overview",
		"/owner/store": "store",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `data-section="`+section+`"`) {
			t.Fatalf("%s: expected data-section=%q", path, section)
		}
	}

	// Unknown pages are a 404 (router-level), never a silent overview.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner/nonexistent-section-page", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown page = %d, want 404", rec.Code)
	}
}

func TestOwnerNavContainsStore(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner", nil)
	router.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "/owner/store") {
		t.Fatal("overview page nav lacks Store link")
	}
	if !strings.Contains(body, ">Store</a>") {
		t.Fatal("english locale nav lacks Store label")
	}
}
