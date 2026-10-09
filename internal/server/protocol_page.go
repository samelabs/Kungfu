package server

// GET /protocol, /protocol/zh-CN and /kungfu.md (WO-30 §2): the Kungfu
// Protocol served from the repository-root documents, embedded at
// compile time by the root package — the site never reads spec files
// at runtime. /protocol renders the English spec, /protocol/zh-CN the
// Chinese reference translation, /kungfu.md returns the normative
// English markdown verbatim for agents.
//
// Rendering is server-side goldmark (GFM tables, auto heading ids,
// fenced code). Safety is goldmark's default posture, kept explicit
// here: WithUnsafe is NOT enabled, so raw HTML in the document (a
// <script>, an <img onerror>) is omitted, never executed; dangerous
// link schemes (javascript:, vbscript:, data:) are stripped by the
// default URL policy. The only AST rewrite is the relative-link
// mapper below, which points the spec's own relative links at the
// URLs this site actually serves.

import (
	"bytes"
	"html"
	"net/http"
	"strings"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	rootkungfu "kungfu.md"
	"kungfu.md/internal/i18n"
	"kungfu.md/web"
)

// protocolPageTitle is the fixed <title> of both rendered protocol
// pages (the spec's own Version row: 1.0 — Draft).
const protocolPageTitle = "Kungfu Protocol 1.0 — Draft"

// protocolMarkdown is the single renderer used for both languages:
// GFM tables, automatic heading ids, fenced code blocks, safe-by-
// default HTML output plus the relative-link mapper.
var protocolMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
		parser.WithASTTransformers(util.Prioritized(protocolRelink{}, 100)),
	),
)

// protocolRelink maps the spec's relative links onto URLs this site
// serves. The markdown files keep their links verbatim; only the
// rendered pages rewrite them. Known documents resolve to this site
// (the raw normative file, the rendered Chinese page); any other
// relative target (e.g. docs/conformance.md) resolves to its blob in
// the public repository.
type protocolRelink struct{}

func (protocolRelink) Transform(doc *gast.Document, _ text.Reader, _ parser.Context) {
	_ = gast.Walk(doc, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		// links are container nodes: Walk reports them on enter AND
		// leave — rewrite only on enter, or the rewritten destination
		// would be mapped a second time
		if !entering {
			return gast.WalkContinue, nil
		}
		if link, ok := n.(*gast.Link); ok {
			link.Destination = []byte(protocolLinkTarget(string(link.Destination)))
		}
		return gast.WalkContinue, nil
	})
}

func protocolLinkTarget(dest string) string {
	switch dest {
	case "", "#", "kungfu.md":
		if dest == "kungfu.md" {
			return "/kungfu.md"
		}
		return dest
	case "kungfu.zh-CN.md":
		return "/protocol/zh-CN"
	}
	if strings.HasPrefix(dest, "#") || strings.Contains(dest, "://") {
		return dest
	}
	return "https://github.com/samelabs/Kungfu/blob/main/" + strings.TrimPrefix(dest, "./")
}

// renderProtocolHTML renders one spec markdown document to HTML body
// content.
func renderProtocolHTML(src []byte) (string, error) {
	var buf bytes.Buffer
	if err := protocolMarkdown.Convert(src, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// firstSpecParagraph extracts the spec's first prose paragraph: the
// first paragraph block after the first "## " heading — the opening of
// §0, not the title block or the metadata table. Markdown inline
// marks are stripped; the text itself is verbatim.
func firstSpecParagraph(src []byte) string {
	var lines []string
	seenSection := false
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "## ") {
			seenSection = true
			continue
		}
		if !seenSection {
			continue
		}
		if line == "" {
			if len(lines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") ||
			line == "---" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") {
			if len(lines) > 0 {
				break
			}
			continue
		}
		lines = append(lines, line)
		if len(lines) >= 6 {
			break
		}
	}
	if len(lines) == 0 {
		return protocolPageTitle
	}
	text := strings.Join(lines, " ")
	// strip inline marks: bold/italic/backticks and [text](url) → text
	for _, mark := range []string{"**", "*", "`", "__"} {
		text = strings.ReplaceAll(text, mark, "")
	}
	if i := strings.Index(text, "]("); i >= 0 && strings.HasSuffix(text, ")") {
		label := text[:i]
		if j := strings.LastIndex(label, "["); j >= 0 {
			text = label[:j] + label[j+1:] + text[i+2:len(text)-1]
		}
	}
	return strings.Join(strings.Fields(text), " ")
}

// handleProtocolPage renders GET /protocol (English) and
// GET /protocol/zh-CN (Chinese reference translation). The page
// language follows the URL path, not ?lang=; each page carries the
// language switcher and the normative-translation note.
func (s *Server) handleProtocolPage(lang string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		src, err := rootkungfu.ProtocolEnglish()
		if lang == "zh" {
			src, err = rootkungfu.ProtocolChinese()
		}
		if err != nil {
			http.Error(w, "protocol document unavailable", http.StatusInternalServerError)
			return
		}
		body, err := renderProtocolHTML(src)
		if err != nil {
			http.Error(w, "protocol document unavailable", http.StatusInternalServerError)
			return
		}

		locale := "en"
		path := "/protocol"
		if lang == "zh" {
			locale = "zh"
			path = "/protocol/zh-CN"
		}
		canonical := "https://kungfu.md" + path
		alternates := []headAlternate{
			{Lang: "en", Href: "https://kungfu.md/protocol"},
			{Lang: "zh", Href: "https://kungfu.md/protocol/zh-CN"},
			{Lang: "x-default", Href: "https://kungfu.md/protocol"},
		}

		head := buildHead(headInput{
			Locale:            locale,
			Path:              path,
			TitleOverride:     protocolPageTitle,
			DescOverride:      firstSpecParagraph(src),
			CanonicalOverride: canonical,
			Alternates:        alternates,
			ExtraHead: `<meta name="application-name" content="Kungfu.md">
    <meta name="theme-color" content="#2f7c73">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="alternate" type="text/markdown" href="https://kungfu.md/kungfu.md" title="Kungfu Protocol (normative markdown)">
    <link rel="stylesheet" href="/assets/site.css">
    <link rel="stylesheet" href="/assets/protocol.css">`,
		})

		enCur, zhCur := "", ""
		if lang == "zh" {
			zhCur = ` aria-current="page"`
		} else {
			enCur = ` aria-current="page"`
		}
		page := head + `
<body class="protocol-page">
<div class="wrap protocol-wrap">
    <header class="protocol-top">
        <a class="protocol-brand" href="` + i18n.LocaleURL(locale, "/") + `">
            <span class="site-logo" aria-hidden="true">🥋</span>
            <span>Kungfu<span class="brand-mark">.md</span></span>
        </a>
        <nav class="protocol-langs" aria-label="Protocol language">
            <a href="/protocol"` + enCur + `>English</a>
            <a href="/protocol/zh-CN"` + zhCur + `>简体中文</a>
        </nav>
    </header>
    <p class="protocol-note">` + html.EscapeString(i18n.T(locale, "protocol.note")) + `</p>
    <main class="protocol-doc card">` + body + `</main>
    ` + protocolFooter(locale) + `
</div>
</body>
</html>`
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(web.FingerprintHTML([]byte(page)))
	}
}

// handleKungfuMD serves GET /kungfu.md: the normative English spec
// markdown, byte-for-byte as committed, for agents to read directly.
func (s *Server) handleKungfuMD(w http.ResponseWriter, r *http.Request) {
	src, err := rootkungfu.ProtocolEnglish()
	if err != nil {
		http.Error(w, "protocol document unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(src)
}

// protocolFooter is the shared footer without the ?lang= switcher:
// these pages switch language through their own dedicated URLs.
func protocolFooter(locale string) string {
	return `<footer class="site-footer">
    <div class="site-footer-meta">
        <div class="site-footer-title"><span class="site-logo site-footer-logo" aria-hidden="true">🥋</span><span>Kungfu.md</span></div>
        <div class="site-footer-copy">Copyright © 2026 Kungfu.md. All rights reserved.</div>
        <div class="site-footer-contact">Contact: <a href="mailto:ad@live.it">ad@live.it</a></div>
        <div class="site-footer-legal">
            <a href="` + i18n.LocaleURL(locale, "/terms") + `">Terms</a>
            <a href="` + i18n.LocaleURL(locale, "/privacy") + `">Privacy</a>
            <a href="` + i18n.LocaleURL(locale, "/credits") + `">Credits</a>
        </div>
    </div>
</footer>`
}
