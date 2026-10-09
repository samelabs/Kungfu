package server

// WO-30 §2/§5: the /protocol pages and /kungfu.md serve the
// repository-root spec documents embedded at compile time; rendering
// is server-side, inert for raw HTML and dangerous link schemes; the
// pages carry the protocol head (fixed title, first-paragraph
// description, en/zh/x-default hreflang) and the language switcher
// with the reference-translation note.

import (
	"net/http/httptest"
	"strings"
	"testing"

	rootkungfu "kungfu.md"
)

func protoGet(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	s.buildRouter().ServeHTTP(rec, req)
	return rec
}

func TestProtocolPagesServeEmbeddedSpec(t *testing.T) {
	s := storeTestServer(t)

	en := protoGet(t, s, "/protocol")
	if en.Code != 200 {
		t.Fatalf("/protocol = %d", en.Code)
	}
	enBody := en.Body.String()
	for _, want := range []string{
		"<title>Kungfu Protocol 1.0 — Draft</title>",
		`<link rel="canonical" href="https://kungfu.md/protocol">`,
		`<link rel="alternate" hreflang="en" href="https://kungfu.md/protocol">`,
		`<link rel="alternate" hreflang="zh" href="https://kungfu.md/protocol/zh-CN">`,
		`<link rel="alternate" hreflang="x-default" href="https://kungfu.md/protocol">`,
		// meta description is the spec's first paragraph (§0 opening)
		"An agent works in sessions. A session begins, the agent acts, the session ends",
		// rendered document: heading anchors and tables (the spec
		// itself carries no fenced code blocks)
		`<h1 id="kungfu">`,
		"<table>",
		// the language switcher and the reference-translation note
		`<a href="/protocol/zh-CN">简体中文</a>`,
		"The English text is normative; the Chinese translation is a reference translation.",
	} {
		if !strings.Contains(enBody, want) {
			t.Fatalf("/protocol missing %q", want)
		}
	}

	zh := protoGet(t, s, "/protocol/zh-CN")
	if zh.Code != 200 {
		t.Fatalf("/protocol/zh-CN = %d", zh.Code)
	}
	zhBody := zh.Body.String()
	for _, want := range []string{
		"<title>Kungfu Protocol 1.0 — Draft</title>",
		`<link rel="canonical" href="https://kungfu.md/protocol/zh-CN">`,
		`<html lang="zh">`,
		"轮次与恢复",
		`<a href="/protocol">English</a>`,
		"英文版为规范文本；中文版为参考译本。",
	} {
		if !strings.Contains(zhBody, want) {
			t.Fatalf("/protocol/zh-CN missing %q", want)
		}
	}

	// the raw normative file: byte-for-byte the embedded repo document
	raw := protoGet(t, s, "/kungfu.md")
	if raw.Code != 200 {
		t.Fatalf("/kungfu.md = %d", raw.Code)
	}
	if got := raw.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("/kungfu.md content-type = %q", got)
	}
	embedded, err := rootkungfu.ProtocolEnglish()
	if err != nil {
		t.Fatalf("root embed: %v", err)
	}
	if raw.Body.String() != string(embedded) {
		t.Fatal("/kungfu.md body differs from the embedded kungfu.md")
	}
}

// TestProtocolRenderingIsInjectionSafe: markdown content is content —
// raw HTML is omitted, never executed; dangerous link schemes are
// stripped; the spec's own relative links map onto URLs this site
// serves.
func TestProtocolRenderingIsInjectionSafe(t *testing.T) {
	evil := "# Title\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n" +
		"[evil](javascript:alert(3))\n\n[data](data:text/html;base64,PHNjcmlwdD4=)\n\n" +
		"[ok](https://example.com/safe)\n\n[doc](docs/conformance.md)\n\n[zhtwin](kungfu.zh-CN.md)\n"
	out, err := renderProtocolHTML([]byte(evil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "<script") {
		t.Fatal("rendered output carries a raw <script> tag")
	}
	if strings.Contains(out, "onerror") {
		t.Fatal("rendered output carries a raw event-handler attribute")
	}
	if strings.Contains(out, `href="javascript:`) || strings.Contains(out, `href="data:`) {
		t.Fatal("rendered output carries a dangerous link scheme")
	}
	if !strings.Contains(out, "raw HTML omitted") {
		t.Fatal("raw HTML should be omitted with a marker, not passed through")
	}
	if !strings.Contains(out, `href="https://example.com/safe"`) {
		t.Fatal("an ordinary https link must survive rendering")
	}
	if !strings.Contains(out, "https://github.com/samelabs/Kungfu/blob/main/docs/conformance.md") {
		t.Fatal("an unknown relative link must map to the public repository")
	}
	if !strings.Contains(out, `href="/protocol/zh-CN"`) {
		t.Fatal("the spec's zh twin link must map to the rendered Chinese page")
	}

	// the served pages themselves contain no script at all
	s := storeTestServer(t)
	for _, target := range []string{"/protocol", "/protocol/zh-CN"} {
		body := protoGet(t, s, target).Body.String()
		if strings.Contains(strings.ToLower(body), "<script") {
			t.Fatalf("%s carries a <script> tag", target)
		}
	}
}

// TestSpecFirstParagraph feeds a synthetic document and checks the
// extractor: the first paragraph after the first "## " heading, with
// inline marks stripped.
func TestSpecFirstParagraph(t *testing.T) {
	doc := "# Kungfu\n\n**A Protocol for Persistent Agent Work**\n\n| | |\n|---|---|\n| Version | 1.0 |\n\n---\n\n## 0. Introduction\n\nFirst real paragraph line one\ncontinues on two lines.\n\nSecond paragraph.\n"
	got := firstSpecParagraph([]byte(doc))
	if got != "First real paragraph line one continues on two lines." {
		t.Fatalf("firstSpecParagraph = %q", got)
	}
}
