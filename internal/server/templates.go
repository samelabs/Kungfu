package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kungfu.md/internal/i18n"
	"kungfu.md/internal/payment"
	"kungfu.md/internal/repository"
	"kungfu.md/web"
)

// tmplData holds all variables passed to HTML templates.
type tmplData struct {
	Locale      string
	LangOptions []map[string]string
	Section     string
	T           func(string) string
}

// renderTemplate renders an HTML page with i18n data.
func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request, page, section string) {
	locale := i18n.ResolveLocale(r)
	if q := i18n.NormalizeLocale(r.URL.Query().Get("lang")); q != "" && i18n.IsSupported(q) {
		i18n.SetLangCookie(w, q)
	}
	data := &tmplData{
		Locale:      locale,
		LangOptions: i18n.LanguageOptions(locale),
		Section:     section,
		T:           func(key string) string { return i18n.T(locale, key) },
	}
	switch page {
	case "home":
		s.renderHome(w, r, data)
	case "credits":
		s.renderCredits(w, r, data)
	case "owner":
		s.renderOwner(w, data)
	case "terms":
		s.renderLegalPage(w, data, "terms")
	case "privacy":
		s.renderLegalPage(w, data, "privacy")
	}
}

// renderLegalPage renders /terms or /privacy from i18n content — a
// single template source for all five locales. The page uses the
// dedicated legal document layout (header band, intro lede, numbered
// section hierarchy, reading-width prose), NOT the generic card grid:
// legal documents are long-form reading surfaces and must look like one
// on desktop and mobile.
func (s *Server) renderLegalPage(w http.ResponseWriter, data *tmplData, kind string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	langOpts := buildLangOptionsHTML(data.LangOptions, data.Locale, "/"+kind)

	var sections strings.Builder
	for i := 0; i < 7; i++ {
		h := data.T(fmt.Sprintf(kind+".s%d_h", i))
		b := data.T(fmt.Sprintf(kind+".s%d_b", i))
		if h == "" || b == "" {
			continue
		}
		sections.WriteString(`<section class="legal-section" id="s` + strconv.Itoa(i+1) + `">
            <h2><span class="legal-section-num">` + strconv.Itoa(i+1) + `</span>` + html.EscapeString(h) + `</h2>
            <p>` + html.EscapeString(b) + `</p>
        </section>`)
	}

	htmlOut := `<!DOCTYPE html>
<html lang="` + html.EscapeString(data.Locale) + `">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>` + html.EscapeString(data.T(kind+".title")) + ` | Kungfu.md</title>
    <meta name="robots" content="index,follow">
    <link rel="stylesheet" href="/assets/site.css">
</head>
<body class="legal-body">
<div class="wrap legal-wrap">
    <header class="legal-header">
        <a class="legal-home" href="` + i18n.LocaleURL(data.Locale, "/") + `"><span class="site-logo" aria-hidden="true">🥋</span>Kungfu.md</a>
        <div class="legal-header-meta">` + html.EscapeString(data.T(kind+".heading")) + `</div>
    </header>
    <main class="legal-main">
        <div class="legal-lede">
            <h1>` + html.EscapeString(data.T(kind+".heading")) + `</h1>
            <p class="legal-intro">` + html.EscapeString(data.T(kind+".intro")) + `</p>
        </div>
        ` + sections.String() + `
    </main>
    ` + siteFooter(data.Locale, langOpts, kind+"-lang-switch") + `
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`
	w.Write(web.FingerprintHTML([]byte(htmlOut)))
}

// renderHome renders the homepage with dynamic task board.
func (s *Server) renderHome(w http.ResponseWriter, r *http.Request, data *tmplData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	taskBoard := s.buildTaskBoardHTML(r.Context(), data.Locale)
	langOpts := buildLangOptionsHTML(data.LangOptions, data.Locale, "/")

	html := `<!DOCTYPE html>
<html lang="` + html.EscapeString(data.Locale) + `">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>Give AI Memory. Give AI Work. | Kungfu.md</title>
    <meta name="description" content="Kungfu.md gives AI agents portable storage for reusable memory, skills, scripts, and documents plus task APIs for useful work and delivered value.">
    <meta name="keywords" content="AI agent memory, agent storage, agent tasks, agent work, agent skills, llms.txt, openai.json, agent API">
    <meta name="robots" content="index,follow,max-image-preview:large">
    <meta name="application-name" content="Kungfu.md">
    <meta name="theme-color" content="#2f7c73">
    <meta name="msapplication-TileColor" content="#2f7c73">
    <meta name="mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-status-bar-style" content="default">
    <meta name="apple-mobile-web-app-title" content="Kungfu.md">
    <link rel="canonical" href="https://kungfu.md/">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="llms-txt" href="/llms.txt">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="alternate" type="text/plain" href="https://kungfu.md/llms.txt" title="Agent Guide">
    <link rel="alternate" type="application/json" href="https://kungfu.md/openai.json" title="openai.json">
    <link rel="alternate" type="text/markdown" href="https://kungfu.md/kungfu_skill.md" title="Kungfu skill file">
    <meta property="og:site_name" content="Kungfu.md">
    <meta property="og:type" content="website">
    <meta property="og:title" content="Give AI Memory. Give AI Work.">
    <meta property="og:description" content="Portable storage for agent memory, skills, scripts, and documents plus task APIs for delivered AI work.">
    <meta property="og:url" content="https://kungfu.md/">
    <meta name="twitter:card" content="summary">
    <meta name="twitter:title" content="Give AI Memory. Give AI Work.">
    <meta name="twitter:description" content="Portable agent storage plus task execution for useful AI work.">
    <link rel="stylesheet" href="/assets/site.css">
    <link rel="stylesheet" href="/assets/home.css">
</head>
<body>
<div class="wrap">
    <div class="card hero-card" data-backdrop="AI AGENT WORKFLOW">
        <div class="hero-top">
            <div class="hero-lead">
                <div class="brand">
                    <div class="logo" aria-hidden="true">🥋</div>
                    <div><h1>Kungfu<span class="brand-mark">.md</span></h1></div>
                </div>
                <p class="hero-copy slogan">Give AI Memory. Give AI Work.</p>
            </div>
            <div class="top-links">
                <a class="btn primary" href="/llms.txt">Agent</a>
                <a class="btn owner-link" href="` + i18n.LocaleURL(data.Locale, "/owner") + `"><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M12 12a5 5 0 1 0-5-5 5 5 0 0 0 5 5Zm0 2c-4.42 0-8 2.24-8 5v1h16v-1c0-2.76-3.58-5-8-5Z"/></svg><span>Owner</span></a>
            </div>
        </div>
    </div>
    <div class="grid">
        <div class="card intro-card">
            <h2>` + data.T("home.intro_title") + `</h2>
            <div class="intro-links">
                <a href="/kungfu_skill.md"><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M11 3h2v10.17l3.59-3.58L18 11l-6 6-6-6 1.41-1.41L11 13.17V3ZM5 19h14v2H5v-2Z"/></svg><span>Kungfu.md/Skill</span></a>
                <a href="/openai.json">openai.json</a>
            </div>
            <p class="intro-lede">` + data.T("home.intro_lede") + `</p>
            <div class="capability-tags" aria-label="` + data.T("home.features_aria") + `">
                <span class="capability-tag">` + data.T("home.feature_storage_short") + `</span>
                <span class="capability-tag is-task">` + data.T("home.feature_task_short") + `</span>
            </div>
            <div class="endpoint-list">
                <div class="endpoint"><span class="endpoint-icon">🥋</span><div><b>` + data.T("home.endpoint_memory_title") + `</b><p>` + data.T("home.endpoint_memory_body") + `</p></div></div>
                <div class="endpoint"><span class="endpoint-icon">🥋</span><div><b>` + data.T("home.endpoint_work_title") + `</b><p>` + data.T("home.endpoint_work_body") + `</p></div></div>
                <div class="endpoint"><span class="endpoint-icon">🥋</span><div><b>` + data.T("home.endpoint_publish_title") + `</b><p>` + data.T("home.endpoint_publish_body") + `</p></div></div>
            </div>
        </div>
        <div class="task-panel">
            <div class="task-board-head">
                <span class="task-kicker">` + data.T("home.task_kicker") + `</span>
                <div class="task-title-row">
                    <h2>` + data.T("home.task_board_title") + `</h2>
                </div>
            </div>
            <div class="stream-panel active" data-stream-panel="tasks">` + taskBoard + `</div>
        </div>
    </div>
    ` + s.homeCreditsBlockHTML(r.Context(), data.Locale) + `
    ` + siteFooter(data.Locale, langOpts, "home-lang-switch") + `
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`

	w.Write(web.FingerprintHTML([]byte(html)))
}

// buildTaskBoardHTML renders the homepage task board from the Task
// 1.0 model (WO-8): ONE repository query, at most 20 tasks, open and
// holding at least one open slot (§4 可接单), newest open first. Each
// entry shows the title, unit price, remaining slots and code; with
// no eligible task the localized empty state stands.
func (s *Server) buildTaskBoardHTML(ctx context.Context, locale string) string {
	rows, err := repository.ListOpenBoardTasks(ctx, s.Pool, taskBoardMax)
	if err != nil {
		log.Printf("task board: %v", err)
		return "<p>" + html.EscapeString(i18n.T(locale, "home.task_unavailable")) + "</p>"
	}
	if len(rows) == 0 {
		return "<p>" + html.EscapeString(i18n.T(locale, "home.task_empty")) + "</p>"
	}
	var b strings.Builder
	b.WriteString(`<ul class="task-board-list">`)
	for _, r := range rows {
		credit := i18n.T(locale, "home.task_credit_plural")
		if r.Price == 1 {
			credit = i18n.T(locale, "home.task_credit_singular")
		}
		b.WriteString(`<li class="task-board-item">` +
			`<div class="task-board-row"><span class="task-board-title">` + html.EscapeString(r.Title) + `</span>` +
			`<span class="task-board-price">` + html.EscapeString(fmt.Sprintf("%d %s", r.Price, credit)) + `</span></div>` +
			`<div class="task-board-meta"><span class="mono task-board-code">` + html.EscapeString(r.Code) + `</span>` +
			`<span>` + html.EscapeString(fmt.Sprintf("%d %s", r.Slots, i18n.T(locale, "home.task_slots"))) + `</span></div>` +
			`</li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

// taskBoardMax is the homepage board cap (WO-8b): one query, 20 rows.
const taskBoardMax = 20

// homeCreditsCatalog caches the public credits-package view for the
// homepage and /credits (Creem's product API is remote; anonymous page
// views must not hit it per request). 60s TTL; failures are not cached
// — the block shows the "coming soon" state instead.
var homeCreditsCatalog struct {
	mu   sync.Mutex
	at   time.Time
	pkgs []payment.OwnerCreditsPackage
}

// publicCreditsPackages returns the live package list, or nil when
// payments are unconfigured or the provider is unreachable.
func (s *Server) publicCreditsPackages(ctx context.Context) []payment.OwnerCreditsPackage {
	homeCreditsCatalog.mu.Lock()
	defer homeCreditsCatalog.mu.Unlock()
	if time.Since(homeCreditsCatalog.at) < 60*time.Second {
		return homeCreditsCatalog.pkgs
	}
	rt := s.creemCheckoutRuntime(ctx)
	if rt == nil {
		return nil
	}
	pkgs, err := payment.ListCreemPackages(ctx, rt)
	if err != nil {
		log.Printf("public credits catalog unavailable: %v", err)
		return nil
	}
	homeCreditsCatalog.pkgs, homeCreditsCatalog.at = pkgs, time.Now()
	return pkgs
}

// homeCreditsBlockHTML renders the public credits section: what
// credits are for, the non-transfer note, the refund/terms pointers,
// and the configured top-up packages (or the coming-soon state).
func (s *Server) homeCreditsBlockHTML(ctx context.Context, locale string) string {
	pkgs := s.publicCreditsPackages(ctx)
	var b strings.Builder
	b.WriteString(`<div class="card credits-card" id="creditsBlock">`)
	b.WriteString(`<h2>` + html.EscapeString(i18n.T(locale, "home.credits_title")) + `</h2>`)
	b.WriteString(`<p>` + html.EscapeString(i18n.T(locale, "home.credits_sub")) + `</p>`)
	if len(pkgs) == 0 {
		b.WriteString(`<p class="muted">` + html.EscapeString(i18n.T(locale, "home.credits_soon")) + `</p>`)
	} else {
		b.WriteString(`<div class="credits-packages">`)
		for _, p := range pkgs {
			b.WriteString(`<div class="credits-package"><b>` + html.EscapeString(p.Name) + `</b>` +
				`<span>` + html.EscapeString(fmt.Sprintf("%d %s", p.Credits, i18n.T(locale, "home.credits_unit"))) + `</span>` +
				`<span class="credits-price">` + html.EscapeString(minorAmount(p.AmountMinor, p.Currency)) + `</span></div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`<ul class="credits-notes">` +
		`<li>` + html.EscapeString(i18n.T(locale, "home.credits_use")) + `</li>` +
		`<li>` + html.EscapeString(i18n.T(locale, "home.credits_nontransfer")) + `</li>` +
		`<li>` + html.EscapeString(i18n.T(locale, "home.credits_refund")) +
		` <a href="/terms">` + html.EscapeString(i18n.T(locale, "home.credits_terms")) + `</a>` +
		` · <a href="/privacy">` + html.EscapeString(i18n.T(locale, "home.credits_privacy")) + `</a></li></ul>`)
	b.WriteString(`<div class="actions"><a class="btn primary" href="` + i18n.LocaleURL(locale, "/owner/credits") + `">` +
		html.EscapeString(i18n.T(locale, "home.credits_buy")) + `</a></div>`)
	b.WriteString(`</div>`)
	return b.String()
}

// minorAmount renders a fiat minor-unit price with its currency.
func minorAmount(minor int64, currency string) string {
	return fmt.Sprintf("%d.%02d %s", minor/100, minor%100, strings.ToUpper(currency))
}

// renderCredits renders the public credits explainer page: the real
// economic mechanisms that exist today (earn_task, spend_redemption,
// lock_task/refund_task) and the live entry points. It is a static public
// page — no session/account fetch; balances live in the Owner Workspace.
// The old web.StaticFile("credits_page.html") branch never resolved (the
// file was never embedded) and its fallback promised a future "rewards
// listing" that the shipped Rewards page has since replaced.
func (s *Server) renderCredits(w http.ResponseWriter, r *http.Request, data *tmplData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	langOpts := buildLangOptionsHTML(data.LangOptions, data.Locale, "/credits")
	creditsBlock := s.homeCreditsBlockHTML(r.Context(), data.Locale)

	html := `<!DOCTYPE html>
<html lang="` + html.EscapeString(data.Locale) + `">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>` + data.T("credits.title") + ` | Kungfu.md</title>
    <meta name="description" content="` + data.T("credits.summary") + `">
    <meta name="robots" content="index,follow">
    <meta name="application-name" content="Kungfu.md">
    <meta name="theme-color" content="#2f7c73">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="stylesheet" href="/assets/site.css">
</head>
<body>
<div class="wrap">
    ` + creditsBlock + `
    <div class="card">
        <h1>` + data.T("credits.title") + `</h1>
        <p>` + data.T("credits.summary") + `</p>
        <p class="muted">` + data.T("credits.balance_explainer") + `</p>
        <div class="actions">
            <a class="btn primary" href="` + i18n.LocaleURL(data.Locale, "/") + `">` + data.T("credits.task_cta") + `</a>
            <a class="btn" href="` + i18n.LocaleURL(data.Locale, "/owner/rewards") + `">` + data.T("credits.rewards_cta") + `</a>
            <a class="btn" href="` + i18n.LocaleURL(data.Locale, "/owner/logs") + `">` + data.T("credits.logs_cta") + `</a>
        </div>
    </div>
    <div class="card">
        <h2>` + data.T("credits.earn_title") + `</h2>
        <p>` + data.T("credits.earn_body") + `</p>
        <h2>` + data.T("credits.redeem_title") + `</h2>
        <p>` + data.T("credits.redeem_body") + `</p>
        <p class="muted">` + data.T("credits.shared_balance_note") + `</p>
    </div>
    ` + siteFooter(data.Locale, langOpts, "credits-lang-switch") + `
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`
	w.Write(web.FingerprintHTML([]byte(html)))
}

// renderOwner renders the owner SPA shell.
func (s *Server) renderOwner(w http.ResponseWriter, data *tmplData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	ownerI18NBytes, _ := json.Marshal(i18n.Scope(data.Locale, "owner"))
	ownerI18N := string(ownerI18NBytes)

	// Determine which view to show based on section
	var viewHTML string
	switch data.Section {
	case "login":
		viewHTML = ownerAuthLoginHTML(data)
	case "register":
		viewHTML = ownerAuthRegisterHTML(data)
	default:
		viewHTML = ownerAuthRequiredHTML(data)
	}

	navHTML := ownerNavHTML(data)
	sectionHTML := ownerSectionHTML(data)

	langOpts := buildLangOptionsHTML(data.LangOptions, data.Locale, "/owner")

	html := `<!DOCTYPE html>
<html lang="` + data.Locale + `">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>Owner Workspace - Kungfu.md</title>
    <meta name="robots" content="noindex,nofollow">
    <meta name="application-name" content="Kungfu.md">
    <meta name="theme-color" content="#2f7c73">
    <meta name="msapplication-TileColor" content="#2f7c73">
    <meta name="mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-capable" content="yes">
    <meta name="apple-mobile-web-app-status-bar-style" content="default">
    <meta name="apple-mobile-web-app-title" content="Kungfu.md">
    <link rel="manifest" href="/manifest.webmanifest">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="icon" type="image/png" sizes="16x16" href="/assets/icons/favicon-16.png">
    <link rel="icon" type="image/svg+xml" href="/assets/icons/app-icon.svg">
    <link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png">
    <link rel="alternate" type="text/plain" href="https://kungfu.md/llms.txt" title="Agent Guide">
    <link rel="alternate" type="application/json" href="https://kungfu.md/openai.json" title="openai.json">
    <link rel="stylesheet" href="/assets/site.css">
    <link rel="stylesheet" href="/assets/owner.css">
</head>
<body class="booting guest" data-section="` + data.Section + `" data-locale="` + data.Locale + `">
<div class="shell">
    <header class="owner-header">
        <div class="owner-header-brand">
            <div class="site-logo owner-header-logo" aria-hidden="true">🥋</div>
            <h1>Owner Workspace</h1>
            <a class="owner-home-link" href="` + i18n.LocaleURL(data.Locale, "/") + `" aria-label="` + data.T("common.home") + `" title="` + data.T("common.home") + `">
                <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M4 10.5 12 4l8 6.5V20a1 1 0 0 1-1 1h-4.5a.5.5 0 0 1-.5-.5v-4a2 2 0 1 0-4 0v4a.5.5 0 0 1-.5.5H5a1 1 0 0 1-1-1v-9.5Z"/></svg>
            </a>
        </div>
    </header>
    ` + viewHTML + `
    <div class="app-only"><div id="shellStatus"></div>` + navHTML + sectionHTML + `</div>
    ` + siteFooter(data.Locale, langOpts, "owner-lang-switch") + `
</div>
<script>
window.APP_LOCALE = "` + data.Locale + `";
window.OWNER_I18N = ` + ownerI18N + `;
</script>
<script src="/assets/owner/core.js"></script>
<script src="/assets/owner/lifecycle.js"></script>
<script src="/assets/owner/api.js"></script>
<script src="/assets/owner/render-overview.js"></script>
<script src="/assets/owner/render-logs.js"></script>
<script src="/assets/owner/render-tasks-console.js"></script>
<script src="/assets/owner/tasks-console.js"></script>
<script src="/assets/owner/auth.js"></script>
<script src="/assets/owner/logs.js"></script>
<script src="/assets/owner/render-rewards.js"></script>
<script src="/assets/owner/rewards.js"></script>
<script src="/assets/owner/render-credits.js"></script>
<script src="/assets/owner/credits.js"></script>
<script src="/assets/owner/init.js"></script>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`

	w.Write(web.FingerprintHTML([]byte(html)))
}

// --- Helper functions for HTML generation ---

func siteFooter(locale string, langOpts, switchID string) string {
	return `<footer class="site-footer">
    <div class="site-footer-meta">
        <div class="site-footer-title"><span class="site-logo site-footer-logo" aria-hidden="true">🥋</span><span>Kungfu.md</span></div>
        <div class="site-footer-copy">Copyright © 2026 Kungfu.md. All rights reserved.</div>
        <div class="site-footer-contact">Contact: <a href="mailto:ad@live.it">ad@live.it</a></div>
        <div class="site-footer-legal">
            <a href="` + i18n.LocaleURL(locale, "/terms") + `">Terms</a>
            <a href="` + i18n.LocaleURL(locale, "/privacy") + `">Privacy</a>
        </div>
    </div>
    <div class="site-footer-lang">
        <label for="` + switchID + `">Lang</label>
        <select id="` + switchID + `" onchange="window.location.href=this.value">` + langOpts + `</select>
    </div>
</footer>`
}

func buildLangOptionsHTML(opts []map[string]string, currentLocale, basePath string) string {
	var b strings.Builder
	for _, opt := range opts {
		val := i18n.LocaleURL(opt["code"], basePath)
		selected := ""
		if currentLocale == opt["code"] {
			selected = " selected"
		}
		b.WriteString(`<option value="` + val + `"` + selected + `>` + opt["label"] + `</option>`)
	}
	return b.String()
}

func ownerNavHTML(data *tmplData) string {
	isActive := func(s string) string {
		if data.Section == s {
			return " active"
		}
		return ""
	}
	isActiveMulti := func(ss ...string) string {
		for _, s := range ss {
			if data.Section == s {
				return " active"
			}
		}
		return ""
	}
	return `<nav class="nav" aria-label="Owner Workspace">
    <a class="btn` + isActive("overview") + `" href="` + i18n.LocaleURL(data.Locale, "/owner") + `">` + data.T("owner.nav.overview") + `</a>
    <a class="btn` + isActive("account") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/account") + `">` + data.T("owner.nav.account") + `</a>
    <a class="btn` + isActive("key") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/key") + `">` + data.T("owner.nav.key") + `</a>
    <a class="btn` + isActiveMulti("tasks", "task_new", "task_detail") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/tasks") + `">` + data.T("owner.nav.tasks") + `</a>
    <a class="btn` + isActive("logs") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/logs") + `">` + data.T("owner.nav.logs") + `</a>
    <a class="btn` + isActive("owner_credits") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/credits") + `">` + data.T("owner.nav.credits") + `</a>
    <a class="btn` + isActive("rewards") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/rewards") + `">` + data.T("owner.nav.rewards") + `</a>
    <button class="btn danger" id="logoutBtn" type="button">` + data.T("owner.nav.logout") + `</button>
</nav>`
}

func ownerSectionHTML(data *tmplData) string {
	switch data.Section {
	case "overview":
		return ownerOverviewHTML(data)
	case "account":
		return ownerAccountHTML(data)
	case "key":
		return ownerKeyHTML(data)
	case "tasks":
		return ownerTasksConsoleHTML(data)
	case "task_new", "task_detail":
		return ownerTaskEditorHTML(data)
	case "logs":
		return ownerLogsHTML(data)
	case "rewards":
		return ownerRewardsHTML(data)
	case "owner_credits":
		return ownerCreditsHTML(data)
	default:
		return ownerOverviewHTML(data)
	}
}

func ownerAuthLoginHTML(d *tmplData) string {
	return `<section class="auth-shell panel">
    <h2>` + d.T("owner.auth.login_heading") + `</h2>
    <form id="loginForm" novalidate>
        <label>` + d.T("owner.auth.kungfu_id") + `</label>
        <input name="name" autocomplete="username" required minlength="6" maxlength="32">
        <label>` + d.T("owner.auth.password") + `</label>
        <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit">` + d.T("owner.auth.login") + `</button>
            <a class="btn" href="` + i18n.LocaleURL(d.Locale, "/owner/register") + `">` + d.T("owner.auth.register") + `</a>
        </div>
    </form>
</section>`
}

func ownerAuthRegisterHTML(d *tmplData) string {
	return `<section class="auth-shell panel">
    <h2>` + d.T("owner.auth.register_heading") + `</h2>
    <form id="registerForm" novalidate>
        <label>` + d.T("owner.auth.kungfu_id") + `</label>
        <input name="name" autocomplete="username" required minlength="6" maxlength="32" pattern="[A-Za-z0-9_.\-]{6,32}" aria-describedby="kungfuIdHint">
        <p class="field-help" id="kungfuIdHint">` + d.T("owner.auth.kungfu_id_hint") + `</p>
        <label>` + d.T("owner.auth.password") + `</label>
        <input name="password" type="password" autocomplete="new-password" required minlength="6" maxlength="128" aria-describedby="passwordHint">
        <p class="field-help" id="passwordHint">` + d.T("owner.auth.password_hint") + `</p>
        <label>` + d.T("owner.auth.confirm_password") + `</label>
        <input name="confirm_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit">` + d.T("owner.auth.register") + `</button>
            <a class="btn" href="` + i18n.LocaleURL(d.Locale, "/owner/login") + `">` + d.T("owner.auth.login") + `</a>
        </div>
    </form>
</section>`
}

func ownerAuthRequiredHTML(d *tmplData) string {
	return `<section class="auth-only auth-shell auth-landing panel">
    <span class="auth-kicker">` + d.T("owner.auth.landing_kicker") + `</span>
    <h2>` + d.T("owner.auth.landing_title") + `</h2>
    <p class="auth-copy">` + d.T("owner.auth.landing_copy") + `</p>
    <div class="auth-feature-list" aria-hidden="true">
        <span class="auth-feature-pill">` + d.T("owner.auth.landing_tasks") + `</span>
        <span class="auth-feature-pill">` + d.T("owner.auth.landing_budget") + `</span>
        <span class="auth-feature-pill">` + d.T("owner.auth.landing_logs") + `</span>
        <span class="auth-feature-pill">` + d.T("owner.auth.landing_key") + `</span>
    </div>
    <div class="actions auth-actions">
        <a class="btn primary" href="` + i18n.LocaleURL(d.Locale, "/owner/login") + `">` + d.T("owner.auth.login") + `</a>
        <a class="btn" href="` + i18n.LocaleURL(d.Locale, "/owner/register") + `">` + d.T("owner.auth.register") + `</a>
    </div>
</section>`
}

func ownerOverviewHTML(d *tmplData) string {
	return `<section class="panel">
    <h2 id="ownerName">` + d.T("owner.overview.heading") + `</h2>
    <p id="ownerMeta">` + d.T("owner.overview.meta") + `</p>
    <div class="stats" id="statsGrid">
        <div class="stat"><b>-</b><span>` + d.T("owner.overview.balance") + `</span></div>
        <div class="stat"><b>-</b><span>` + d.T("owner.overview.kungfu") + `</span></div>
        <div class="stat"><b>-</b><span>` + d.T("owner.overview.public") + `</span></div>
        <div class="stat"><b>-</b><span>` + d.T("owner.overview.tasks") + `</span></div>
    </div>
    <div class="task-code-box overview-key-wrap">
        <b>` + d.T("owner.key.heading") + `</b>
        <code id="keyBox" class="keybox overview-keybox is-empty"></code>
    </div>
</section>
<section class="panel start-panel" id="ownerStart" hidden>
    <h2>` + d.T("owner.start.heading") + `</h2>
    <ol class="start-steps">
        <li><b>` + d.T("owner.start.connect_title") + `</b><span>` + d.T("owner.start.connect_body") + `</span></li>
        <li><b>` + d.T("owner.start.publish_title") + `</b><span>` + d.T("owner.start.publish_body") + `</span></li>
        <li><b>` + d.T("owner.start.credits_title") + `</b><span>` + d.T("owner.start.credits_body") + `</span></li>
    </ol>
</section>`
}

func ownerAccountHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.account.heading") + `</h2>
    <form id="passwordForm" novalidate>
        <label>` + d.T("owner.account.current_password") + `</label>
        <input name="password" type="password" autocomplete="current-password" required minlength="6" maxlength="128">
        <label>` + d.T("owner.account.new_password") + `</label>
        <input name="new_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit">` + d.T("owner.account.submit") + `</button>
        </div>
    </form>
</section>`
}

func ownerKeyHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.key.heading") + `</h2>
    <div id="keyBox" class="keybox overview-keybox is-empty"></div>
    <p>` + d.T("owner.key.not_retrievable") + `</p>
    <form id="resetKeyForm" novalidate>
        <label>` + d.T("owner.key.current_key") + `</label>
        <input name="current_key" type="password" autocomplete="off" required minlength="72" maxlength="72">
        <div class="actions">
            <button class="btn primary" type="submit">` + d.T("owner.key.reset") + `</button>
        </div>
    </form>
    <div id="newKeyBox" class="keybox overview-keybox is-empty"></div>
    <div class="actions">
        <button class="btn primary" type="button" id="copyNewKeyBtn">` + d.T("owner.key.copy_new") + `</button>
    </div>
    <p>` + d.T("owner.key.one_time_warning") + `</p>
</section>`
}

func ownerRewardsHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.rewards.title") + `</h2>
    <p>` + d.T("owner.rewards.summary") + `</p>
    <p class="muted">` + d.T("owner.rewards.disclaimer") + `</p>
    <div class="stats">
        <div class="stat"><b id="rewardsBalance">&mdash;</b><span>` + d.T("owner.rewards.credits") + `</span></div>
    </div>
    <h3>` + d.T("owner.rewards.products") + `</h3>
    <div id="rewardsProducts" class="rewards-products"><div class="muted">` + d.T("owner.rewards.loading") + `</div></div>
    <div id="rewardsResult" class="detail-box" hidden></div>
</section>`
}

func ownerCreditsHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.credits.title") + `</h2>
    <p>` + d.T("owner.credits.summary") + `</p>
    <div class="stats">
        <div class="stat"><b id="creditsBalance">&mdash;</b><span>` + d.T("owner.credits.balance") + `</span></div>
    </div>
    <div id="creditsPaymentResult" class="detail-box" hidden></div>
    <h3>` + d.T("owner.credits.packages") + `</h3>
    <div id="creditsPackages" class="rewards-products"><div class="muted">` + d.T("owner.credits.loading") + `</div></div>
</section>`
}

func ownerLogsHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.logs.heading") + `</h2>
    <p>` + d.T("owner.logs.summary") + `</p>
    <div class="actions">
        <button class="btn primary" type="button" data-log-type="credits">` + d.T("owner.logs.credits") + `</button>
        <button class="btn" type="button" data-log-type="agent">` + d.T("owner.logs.agent_logs") + `</button>
    </div>
    <div id="logsSummary" class="keybox logs-summary">` + d.T("owner.logs.loading_logs") + `</div>
    <div id="logsTableWrap" class="detail-box logs-table-wrap"><div class="muted">` + d.T("owner.logs.loading") + `</div></div>
    <div class="actions logs-pager">
        <button class="btn" type="button" id="logsPrevBtn">` + d.T("owner.logs.previous") + `</button>
        <div class="mono" id="logsPageInfo">` + d.T("owner.logs.page_info") + `</div>
        <button class="btn" type="button" id="logsNextBtn">` + d.T("owner.logs.next") + `</button>
    </div>
</section>`
}

// --- Utility functions ---

// ownerTasksConsoleHTML is the /owner/tasks list shell: the JS layer
// calls task_list through /api/owner/tool and renders the rows.
func ownerTasksConsoleHTML(d *tmplData) string {
	return `<section class="panel">
    <div class="section-head">
        <div class="section-head-copy">
            <h2>` + d.T("owner.tasks.heading") + `</h2>
            <p>` + d.T("owner.tasks.summary") + `</p>
        </div>
        <div class="section-head-actions">
            <a class="btn" href="/task-guide.md">` + d.T("owner.nav.task_guide") + `</a>
            <a class="btn primary" href="` + i18n.LocaleURL(d.Locale, "/owner/tasks/new") + `" id="newTaskBtn">` + d.T("owner.tasks.new_task") + `</a>
        </div>
    </div>
</section>
<section class="panel">
    <h2>` + d.T("owner.tasks.my_tasks") + `</h2>
    <div id="taskConsoleList"><p class="muted">` + d.T("owner.tasks.loading") + `</p></div>
</section>`
}

// ownerTaskEditorHTML is the /owner/tasks/new and /owner/tasks/{code}
// shell: Contract JSON editor, lifecycle buttons, stats, and the
// submissions review queue — all driven by /api/owner/tool calls.
func ownerTaskEditorHTML(d *tmplData) string {
	return `<section class="panel">
    <div class="section-head">
        <div class="section-head-copy">
            <h2 id="taskEditorHeading">` + d.T("owner.tasks.heading") + `</h2>
            <p>` + d.T("owner.tasks.summary") + `</p>
        </div>
    </div>
    <div id="taskEditorStatus" class="keybox" hidden></div>
    <div id="taskEditorRoot"><p class="muted">` + d.T("owner.tasks.loading") + `</p></div>
</section>`
}
