package server

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"kungfu.md/web"
)

// Every HTML surface references assets by content fingerprint, so a
// deploy reaches every browser and proxy cache without manual bumps.
func TestHTMLReferencesFingerprintedAssets(t *testing.T) {
	s := newAdminTestServer(t)
	router := s.buildRouter()
	ref := regexp.MustCompile(`(?:href|src)="(/assets/[^"]+)"`)
	for _, path := range []string{"/", "/credits", "/owner", "/owner/tasks", "/owner/task-guide", "/terms", "/samelabs/login"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/120.0")
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		refs := ref.FindAllStringSubmatch(rec.Body.String(), -1)
		if len(refs) == 0 {
			t.Fatalf("%s: no asset references (status %d)", path, rec.Code)
		}
		for _, m := range refs {
			bare, _, _ := strings.Cut(m[1], "?")
			if m[1] != web.AssetURL(bare) || !strings.Contains(m[1], "?v=") {
				t.Fatalf("%s: %s is not fingerprinted (want %s)", path, m[1], web.AssetURL(bare))
			}
		}
	}
}

func TestFingerprintedAssetCaching(t *testing.T) {
	s := newAdminTestServer(t)
	router := s.buildRouter()
	get := func(url string) string {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 200 {
			t.Fatalf("%s = %d", url, rec.Code)
		}
		return rec.Header().Get("Cache-Control")
	}
	url := web.AssetURL("/assets/owner/core.js")
	if cc := get(url); !strings.Contains(cc, "immutable") {
		t.Fatalf("matching fingerprint %s: Cache-Control %q", url, cc)
	}
	if cc := get("/assets/owner/core.js?v=stale00000"); cc != "no-cache" {
		t.Fatalf("stale fingerprint: Cache-Control %q", cc)
	}
	if cc := get("/assets/owner/core.js"); cc != "no-cache" {
		t.Fatalf("unversioned: Cache-Control %q", cc)
	}
}
