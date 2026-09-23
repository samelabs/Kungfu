package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Homepage UA routing regressions (frontend/contract closure ticket):
// Normal browsers AND search crawlers (Googlebot, Bingbot) must receive the
// canonical human HTML homepage. Only explicit agent/CLI client signatures
// are routed to the llms.txt discovery surface, which must also remain
// directly reachable at /llms.txt for every client.
func TestHomepageUARouting(t *testing.T) {
	s := newLogoutTestServer(t)
	router := s.buildRouter()

	fetch := func(path, ua, accept string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	isHumanHTML := func(rec *httptest.ResponseRecorder) bool {
		body := rec.Body.String()
		// The human homepage is HTML; the agent discovery surface is
		// llms.txt plain text. Assert both directions.
		return strings.Contains(rec.Header().Get("Content-Type"), "text/html") &&
			!strings.Contains(body, "# kungfu.md — Agent Discovery Index") // llms.txt title marker
	}
	isAgentDiscovery := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Header().Get("Content-Type"), "text/plain") &&
			strings.Contains(rec.Body.String(), "# kungfu.md — Agent Discovery Index")
	}

	cases := []struct {
		name    string
		ua      string
		accept  string
		humanly bool
	}{
		{"normal_chrome", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36", "text/html,application/xhtml+xml", true},
		{"normal_safari_ios", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "text/html", true},
		{"googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "text/html", true},
		{"bingbot", "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)", "text/html", true},
		{"curl", "curl/8.5.0", "", false},
		{"wget", "Wget/1.21.4", "", false},
		{"python_requests", "python-requests/2.31.0", "", false},
		{"go_http", "Go-http-client/2.0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := fetch("/", tc.ua, tc.accept)
			if rec.Code != 200 {
				t.Fatalf("GET /: status %d", rec.Code)
			}
			if tc.humanly && !isHumanHTML(rec) {
				t.Fatalf("UA %q expected human HTML homepage, got content-type=%q body-prefix=%q",
					tc.ua, rec.Header().Get("Content-Type"), truncate(rec.Body.String(), 80))
			}
			if !tc.humanly && !isAgentDiscovery(rec) {
				t.Fatalf("UA %q expected agent discovery (llms.txt), got content-type=%q body-prefix=%q",
					tc.ua, rec.Header().Get("Content-Type"), truncate(rec.Body.String(), 80))
			}
		})
	}

	// /llms.txt stays a dedicated agent discovery surface for ANY client.
	t.Run("explicit_llms_txt_browser", func(t *testing.T) {
		rec := fetch("/llms.txt", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0", "text/html")
		if rec.Code != 200 || !isAgentDiscovery(rec) {
			t.Fatalf("GET /llms.txt with browser UA: status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
