/* Admin sessions: list, revoke one, force logout one admin. */
'use strict';

async function loadSessions() {
    const json = await adminGet('/api/admin/sessions');
    if (!json.success) throw new Error(apiError(json, 'Failed to load sessions'));
    state.sessions = json.data.sessions || [];
}

function renderSessions() {
    const card = document.getElementById('adminSessionsCard');
    if (!card) return;
    if (!state.sessions.length) {
        card.innerHTML = '<p class="muted">No sessions.</p>';
        return;
    }
    const me = state.principal;
    const rows = state.sessions.map(s => {
        const active = !s.revoked_at;
        const self = me && s.admin_id === me.id && active;
        return `<tr>
            <td>${s.id}</td>
            <td>${escapeHtml(s.username)}${self ? ' <span class="tag">you</span>' : ''}</td>
            <td>${escapeHtml(s.ip_address || '—')}</td>
            <td class="ua">${escapeHtml(s.user_agent || '—')}</td>
            <td>${fmtTime(s.created_at)}</td>
            <td>${fmtTime(s.last_seen_at)}</td>
            <td>${fmtTime(s.expires_at)}</td>
            <td>${active
                ? `<button class="btn small danger" data-act="revoke" data-id="${s.id}">Revoke</button>`
                : '<span class="muted">revoked</span>'}</td>
            <td>${active ? `<button class="btn small" data-act="force" data-admin="${s.admin_id}" data-name="${escapeHtml(s.username)}">Force logout all</button>` : ''}</td>
        </tr>`;
    }).join('');
    card.innerHTML = `<table class="admin-table wide">
        <thead><tr><th>ID</th><th>Admin</th><th>IP</th><th>User agent</th><th>Created</th><th>Last seen</th><th>Expires</th><th></th><th></th></tr></thead>
        <tbody>${rows}</tbody>
    </table>`;
}

async function bindAdminSessionsEvents() {
    const card = document.getElementById('adminSessionsCard');
    if (!card) return;
    card.addEventListener('click', async (e) => {
        const btn = e.target.closest('button[data-act]');
        if (!btn) return;
        try {
            if (btn.dataset.act === 'revoke') {
                const id = parseInt(btn.dataset.id, 10);
                const json = await adminMutate(`/api/admin/sessions/${id}`, 'DELETE');
                if (!json.success) throw new Error(apiError(json));
                // revoking our own current session logs us out
                const revokedSelf = json.data && state.principal &&
                    json.data.admin_id === state.principal.id;
                await loadSessions(); renderSessions();
                if (revokedSelf) window.location.assign('/admin/login');
            } else if (btn.dataset.act === 'force') {
                const adminId = parseInt(btn.dataset.admin, 10);
                if (!window.confirm(`Force logout ALL sessions of ${btn.dataset.name}?`)) return;
                const json = await adminMutate(`/api/admin/users/${adminId}/force-logout`, 'POST');
                if (!json.success) throw new Error(apiError(json));
                await loadSessions(); renderSessions();
            }
        } catch (err) {
            window.alert(err.message);
        }
    });
}
