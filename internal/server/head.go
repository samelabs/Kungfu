package server

// Unified HTML head generation (WO-19 S1): one builder emits the
// shared head of every server-rendered page — a localized <title> and
// meta description (seo.* keys, one set per page per language), a
// canonical address (?lang= except en), the hreflang set
// (en/zh/ja/ko/es + x-default), Open Graph and Twitter card tags with
// the 512px app icon as the share image (180 KB: under the ~300 KB
// ceiling some messengers apply to link-preview images), and <html lang> matching
// the display locale. Pages pass their own extras (stylesheets,
// manifest, alternates, JSON-LD); private surfaces (owner, samelabs)
// set NoIndex.

import (
	"encoding/json"
	"html"
	"strings"

	"kungfu.md/internal/i18n"
)

const (
	siteCanonicalBase = "https://kungfu.md"
	ogImageURL        = "https://kungfu.md/assets/icons/app-icon-512.png"
	ogImageWidth      = "512"
	ogImageHeight     = "512"
	ogImageAlt        = "Kungfu.md logo"
)

// ogLocaleByLang maps a supported locale to its Open Graph locale tag.
var ogLocaleByLang = map[string]string{
	"en": "en_US",
	"zh": "zh_CN",
	"ja": "ja_JP",
	"ko": "ko_KR",
	"es": "es_ES",
}

// headInput is one page's head request.
type headInput struct {
	Locale    string // resolved display locale
	Path      string // canonical path, e.g. "/" or "/credits"
	TitleKey  string // i18n key of the page title (suffix added here)
	DescKey   string // i18n key of the meta description
	NoIndex   bool   // private surfaces (owner, samelabs)
	ExtraHead string // page-specific lines, already HTML-escaped
}

// canonicalURL is a page's canonical address: the bare path for en,
// the path plus ?lang= for every other locale.
func canonicalURL(locale, path string) string {
	if locale == "" || locale == "en" {
		return siteCanonicalBase + path
	}
	return siteCanonicalBase + path + "?lang=" + locale
}

// buildHead renders <!DOCTYPE html> through </head>; the caller opens
// its own <body> (classes differ per surface).
func buildHead(in headInput) string {
	locale := i18n.NormalizeLocale(in.Locale)
	if locale == "" {
		locale = "en"
	}
	robots := "index,follow,max-image-preview:large"
	if in.NoIndex {
		robots = "noindex,nofollow"
	}
	title := html.EscapeString(i18n.T(locale, in.TitleKey))
	fullTitle := title + " | Kungfu.md"
	desc := html.EscapeString(i18n.T(locale, in.DescKey))
	canonical := html.EscapeString(canonicalURL(locale, in.Path))
	ogLocale := ogLocaleByLang[locale]

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="` + html.EscapeString(locale) + `">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>` + fullTitle + `</title>
    <meta name="description" content="` + desc + `">
    <meta name="robots" content="` + robots + `">
    <link rel="canonical" href="` + canonical + `">`)
	for _, code := range i18n.SupportedLocales() {
		b.WriteString("\n    " + `<link rel="alternate" hreflang="` + code + `" href="` +
			html.EscapeString(canonicalURL(code, in.Path)) + `">`)
	}
	b.WriteString("\n    " + `<link rel="alternate" hreflang="x-default" href="` +
		html.EscapeString(canonicalURL("en", in.Path)) + `">`)
	b.WriteString(`
    <meta property="og:site_name" content="Kungfu.md">
    <meta property="og:type" content="website">
    <meta property="og:title" content="` + fullTitle + `">
    <meta property="og:description" content="` + desc + `">
    <meta property="og:url" content="` + canonical + `">
    <meta property="og:locale" content="` + ogLocale + `">
    <meta property="og:image" content="` + ogImageURL + `">
    <meta property="og:image:width" content="` + ogImageWidth + `">
    <meta property="og:image:height" content="` + ogImageHeight + `">
    <meta property="og:image:type" content="image/png">
    <meta property="og:image:alt" content="` + ogImageAlt + `">
    <meta name="twitter:card" content="summary">
    <meta name="twitter:title" content="` + fullTitle + `">
    <meta name="twitter:description" content="` + desc + `">
    <meta name="twitter:image" content="` + ogImageURL + `">
    <meta name="twitter:image:alt" content="` + ogImageAlt + `">`)
	if in.ExtraHead != "" {
		b.WriteString("\n    " + in.ExtraHead)
	}
	b.WriteString("\n</head>")
	return b.String()
}

// homeJSONLD is the homepage's structured data (WO-19 S2): a
// WebSite with a SearchAction on the task board's ?q= parameter, and
// the site Organization. Marshalled, never string-built.
func homeJSONLD() string {
	website := map[string]any{
		"@context": "https://schema.org",
		"@type":    "WebSite",
		"name":     "Kungfu.md",
		"url":      siteCanonicalBase + "/",
		"potentialAction": map[string]any{
			"@type":       "SearchAction",
			"target":      map[string]any{"@type": "EntryPoint", "urlTemplate": siteCanonicalBase + "/?q={search_term_string}"},
			"query-input": "required name=search_term_string",
		},
	}
	org := map[string]any{
		"@context": "https://schema.org",
		"@type":    "Organization",
		"name":     "Kungfu.md",
		"url":      siteCanonicalBase + "/",
		"logo":     ogImageURL,
	}
	rawSite, _ := json.Marshal(website)
	rawOrg, _ := json.Marshal(org)
	return `<script type="application/ld+json">` + string(rawSite) + `</script>` + "\n    " +
		`<script type="application/ld+json">` + string(rawOrg) + `</script>`
}
