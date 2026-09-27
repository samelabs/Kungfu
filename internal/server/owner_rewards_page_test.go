package server

// Owner Rewards page wiring tests: the /owner/rewards route renders the Owner
// Workspace rewards section inside the existing shell (nav, scripts, auth),
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
	req := httptest.NewRequest(http.MethodGet, "/owner/rewards", nil)
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
	if !strings.Contains(body, `data-section="rewards"`) {
		t.Fatal("rewards section marker missing")
	}
	if !strings.Contains(body, "id=\"rewardsProducts\"") {
		t.Fatal("rewards products container missing")
	}
	if !strings.Contains(body, "id=\"rewardsResult\"") {
		t.Fatal("rewards result container missing")
	}
	if !strings.Contains(body, "id=\"rewardsBalance\"") {
		t.Fatal("rewards balance element missing")
	}

	// Rewards nav link present.
	if !strings.Contains(body, "/owner/rewards") {
		t.Fatal("rewards nav link missing")
	}

	// Rewards JS modules referenced.
	if !strings.Contains(body, "/assets/owner/rewards.js") {
		t.Fatal("rewards.js reference missing")
	}
	if !strings.Contains(body, "/assets/owner/render-rewards.js") {
		t.Fatal("render-rewards.js reference missing")
	}
}

func TestOwnerStorePageDoesNotRegressOverviewSection(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	// The rewards section renders its own view; the overview page keeps
	// rendering the overview view (section switch does not regress).
	for path, section := range map[string]string{
		"/owner":         "overview",
		"/owner/rewards": "rewards",
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
	if !strings.Contains(body, "/owner/rewards") {
		t.Fatal("overview page nav lacks Rewards link")
	}
	if !strings.Contains(body, ">Rewards</a>") {
		t.Fatal("english locale nav lacks Rewards label")
	}
}
