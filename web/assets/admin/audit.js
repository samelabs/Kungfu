/* Admin audit explorer: filters, pagination, rendering. */
'use strict';

async function loadAudit() {
    const f = state.audit.filters;
    const params = new URLSearchParams({
        page: String(state.audit.page),
        page_size: String(state.audit.pageSize)
    });
    if (f.action) params.set('action', f.action);
    if (f.actor_username) params.set('actor_username', f.actor_username);
    if (f.target_type) params.set('target_type', f.target_type);
    if (f.success) params.set('success', f.success);
    const json = await adminGet('/api/admin/audit?' + params.toString());
    if (!json.success) throw new Error(apiError(json, 'Failed to load audit'));
    state.audit.items = json.data.items || [];
    state.audit.total = json.data.total || 0;
}

function renderAudit() {
    const card = document.getElementById('adminAuditCard');
    if (!card) return;
    if (!state.audit.items.length) {
        card.innerHTML = '<p class="muted">No audit entries.</p>';
    } else {
        const rows = state.audit.items.map(l => {
            const detail = [];
            if (l.before_json != null) detail.push('<details><summary>before</summary><pre>' +
                escapeHtml(JSON.stringify(l.before_json, null, 2)) + '</pre></details>');
            if (l.after_json != null) detail.push('<details><summary>after</summary><pre>' +
                escapeHtml(JSON.stringify(l.after_json, null, 2)) + '</pre></details>');
            if (l.metadata_json != null) detail.push('<details><summary>metadata</summary><pre>' +
                escapeHtml(JSON.stringify(l.metadata_json, null, 2)) + '</pre></details>');
            return `<tr>
                <td>${l.id}</td>
                <td>${fmtTime(l.created_at)}</td>
                <td>${escapeHtml(l.actor_username)}</td>
                <td><code>${escapeHtml(l.action)}</code></td>
                <td>${escapeHtml(l.target_type || '')} ${escapeHtml(l.target_id || '')}</td>
                <td>${l.success ? '<span class="status ok">ok</span>' : '<span class="status error">fail</span>'}</td>
                <td class="detail">${detail.join('') || '—'}</td>
            </tr>`;
        }).join('');
        card.innerHTML = `<table class="admin-table wide">
            <thead><tr><th>ID</th><th>Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Result</th><th>Facts</th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
    }
    const info = document.getElementById('auditPageInfo');
    if (info) {
        const pages = Math.max(1, Math.ceil(state.audit.total / state.audit.pageSize));
        info.textContent = `page ${state.audit.page} / ${pages} (${state.audit.total} entries)`;
    }
}

function bindAdminAuditEvents() {
    const form = document.getElementById('adminAuditFilterForm');
    if (form) {
        form.addEventListener('submit', (e) => {
            e.preventDefault();
            state.audit.filters = {
                action: document.getElementById('auditFAction').value.trim(),
                actor_username: document.getElementById('auditFActor').value.trim(),
                target_type: document.getElementById('auditFTargetType').value.trim(),
                success: document.getElementById('auditFSuccess').value
            };
            state.audit.page = 1;
            loadAudit().then(renderAudit).catch(err => window.alert(err.message));
        });
    }
    const prev = document.getElementById('auditPrevPage');
    const next = document.getElementById('auditNextPage');
    if (prev) prev.addEventListener('click', () => {
        if (state.audit.page > 1) { state.audit.page--; loadAudit().then(renderAudit); }
    });
    if (next) next.addEventListener('click', () => {
        const pages = Math.max(1, Math.ceil(state.audit.total / state.audit.pageSize));
        if (state.audit.page < pages) { state.audit.page++; loadAudit().then(renderAudit); }
    });
}
