package server

// The public content pages /terms, /privacy and /credits share ONE
// structure (WO-28): the legal document layout — unified head extras,
// header band with the home link, lede (h1 + intro), numbered
// legal-section blocks, site footer. Legal documents are long-form
// reading surfaces and must look like one on desktop and mobile; /credits
// uses the same shell so the three pages read as one family. A public
// page links to no signed-in-only screen except the top-up button.

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"kungfu.md/internal/i18n"
	"kungfu.md/web"
)

// publicContentHeadExtras is the shared head block of the three public
// content pages: theme-color, manifest, favicons, site.css — one set,
// identical on /terms, /privacy and /credits.
const publicContentHeadExtras = `<meta name="theme-color" content="#2f7c73">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="stylesheet" href="/assets/site.css">`

// legalSection is one numbered block of a legal-layout page. BodyHTML
// arrives pre-built (already HTML-escaped at every interpolation) from
// the caller; Heading is escaped here.
type legalSection struct {
	Heading  string
	BodyHTML string
}

// renderLegalLayout writes one page in the legal document layout: the
// unified head extras, the header band with the home link, the lede
// (h1 + intro), the numbered sections, the site footer. The single
// shell behind renderLegalPage and renderCredits.
func renderLegalLayout(w http.ResponseWriter, data *tmplData, path, titleKey, descKey, heading, intro, langSwitchID string, sections []legalSection) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	langOpts := buildLangOptionsHTML(data.LangOptions, data.Locale, path)

	var body strings.Builder
	for i, sec := range sections {
		body.WriteString(`<section class="legal-section" id="s` + strconv.Itoa(i+1) + `">
            <h2><span class="legal-section-num">` + strconv.Itoa(i+1) + `</span>` + html.EscapeString(sec.Heading) + `</h2>
            ` + sec.BodyHTML + `
        </section>`)
	}

	htmlOut := buildHead(headInput{
		Locale:    data.Locale,
		Path:      path,
		TitleKey:  titleKey,
		DescKey:   descKey,
		ExtraHead: publicContentHeadExtras,
	}) + `
<body class="legal-body">
<div class="wrap legal-wrap">
    <header class="legal-header">
        <a class="legal-home" href="` + i18n.LocaleURL(data.Locale, "/") + `"><span class="site-logo" aria-hidden="true">🥋</span>Kungfu.md</a>
        <div class="legal-header-meta">` + html.EscapeString(heading) + `</div>
    </header>
    <main class="legal-main">
        <div class="legal-lede">
            <h1>` + html.EscapeString(heading) + `</h1>
            <p class="legal-intro">` + html.EscapeString(intro) + `</p>
        </div>
        ` + body.String() + `
    </main>
    ` + siteFooter(data.Locale, langOpts, langSwitchID) + `
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`
	w.Write(web.FingerprintHTML([]byte(htmlOut)))
}

// renderLegalPage renders /terms or /privacy from i18n content — a
// single template source for all five locales. Sections render while
// the i18n keys exist (s0, s1, …): adding or folding a section is a
// locales.json edit, never a renderer change.
func (s *Server) renderLegalPage(w http.ResponseWriter, data *tmplData, kind string) {
	heading := data.T(kind + ".heading")
	intro := data.T(kind + ".intro")
	var sections []legalSection
	for i := 0; ; i++ {
		hKey := fmt.Sprintf("%s.s%d_h", kind, i)
		bKey := fmt.Sprintf("%s.s%d_b", kind, i)
		if !i18n.Has(data.Locale, hKey) && !i18n.Has(data.Locale, bKey) {
			break
		}
		if !i18n.Has(data.Locale, hKey) || !i18n.Has(data.Locale, bKey) {
			continue
		}
		sections = append(sections, legalSection{
			Heading:  data.T(hKey),
			BodyHTML: "<p>" + html.EscapeString(data.T(bKey)) + "</p>",
		})
	}
	renderLegalLayout(w, data, "/"+kind,
		"seo."+kind+"_title", "seo."+kind+"_desc",
		heading, intro, kind+"-lang-switch", sections)
}

// renderCredits renders the public credits explainer page in the same
// legal document layout as /terms and /privacy. It is a static public
// page — no session/account fetch; balances live in the Owner
// Workspace. The only signed-in-only link is the top-up button
// (/owner/credits); reward redemption and credit activity stay behind
// the workspace's own navigation.
func (s *Server) renderCredits(w http.ResponseWriter, r *http.Request, data *tmplData) {
	pkgs := s.publicCreditsPackages(r.Context())
	t := data.T

	var packages strings.Builder
	if len(pkgs) == 0 {
		packages.WriteString(`<p>` + html.EscapeString(t("home.credits_soon")) + `</p>`)
	} else {
		packages.WriteString(`<div class="credits-packages">`)
		for _, p := range pkgs {
			packages.WriteString(`<div class="credits-package"><b>` + html.EscapeString(p.Name) + `</b>` +
				`<span>` + html.EscapeString(fmt.Sprintf("%d %s", p.Credits, t("home.credits_unit"))) + `</span>` +
				`<span class="credits-price">` + html.EscapeString(minorAmount(p.AmountMinor, p.Currency)) + `</span></div>`)
		}
		packages.WriteString(`</div>`)
	}
	packages.WriteString(`<div class="actions"><a class="btn primary" href="` + i18n.LocaleURL(data.Locale, "/owner/credits") + `">` +
		html.EscapeString(t("home.credits_buy")) + `</a></div>`)

	refunds := "<p>" + html.EscapeString(t("credits.refunds_creem")) + " " + html.EscapeString(t("home.credits_nontransfer")) + "</p>" +
		"<p>" + html.EscapeString(t("credits.refunds_reversal")) + "</p>" +
		"<p>" + html.EscapeString(t("credits.full_terms_label")) +
		` <a href="` + i18n.LocaleURL(data.Locale, "/terms") + `">` + html.EscapeString(t("home.credits_terms")) + `</a>` +
		` · <a href="` + i18n.LocaleURL(data.Locale, "/privacy") + `">` + html.EscapeString(t("home.credits_privacy")) + `</a></p>`

	sections := []legalSection{
		{Heading: t("credits.packages_h"), BodyHTML: packages.String()},
		{Heading: t("credits.sources_h"), BodyHTML: "<p>" + html.EscapeString(t("credits.sources_signup")) + "</p>" +
			"<p>" + html.EscapeString(t("credits.sources_topups")) + "</p>" +
			"<p>" + html.EscapeString(t("credits.sources_work")) + "</p>"},
		{Heading: t("credits.use_h"), BodyHTML: "<p>" + html.EscapeString(t("credits.use_budget")) + "</p>" +
			"<p>" + html.EscapeString(t("credits.use_redemptions")) + "</p>"},
		{Heading: t("credits.refunds_h"), BodyHTML: refunds},
		{Heading: t("credits.one_balance_h"), BodyHTML: "<p>" + html.EscapeString(t("credits.balance_explainer")) + "</p>"},
	}
	renderLegalLayout(w, data, "/credits",
		"seo.credits_title", "seo.credits_desc",
		t("credits.title"), t("credits.summary"), "credits-lang-switch", sections)
}
