/* Admin workspace init: per-section bootstrap + self password change. */
'use strict';

function renderDashboard() {
    const card = document.getElementById('adminDashboardCard');
    if (!card || !state.principal) return;
    const perms = (state.principal.permissions || []).map(p =>
        `<span class="tag">${escapeHtml(p)}</span>`).join(' ');
    const roles = (state.principal.roles || []).map(r =>
        `<span class="tag">${escapeHtml(r)}</span>`).join(' ');
    card.innerHTML = `
        <dl class="admin-kv">
            <dt>Admin</dt><dd>${escapeHtml(state.principal.username)} (#${state.principal.id})</dd>
            <dt>Display name</dt><dd>${escapeHtml(state.principal.display_name)}</dd>
            <dt>Roles</dt><dd>${roles || '—'}</dd>
            <dt>Permissions</dt><dd>${perms || '—'}</dd>
            <dt>Session expires</dt><dd>${fmtTime(state.principal.expires_at)}</dd>
        </dl>`;
}

function renderAccount() {
    const card = document.getElementById('adminAccountCard');
    if (!card || !state.principal) return;
    card.innerHTML = `
        <dl class="admin-kv">
            <dt>Username</dt><dd>${escapeHtml(state.principal.username)}</dd>
            <dt>Display name</dt><dd>${escapeHtml(state.principal.display_name)}</dd>
            <dt>Roles</dt><dd>${(state.principal.roles || []).map(r => `<span class="tag">${escapeHtml(r)}</span>`).join(' ') || '—'}</dd>
        </dl>`;
}

function bindAdminAccountEvents() {
    const form = document.getElementById('adminPasswordForm');
    if (form) {
        form.addEventListener('submit', async (e) => {
            e.preventDefault();
            setNotice('adminPasswordNotice', '', '');
            const current = document.getElementById('adminCurrentPassword').value;
            const next = document.getElementById('adminNewPassword').value;
            try {
                const json = await adminMutate('/api/admin/me/password', 'POST', {
                    current_password: current,
                    new_password: next
                });
                if (!json.success) throw new Error(apiError(json));
                setNotice('adminPasswordNotice', json.message || 'Password changed — redirecting…', 'ok');
                setTimeout(() => window.location.assign('/admin/login'), 1200);
            } catch (err) {
                setNotice('adminPasswordNotice', err.message, 'error');
            }
        });
    }
}

async function renderPage() {
    if (SECTION === 'login') return; // login form is static HTML
    if (!requirePrincipalElseRedirect()) return;
    applyPermissionVisibility();

    try {
        if (SECTION === 'dashboard') renderDashboard();
        if (SECTION === 'account') { renderAccount(); }
        if (SECTION === 'users') {
            if (hasPermission('admin.users.read')) {
                await loadUsers(); renderUsers();
            }
        }
        if (SECTION === 'roles') {
            if (hasPermission('admin.roles.read')) {
                await Promise.all([loadRoles(), loadPermissions()]);
                renderRoles(); renderPermissions();
            }
        }
        if (SECTION === 'sessions') {
            if (hasPermission('admin.sessions.manage')) {
                await loadSessions(); renderSessions();
            }
        }
        if (SECTION === 'audit') {
            if (hasPermission('admin.audit.read')) {
                await loadAudit(); renderAudit();
            }
        }
        if (SECTION === 'store_products') {
            if (hasPermission('store.products.read')) {
                await loadStoreProducts(); renderStoreProducts();
            }
        }
        if (SECTION === 'store_redemptions') {
            if (hasPermission('store.redemptions.read')) {
                await loadStoreRedemptions(); renderStoreRedemptions();
            }
        }
    } catch (err) {
        window.alert(err.message);
    }
}

(async function init() {
    bindAdminAuthEvents();
    bindAdminAccountEvents();
    bindAdminUsersEvents();
    bindAdminRolesEvents();
    bindAdminSessionsEvents();
    bindAdminAuditEvents();
    bindStoreProductsEvents();
    bindStoreRedemptionsEvents();

    if (SECTION !== 'login') {
        try {
            await restorePrincipal();
        } catch (e) {
            // adminFetch already redirects to /admin/login on 401
        }
    }
    document.body.classList.remove('booting');
    await renderPage();
})();
