package server

import (
	"net/http"

	"kungfu.md/internal/i18n"
)

// ============================================================
// B1.2: Admin Workspace — a fully independent SPA shell.
// Shares NOTHING with the Owner workspace: no kf_owner, no owner
// session state, no /api/owner/* calls; only the admin plane.
// ============================================================

// handleAdminPage renders the Admin Workspace shell for a section.
func (s *Server) handleAdminPage(section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locale := i18n.ResolveLocale(r)
		s.renderAdmin(w, &tmplData{
			Locale:      locale,
			LangOptions: i18n.LanguageOptions(locale),
			Section:     section,
			T:           func(string) string { return "" }, // admin UI is unilingual (en)
		})
	}
}

// renderAdmin renders the admin SPA shell.
func (s *Server) renderAdmin(w http.ResponseWriter, data *tmplData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	nav := adminNavHTML(data)
	section := adminSectionHTML(data)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
    <title>Admin Workspace - Kungfu.md</title>
    <meta name="robots" content="noindex,nofollow">
    <link rel="icon" type="image/png" sizes="32x32" href="/assets/icons/favicon-32.png">
    <link rel="stylesheet" href="/assets/site.css?v=5">
    <link rel="stylesheet" href="/assets/admin.css?v=1">
</head>
<body class="booting" data-section="` + data.Section + `">
<div class="shell admin-shell">
    <header class="admin-header">
        <div class="admin-header-brand">
            <div class="site-logo" aria-hidden="true">🛡️</div>
            <h1>Admin Workspace</h1>
            <a class="admin-home-link" href="/" aria-label="Home">↗</a>
        </div>
    </header>
    ` + nav + `
    <main class="admin-main">` + section + `</main>
</div>
<script src="/assets/admin/core.js?v=1"></script>
<script src="/assets/admin/api.js?v=1"></script>
<script src="/assets/admin/auth.js?v=1"></script>
<script src="/assets/admin/users.js?v=1"></script>
<script src="/assets/admin/roles.js?v=1"></script>
<script src="/assets/admin/sessions.js?v=1"></script>
<script src="/assets/admin/audit.js?v=1"></script>
<script src="/assets/admin/store.js?v=1"></script>
<script src="/assets/admin/init.js?v=1"></script>
</body>
</html>`

	w.Write([]byte(html))
}

func adminNavHTML(data *tmplData) string {
	_ = data // nav visibility is driven client-side from permissions
	return `<nav class="nav admin-nav" aria-label="Admin Workspace" id="adminNav" hidden>
    <a class="btn" data-admin-nav="dashboard" href="/admin">Dashboard</a>
    <a class="btn" data-admin-nav="users" href="/admin/users">Admins</a>
    <a class="btn" data-admin-nav="roles" href="/admin/roles">Roles</a>
    <a class="btn" data-admin-nav="sessions" href="/admin/sessions">Sessions</a>
    <a class="btn" data-admin-nav="audit" href="/admin/audit">Audit</a>
    <a class="btn" data-admin-nav="store_products" href="/admin/store/products">Store Products</a>
    <a class="btn" data-admin-nav="store_redemptions" href="/admin/store/redemptions">Store Redemptions</a>
    <a class="btn" data-admin-nav="account" href="/admin/account">Account</a>
    <button class="btn danger" id="adminLogoutBtn" type="button">Logout</button>
</nav>`
}

func adminSectionHTML(data *tmplData) string {
	switch data.Section {
	case "login":
		return adminLoginHTML()
	case "account":
		return adminAccountHTML()
	case "users":
		return adminUsersHTML()
	case "roles":
		return adminRolesHTML()
	case "sessions":
		return adminSessionsHTML()
	case "audit":
		return adminAuditHTML()
	case "store_products":
		return adminStoreProductsHTML()
	case "store_redemptions":
		return adminStoreRedemptionsHTML()
	default: // dashboard
		return adminDashboardHTML()
	}
}

func adminLoginHTML() string {
	return `<section class="admin-section" id="adminLoginSection">
    <h2>Admin Login</h2>
    <form id="adminLoginForm" class="admin-form" autocomplete="off">
        <label for="adminLoginUsername">Username</label>
        <input id="adminLoginUsername" name="username" type="text" required minlength="3" maxlength="64" autocomplete="off">
        <label for="adminLoginPassword">Password</label>
        <input id="adminLoginPassword" name="password" type="password" required autocomplete="off">
        <button class="btn primary" type="submit">Log in</button>
        <p class="notice" id="adminLoginNotice" hidden></p>
    </form>
</section>`
}

func adminDashboardHTML() string {
	return `<section class="admin-section" id="adminDashboardSection">
    <h2>Dashboard</h2>
    <div id="adminDashboardCard" class="admin-card"></div>
</section>`
}

func adminAccountHTML() string {
	return `<section class="admin-section" id="adminAccountSection">
    <h2>My Account</h2>
    <div id="adminAccountCard" class="admin-card"></div>
    <h3>Change password</h3>
    <form id="adminPasswordForm" class="admin-form" autocomplete="off">
        <label for="adminCurrentPassword">Current password</label>
        <input id="adminCurrentPassword" type="password" required autocomplete="off">
        <label for="adminNewPassword">New password (min 8 chars)</label>
        <input id="adminNewPassword" type="password" required minlength="8" autocomplete="off">
        <button class="btn primary" type="submit">Change password</button>
        <p class="notice" id="adminPasswordNotice" hidden></p>
    </form>
</section>`
}

func adminUsersHTML() string {
	return `<section class="admin-section" id="adminUsersSection">
    <h2>Admins</h2>
    <div id="adminUsersCard" class="admin-card"></div>
    <div data-admin-view="users.manage">
        <h3>Create admin</h3>
        <form id="adminUserCreateForm" class="admin-form" autocomplete="off">
            <label for="adminNewUsername">Username (a-z0-9._-)</label>
            <input id="adminNewUsername" type="text" required minlength="3" maxlength="64" pattern="[a-zA-Z0-9._-]{3,64}" autocomplete="off">
            <label for="adminNewDisplayName">Display name</label>
            <input id="adminNewDisplayName" type="text" required maxlength="128" autocomplete="off">
            <label for="adminNewUserPassword">Password (min 8 chars)</label>
            <input id="adminNewUserPassword" type="password" required minlength="8" autocomplete="off">
            <button class="btn primary" type="submit">Create</button>
            <p class="notice" id="adminUserCreateNotice" hidden></p>
        </form>
    </div>
</section>`
}

func adminRolesHTML() string {
	return `<section class="admin-section" id="adminRolesSection">
    <h2>Roles</h2>
    <div id="adminRolesCard" class="admin-card"></div>
    <div data-admin-view="roles.manage">
        <h3>Create role</h3>
        <form id="adminRoleCreateForm" class="admin-form" autocomplete="off">
            <label for="adminNewRoleCode">Code (a-z0-9._-)</label>
            <input id="adminNewRoleCode" type="text" required minlength="3" maxlength="64" pattern="[a-zA-Z0-9._-]{3,64}" autocomplete="off">
            <label for="adminNewRoleName">Name</label>
            <input id="adminNewRoleName" type="text" required maxlength="128" autocomplete="off">
            <label for="adminNewRoleDesc">Description (optional)</label>
            <input id="adminNewRoleDesc" type="text" maxlength="500" autocomplete="off">
            <button class="btn primary" type="submit">Create</button>
            <p class="notice" id="adminRoleCreateNotice" hidden></p>
        </form>
    </div>
    <h3>Permissions</h3>
    <div id="adminPermissionsCard" class="admin-card"></div>
</section>`
}

func adminSessionsHTML() string {
	return `<section class="admin-section" id="adminSessionsSection">
    <h2>Sessions</h2>
    <div id="adminSessionsCard" class="admin-card"></div>
</section>`
}

func adminAuditHTML() string {
	return `<section class="admin-section" id="adminAuditSection">
    <h2>Audit Explorer</h2>
    <form id="adminAuditFilterForm" class="admin-filters" autocomplete="off">
        <input id="auditFAction" type="text" placeholder="action">
        <input id="auditFActor" type="text" placeholder="actor_username">
        <input id="auditFTargetType" type="text" placeholder="target_type">
        <select id="auditFSuccess">
            <option value="">success: any</option>
            <option value="true">true</option>
            <option value="false">false</option>
        </select>
        <button class="btn" type="submit">Filter</button>
    </form>
    <div id="adminAuditCard" class="admin-card"></div>
    <div class="admin-pager">
        <button class="btn" id="auditPrevPage" type="button">← Prev</button>
        <span id="auditPageInfo"></span>
        <button class="btn" id="auditNextPage" type="button">Next →</button>
    </div>
</section>`
}

func adminStoreProductsHTML() string {
	return `<section class="admin-section" id="adminStoreProductsSection">
    <h2>Store Products</h2>
    <form id="adminStoreProductFilters" class="admin-filters" autocomplete="off">
        <select id="storeProductStatus">
            <option value="all">status: all</option>
            <option value="active">active</option>
            <option value="inactive">inactive</option>
        </select>
        <input id="storeProductQ" type="text" placeholder="search code / title">
        <button class="btn" type="submit">Filter</button>
    </form>
    <div id="adminStoreProductsCard" class="admin-card"></div>
    <div class="admin-pager">
        <button class="btn" id="storeProductsPrev" type="button">← Prev</button>
        <span id="storeProductsPageInfo"></span>
        <button class="btn" id="storeProductsNext" type="button">Next →</button>
    </div>
    <div data-admin-view="store.products.manage">
        <h3>Create product</h3>
        <form id="adminStoreProductCreateForm" class="admin-form" autocomplete="off">
            <label for="storeNewTitle">Title</label>
            <input id="storeNewTitle" type="text" required maxlength="128" autocomplete="off">
            <label for="storeNewDesc">Description (optional)</label>
            <input id="storeNewDesc" type="text" maxlength="500" autocomplete="off">
            <label for="storeNewPrice">Credits price</label>
            <input id="storeNewPrice" type="number" step="any" min="0.0001" required autocomplete="off">
            <button class="btn primary" type="submit">Create</button>
            <p class="notice" id="storeProductCreateNotice" hidden></p>
        </form>
    </div>
</section>`
}

func adminStoreRedemptionsHTML() string {
	return `<section class="admin-section" id="adminStoreRedemptionsSection">
    <h2>Store Redemptions</h2>
    <form id="adminStoreRedemptionFilters" class="admin-filters" autocomplete="off">
        <select id="storeRedemptionStatus">
            <option value="all">status: all</option>
            <option value="pending_review">pending_review</option>
            <option value="approved">approved</option>
            <option value="rejected">rejected</option>
            <option value="fulfilled">fulfilled</option>
            <option value="cancelled">cancelled</option>
        </select>
        <input id="storeRedemptionBotID" type="number" min="1" placeholder="bot_id">
        <input id="storeRedemptionQ" type="text" placeholder="search code / title / request_key">
        <button class="btn" type="submit">Filter</button>
    </form>
    <div id="adminStoreRedemptionsCard" class="admin-card"></div>
    <div class="admin-pager">
        <button class="btn" id="storeRedemptionsPrev" type="button">← Prev</button>
        <span id="storeRedemptionsPageInfo"></span>
        <button class="btn" id="storeRedemptionsNext" type="button">Next →</button>
    </div>
</section>`
}
