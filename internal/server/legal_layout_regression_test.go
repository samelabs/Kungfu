package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Legal document layout structural contract (frontend/contract closure):
// /terms and /privacy must render as formal legal documents — site header
// band with home link, lede intro, numbered section hierarchy inside a
// reading-width main — plus the site-wide footer legal group. Locks the
// structure so a regression back to bare text / a .card grid is caught.
func TestLegalDocumentLayout(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	for _, path := range []string{"/terms", "/privacy"} {
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
	_, _ = s.Pool.Exec(context.Background(), `SELECT 1`) // keep pool warm lint-free
}
