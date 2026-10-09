package server

// WO-19 S5: the unified head carries canonical, hreflang, Open Graph
// and Twitter tags on the public pages; the owner surface stays
// noindex; robots.txt keeps crawlers out of the private planes.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/web"
)

func seoGet(t *testing.T, s *Server, target string) string {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	// a browser UA: "/" routes plain HTTP clients to llms.txt
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120.0")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s = %d", target, rec.Code)
	}
	return rec.Body.String()
}

func TestHomePageHeadSEOMeta(t *testing.T) {
	s := storeTestServer(t)
	body := seoGet(t, s, "/")

	for _, want := range []string{
		`<link rel="canonical" href="https://kungfu.md/">`,
		`<link rel="alternate" hreflang="en" href="https://kungfu.md/">`,
		`<link rel="alternate" hreflang="zh" href="https://kungfu.md/?lang=zh">`,
		`<link rel="alternate" hreflang="ja" href="https://kungfu.md/?lang=ja">`,
		`<link rel="alternate" hreflang="ko" href="https://kungfu.md/?lang=ko">`,
		`<link rel="alternate" hreflang="es" href="https://kungfu.md/?lang=es">`,
		`<link rel="alternate" hreflang="x-default" href="https://kungfu.md/">`,
		`<meta property="og:title" content="Kungfu — A Protocol for Persistent Agent Work | Kungfu.md">`,
		`<meta property="og:url" content="https://kungfu.md/">`,
		`<meta property="og:image" content="https://kungfu.md/assets/icons/app-icon-512.png">`,
		`<meta property="og:image:width" content="512">`,
		`<meta property="og:image:height" content="512">`,
		`<meta property="og:image:alt" content="Kungfu.md logo">`,
		`<meta property="og:locale" content="en_US">`,
		`<meta name="twitter:card" content="summary">`,
		`<meta name="twitter:title" content="Kungfu — A Protocol for Persistent Agent Work | Kungfu.md">`,
		`<meta name="twitter:image" content="https://kungfu.md/assets/icons/app-icon-512.png">`,
		`<html lang="en">`,
		// JSON-LD (S2): WebSite SearchAction + Organization; the
		// WebSite carries the English seo.home_desc description
		// (WO-30 §1.2)
		`<script type="application/ld+json">`,
		`"SearchAction"`,
		`"urlTemplate":"https://kungfu.md/?q={search_term_string}"`,
		`"description":"Kungfu is an open protocol for agent work that outlives the session: versioned memory, persistent threads and work contracts. kungfu.md runs its open-source reference implementation over MCP and HTTP."`,
		`"Organization"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("homepage head missing %q", want)
		}
	}

	// a non-en locale: canonical, og:url and <html lang> follow it
	zh := seoGet(t, s, "/?lang=zh")
	for _, want := range []string{
		`<link rel="canonical" href="https://kungfu.md/?lang=zh">`,
		`<meta property="og:url" content="https://kungfu.md/?lang=zh">`,
		`<meta property="og:locale" content="zh_CN">`,
		`<html lang="zh">`,
	} {
		if !strings.Contains(zh, want) {
			t.Fatalf("homepage (?lang=zh) head missing %q", want)
		}
	}
}

func TestLegalPageHeadSEOMeta(t *testing.T) {
	s := storeTestServer(t)
	body := seoGet(t, s, "/terms")

	for _, want := range []string{
		`<link rel="canonical" href="https://kungfu.md/terms">`,
		`<link rel="alternate" hreflang="es" href="https://kungfu.md/terms?lang=es">`,
		`<link rel="alternate" hreflang="x-default" href="https://kungfu.md/terms">`,
		`<meta property="og:title" content="Terms of Service | Kungfu.md">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:site_name" content="Kungfu.md">`,
		`<meta name="twitter:card" content="summary">`,
		`<meta name="twitter:description"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/terms head missing %q", want)
		}
	}
}

func TestOwnerPagesNoIndex(t *testing.T) {
	s := storeTestServer(t)
	for _, target := range []string{"/owner", "/owner/tasks", "/owner/register"} {
		if body := seoGet(t, s, target); !strings.Contains(body, `<meta name="robots" content="noindex,nofollow">`) {
			t.Fatalf("%s must carry noindex,nofollow", target)
		}
	}
}

func TestRobotsAndSitemap(t *testing.T) {
	robots, err := web.StaticFile("robots.txt")
	if err != nil {
		t.Fatalf("robots.txt: %v", err)
	}
	for _, want := range []string{
		"Disallow: /owner", "Disallow: /samelabs",
		"Disallow: /api/", "Disallow: /mcp",
		"Sitemap: https://kungfu.md/sitemap.xml",
	} {
		if !strings.Contains(string(robots), want) {
			t.Fatalf("robots.txt missing %q", want)
		}
	}

	sitemap, err := web.StaticFile("sitemap.xml")
	if err != nil {
		t.Fatalf("sitemap.xml: %v", err)
	}
	for _, want := range []string{
		"https://kungfu.md/",
		"https://kungfu.md/credits",
		"https://kungfu.md/terms",
		"https://kungfu.md/privacy",
		"https://kungfu.md/protocol",
		"https://kungfu.md/protocol/zh-CN",
		"https://kungfu.md/kungfu.md",
		"https://kungfu.md/llms.txt",
		"https://kungfu.md/task-guide.md",
		"https://kungfu.md/kungfu_skill.md",
		"https://kungfu.md/openai.json",
		`<xhtml:link rel="alternate" hreflang="zh" href="https://kungfu.md/?lang=zh"/>`,
		`<xhtml:link rel="alternate" hreflang="x-default" href="https://kungfu.md/credits"/>`,
		`<xhtml:link rel="alternate" hreflang="zh" href="https://kungfu.md/protocol/zh-CN"/>`,
	} {
		if !strings.Contains(string(sitemap), want) {
			t.Fatalf("sitemap.xml missing %q", want)
		}
	}
}
