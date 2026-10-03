package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Legal document layout structural contract (frontend/contract closure):
// /terms, /privacy and /credits must render as formal legal documents —
// site header band with home link, lede intro, numbered section
// hierarchy inside a reading-width main — plus the site-wide footer
// legal group. Locks the structure so a regression back to bare text /
// a .card grid is caught.
func TestLegalDocumentLayout(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	for _, path := range []string{"/terms", "/privacy", "/credits"} {
		req := httptest.NewRequest(http.MethodGet, path+"?lang=en", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		body := rec.Body.String()

		for _, want := range []string{
			`class="legal-header"`,      // site-consistent header band
			`class="legal-home"`,        // home link back to the site
			`class="legal-lede"`,        // title + intro hierarchy
			`class="legal-main"`,        // reading-width prose container
			`class="legal-section"`,     // numbered section structure
			`class="legal-section-num"`, // section numbering
			`class="site-footer"`,       // unified site footer
			`class="site-footer-legal"`, // footer legal group
			`<meta name="theme-color"`,  // shared head extras (WO-28)
			`<link rel="manifest"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q — legal document layout regressed", path, want)
			}
		}
		// The old generic-card rendering must NOT be the section container.
		if strings.Contains(body, `<div class="card"><h2>`) {
			t.Errorf("%s: sections still rendered as generic .card grid", path)
		}
		// Header/footer identity: both reference the site name.
		if !strings.Contains(body, `Kungfu.md`) {
			t.Errorf("%s: site identity missing", path)
		}
	}

	// /credits is a public page: no link into a signed-in-only screen
	// except the top-up button (WO-28 A2).
	req := httptest.NewRequest(http.MethodGet, "/credits?lang=en", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/credits: status %d", rec.Code)
	}
	creditsBody := rec.Body.String()
	if strings.Contains(creditsBody, "/owner/rewards") || strings.Contains(creditsBody, "/owner/logs") {
		t.Errorf("/credits links to a signed-in-only screen (owner/rewards or owner/logs)")
	}
	if !strings.Contains(creditsBody, "/owner/credits") {
		t.Errorf("/credits lost the top-up button (/owner/credits)")
	}

	_, _ = s.Pool.Exec(context.Background(), `SELECT 1`) // keep pool warm lint-free
}
