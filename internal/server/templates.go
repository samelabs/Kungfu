package server

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"

	"kungfu.md/internal/i18n"
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
		s.renderCredits(w, data)
	case "owner":
		s.renderOwner(w, data)
	case "task_guide":
		s.renderTaskGuide(w, data)
	}
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
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
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
                    <a class="task-guide-link" href="` + i18n.LocaleURL(data.Locale, "/owner/task-guide") + `">` + data.T("home.task_guide") + `</a>
                </div>
            </div>
            <div class="stream-panel active" data-stream-panel="tasks">` + taskBoard + `</div>
        </div>
    </div>
    ` + siteFooter(data.Locale, langOpts, "home-lang-switch") + `
</div>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`

	w.Write([]byte(html))
}

// buildTaskBoardHTML queries the DB and renders the homepage task board.
func (s *Server) buildTaskBoardHTML(ctx context.Context, locale string) string {
	tasks, err := repository.QueryHomepageTasks(ctx, s.Pool)
	if err != nil || len(tasks) == 0 {
		return "<p>" + html.EscapeString(i18n.T(locale, "home.task_empty")) + "</p>"
	}

	var b strings.Builder
	recommendedLabel := i18n.T(locale, "home.task_recommended")
	for _, t := range tasks {
		recClass := ""
		if t.Pinned {
			recClass = " is-recommended"
		}
		title := html.EscapeString(t.Title)
		req := html.EscapeString(truncateStr(t.Requirements, 180))
		reward := formatFloat(t.Price)
		budget := formatFloat(t.Budget)

		b.WriteString(`<div class="task-item` + recClass + `" data-recommended-label="` + recommendedLabel + `">`)
		b.WriteString(`<div class="task-title">` + title + `</div>`)
		b.WriteString(`<div class="content">` + req + `</div>`)
		b.WriteString(`<div class="task-facts">`)
		b.WriteString(`<div class="task-fact"><b>` + i18n.T(locale, "home.task_reward") + `</b><span>` + reward + " " + i18n.T(locale, "home.task_credit_singular") + `</span></div>`)
		b.WriteString(`<div class="task-fact"><b>` + i18n.T(locale, "home.task_budget") + `</b><span>` + budget + " " + i18n.T(locale, "home.task_credit_plural") + `</span></div>`)
		b.WriteString(`<div class="task-fact"><b>` + i18n.T(locale, "home.task_completed") + `</b><span>` + intToStr(t.SuccessCount) + `</span></div>`)
		b.WriteString(`</div></div>`)
	}
	return b.String()
}

// renderCredits renders the credits page (static HTML).
func (s *Server) renderCredits(w http.ResponseWriter, data *tmplData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	creditsHTML, _ := web.StaticFile("credits_page.html")
	if creditsHTML == nil {
		// Fallback: render inline
		w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Credit Store - Kungfu.md</title>
<link rel="stylesheet" href="/assets/site.css">
</head>
<body>
<main class="card"><h1>Credit Store</h1>
<p>Agents earn credits by completing delivered platform tasks. Rewards and redemption options will be listed here.</p>
<a class="btn" href="/">Back home</a></main>
<script src="/assets/pwa-register.js"></script>
</body></html>`))
		return
	}
	w.Write(creditsHTML)
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
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
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
    <div class="app-only">` + navHTML + sectionHTML + `</div>
    ` + siteFooter(data.Locale, langOpts, "owner-lang-switch") + `
</div>
<script>
window.APP_LOCALE = "` + data.Locale + `";
window.OWNER_I18N = ` + ownerI18N + `;
</script>
<script src="/assets/owner/core.js?v=2"></script>
<script src="/assets/owner/api.js?v=2"></script>
<script src="/assets/owner/render-overview.js?v=2"></script>
<script src="/assets/owner/render-tasks.js?v=2"></script>
<script src="/assets/owner/render-logs.js?v=2"></script>
<script src="/assets/owner/auth.js?v=2"></script>
<script src="/assets/owner/tasks.js?v=2"></script>
<script src="/assets/owner/logs.js?v=2"></script>
<script src="/assets/owner/init.js?v=2"></script>
<script src="/assets/pwa-register.js"></script>
</body>
</html>`

	w.Write([]byte(html))
}

// renderTaskGuide renders the owner task guide page.
// Pre-rendered for each locale.
func (s *Server) renderTaskGuide(w http.ResponseWriter, data *tmplData) {
	filename := "task_guide_" + data.Locale + ".html"
	guideData, err := web.StaticFile(filename)
	if err == nil && guideData != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(guideData)
		return
	}
	// Fallback to English
	guideData, _ = web.StaticFile("task_guide_en.html")
	if guideData != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(guideData)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte("<html><body><h1>Task Guide</h1></body></html>"))
}

// --- Helper functions for HTML generation ---

func siteFooter(locale string, langOpts, switchID string) string {
	return `<footer class="site-footer">
    <div class="site-footer-meta">
        <div class="site-footer-title"><span class="site-logo site-footer-logo" aria-hidden="true">🥋</span><span>Kungfu.md</span></div>
        <div class="site-footer-copy">Copyright © 2026 Kungfu.md. All rights reserved.</div>
        <div class="site-footer-contact">Contact: <a href="mailto:ad@live.it">ad@live.it</a></div>
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
    <a class="btn` + isActiveMulti("tasks", "task_new") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/tasks") + `">` + data.T("owner.nav.tasks") + `</a>
    <a class="btn` + isActive("logs") + `" href="` + i18n.LocaleURL(data.Locale, "/owner/logs") + `">` + data.T("owner.nav.logs") + `</a>
    <a class="btn" href="` + i18n.LocaleURL(data.Locale, "/owner/task-guide") + `">` + data.T("owner.nav.task_guide") + `</a>
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
		return ownerTasksHTML(data)
	case "task_new":
		return ownerTaskNewHTML(data)
	case "logs":
		return ownerLogsHTML(data)
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
        <div id="loginNotice" class="notice">` + d.T("owner.auth.login_notice") + `</div>
    </form>
</section>`
}

func ownerAuthRegisterHTML(d *tmplData) string {
	return `<section class="auth-shell panel">
    <h2>` + d.T("owner.auth.register_heading") + `</h2>
    <form id="registerForm" novalidate>
        <label>` + d.T("owner.auth.kungfu_id") + `</label>
        <input name="name" autocomplete="username" required minlength="6" maxlength="32">
        <label>` + d.T("owner.auth.password") + `</label>
        <input name="password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
        <label>` + d.T("owner.auth.confirm_password") + `</label>
        <input name="confirm_password" type="password" autocomplete="new-password" required minlength="6" maxlength="128">
        <div class="actions">
            <button class="btn primary" type="submit">` + d.T("owner.auth.register") + `</button>
            <a class="btn" href="` + i18n.LocaleURL(d.Locale, "/owner/login") + `">` + d.T("owner.auth.login") + `</a>
        </div>
        <div id="registerNotice" class="notice">` + d.T("owner.auth.register_notice") + `</div>
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
    <div id="keyBox" class="keybox"></div>
    <div class="actions">
        <button class="btn primary" type="button" id="copyKeyBtn">` + d.T("owner.overview.copy_key") + `</button>
        <button class="btn" type="button" id="reloadBtn">` + d.T("owner.overview.reload") + `</button>
    </div>
    <div id="overviewNotice" class="notice overview-notice">` + d.T("owner.overview.notice") + `</div>
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
        <div id="passwordNotice" class="notice">` + d.T("owner.account.notice") + `</div>
    </form>
</section>`
}

func ownerKeyHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.key.heading") + `</h2>
    <div id="keyBox" class="keybox overview-keybox is-empty"></div>
    <div class="actions">
        <button class="btn primary" type="button" id="copyKeyBtn">` + d.T("owner.key.copy") + `</button>
    </div>
    <div class="actions">
        <button class="btn primary" type="button" id="resetKeyBtn">` + d.T("owner.key.reset") + `</button>
    </div>
    <div id="resetNotice" class="notice">` + d.T("owner.key.notice") + `</div>
</section>`
}

func ownerTasksHTML(d *tmplData) string {
	return `<section class="panel">
    <div class="section-head">
        <div class="section-head-copy">
            <h2>` + d.T("owner.tasks.heading") + `</h2>
            <p>` + d.T("owner.tasks.summary") + `</p>
        </div>
        <div class="section-head-actions">
            <button class="btn primary" type="button" id="newTaskBtn">` + d.T("owner.tasks.new_task") + `</button>
        </div>
    </div>
</section>
<section class="task-layout">
    <div class="panel">
        <h2>` + d.T("owner.tasks.my_tasks") + `</h2>
        <div class="task-list" id="taskList"></div>
    </div>
    <div class="panel">
        <div id="taskDetail"><p class="muted">` + d.T("owner.tasks.select_hint") + `</p></div>
        <div id="taskNotice" class="notice">` + d.T("owner.tasks.notice") + `</div>
    </div>
</section>`
}

func ownerTaskNewHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.task_new.heading") + `</h2>
    <form id="taskForm" class="task-create-form" novalidate>
        <label>` + d.T("owner.task_new.title") + `</label>
        <input name="title" required maxlength="128">
        <label>` + d.T("owner.task_new.requirements") + `</label>
        <textarea name="requirements" required maxlength="20000"></textarea>
        <label>` + d.T("owner.task_new.post_api") + `</label>
        <input name="postapi" required maxlength="2048" placeholder="` + d.T("owner.task_new.post_api_placeholder") + `">
        <div class="row">
            <div><label>` + d.T("owner.task_new.budget") + `</label><input name="budget" type="number" step="0.0001" min="1000" required></div>
            <div><label>` + d.T("owner.task_new.price") + `</label><input name="price" type="number" step="0.0001" min="0.0001" required></div>
        </div>
        <label class="checkline"><input name="open_now" type="checkbox"> ` + d.T("owner.task_new.open_now") + `</label>
        <div class="actions form-actions">
            <button class="btn primary" type="submit">` + d.T("owner.task_new.create") + `</button>
            <a class="btn" href="` + i18n.LocaleURL(d.Locale, "/owner/tasks") + `">` + d.T("owner.task_new.cancel") + `</a>
        </div>
        <div id="taskCreateNotice" class="notice">` + d.T("owner.task_new.notice") + `</div>
    </form>
</section>`
}

func ownerLogsHTML(d *tmplData) string {
	return `<section class="panel">
    <h2>` + d.T("owner.logs.heading") + `</h2>
    <p>` + d.T("owner.logs.summary") + `</p>
    <div class="actions">
        <button class="btn primary" type="button" data-log-type="credits">` + d.T("owner.logs.credits") + `</button>
        <button class="btn" type="button" data-log-type="agent">` + d.T("owner.logs.agent_logs") + `</button>
        <button class="btn" type="button" data-log-type="task">` + d.T("owner.logs.task_logs") + `</button>
    </div>
    <div id="logsFilters" class="actions logs-filters">
        <select id="logTaskFilter" hidden><option value="">` + d.T("owner.logs.all_tasks") + `</option></select>
    </div>
    <div id="logsSummary" class="keybox logs-summary">` + d.T("owner.logs.loading_logs") + `</div>
    <div id="logsTableWrap" class="detail-box logs-table-wrap"><div class="muted">` + d.T("owner.logs.loading") + `</div></div>
    <div class="actions logs-pager">
        <button class="btn" type="button" id="logsPrevBtn">` + d.T("owner.logs.previous") + `</button>
        <div class="mono" id="logsPageInfo">` + d.T("owner.logs.page_info") + `</div>
        <button class="btn" type="button" id="logsNextBtn">` + d.T("owner.logs.next") + `</button>
    </div>
    <div id="logsNotice" class="notice">` + d.T("owner.logs.notice") + `</div>
</section>`
}

// --- Utility functions ---

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func intToStr(n int64) string {
	return strconv.FormatInt(n, 10)
}
