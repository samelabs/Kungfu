/* Admin Workspace core: state + tiny DOM helpers. Independent from
 * the Owner workspace; reads only /api/admin/*. Secrets (csrf token,
 * session material, passwords) live ONLY in memory here — never in
 * localStorage/sessionStorage. */
'use strict';

const SECTION = document.body.dataset.section || '';

const state = {
    principal: null,     // {id, username, display_name, roles, permissions, expires_at, csrf_token}
    users: [],
    roles: [],
    permissions: [],
    sessions: [],
    audit: {
        items: [],
        page: 1,
        pageSize: 50,
        total: 0,
        filters: {action: '', actor_username: '', target_type: '', success: ''}
    }
};

function qs(selector) { return document.querySelector(selector); }
function qsa(selector) { return Array.from(document.querySelectorAll(selector)); }

function escapeHtml(v) {
    return String(v == null ? '' : v)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

function fmtTime(v) {
    if (!v) return '—';
    const d = new Date(v);
    if (isNaN(d.getTime())) return escapeHtml(v);
    return d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC');
}

function setNotice(id, message, kind) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = String(message == null ? '' : message);
    el.className = 'notice ' + (kind === 'ok' ? 'ok' : kind === 'error' ? 'error' : '');
    el.hidden = !message;
}

function hasPermission(code) {
    const p = state.principal && state.principal.permissions;
    if (!Array.isArray(p)) return false;
    return p.includes('*') || p.includes(code);
}

/* Client-side visibility ONLY — the server independently enforces
 * permissions on every API call. */
function applyPermissionVisibility() {
    const needed = {
        'users': ['admin.users.read'],
        'roles': ['admin.roles.read'],
        'sessions': ['admin.sessions.manage'],
        'audit': ['admin.audit.read'],
        'store_products': ['store.products.read'],
        'store_redemptions': ['store.redemptions.read']
    };
    document.querySelectorAll('#adminNav [data-admin-nav]').forEach(link => {
        const key = link.dataset.adminNav;
        const req = needed[key];
        if (req && !req.some(hasPermission)) link.hidden = true;
    });
    document.querySelectorAll('[data-admin-view]').forEach(el => {
        const req = el.dataset.adminView.split(',');
        if (!req.some(k => hasPermission(k.trim()))) el.hidden = true;
    });
    const nav = document.getElementById('adminNav');
    if (nav && state.principal) nav.hidden = false;
}
