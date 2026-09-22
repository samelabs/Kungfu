/* 011 Platform Account Administration UI.
 * Server authorization is the authority; UI visibility is cosmetic.
 * Accounts page manages tb_bots platform accounts — NOT admin
 * accounts (/admin/users) and NOT finance. Enable / Disable is
 * access suspension only; the server owns every economic fact. */
'use strict';

const accountsState = {
    items: [], page: 1, pageSize: 20, total: 0,
    filters: {status: 'all', q: ''},
    detail: null
};

// Balance arrives as a canonical decimal STRING from the server.
// Preserved verbatim for display — NEVER routed through JS Number
// (int64 corruption boundary).
function fmtBalance(v) {
    return escapeHtml(String(v));
}

function fmtTime(v) {
    if (v === null || v === undefined) return '—';
    return escapeHtml(String(v));
}

async function loadAccounts() {
    const st = accountsState;
    const params = new URLSearchParams({
        status: st.filters.status,
        page: String(st.page),
        page_size: String(st.pageSize)
    });
    if (st.filters.q) params.set('q', st.filters.q);
    const json = await adminGet('/api/admin/accounts?' + params.toString());
    if (!json.success) throw new Error(apiError(json, 'Failed to load accounts'));
    st.items = json.data.accounts || [];
    st.total = json.data.total || 0;
}

async function loadAccountDetail(id) {
    const json = await adminGet('/api/admin/accounts/' + encodeURIComponent(id));
    if (!json.success) throw new Error(apiError(json, 'Failed to load account'));
    accountsState.detail = json.data.account;
}

function renderAccounts() {
    const card = document.getElementById('adminAccountsCard');
    if (!card) return;
    const canManage = hasPermission('accounts.manage');
    const info = document.getElementById('accountsPageInfo');
    if (info) info.textContent = 'page ' + accountsState.page + ' / ' +
        Math.max(1, Math.ceil(accountsState.total / accountsState.pageSize)) +
        ' (' + accountsState.total + ' accounts)';
    if (!accountsState.items.length) {
        card.innerHTML = '<p class="muted">No accounts match.</p>';
        return;
    }
    const rows = accountsState.items.map(a => {
        const actions = [];
        actions.push('<button class="btn small" data-aact="detail" data-id="' + a.id + '">Detail</button>');
        if (canManage) {
            actions.push(a.status === 'active'
                ? '<button class="btn small danger" data-aact="disable" data-id="' + a.id + '">Disable</button>'
                : '<button class="btn small" data-aact="enable" data-id="' + a.id + '">Enable</button>');
        }
        return '<tr>' +
            '<td>' + a.id + '</td>' +
            '<td>' + escapeHtml(a.bot_name) + '</td>' +
            '<td>' + escapeHtml(a.status) + '</td>' +
            '<td>' + fmtBalance(a.balance) + '</td>' +
            '<td>****' + escapeHtml(a.api_key_last4) + '</td>' +
            '<td>' + fmtTime(a.last_active_at) + '</td>' +
            '<td>' + fmtTime(a.created_at) + '</td>' +
            '<td>' + actions.join(' ') + '</td>' +
            '</tr>';
    }).join('');
    card.innerHTML = '<table class="admin-table"><thead><tr>' +
        '<th>ID</th><th>Bot name</th><th>Status</th><th>Balance (Credits)</th>' +
        '<th>Agent key</th><th>Last active</th><th>Created</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
}

function renderAccountDetail() {
    const card = document.getElementById('adminAccountDetailCard');
    if (!card || !accountsState.detail) return;
    const d = accountsState.detail;
    // Two DISTINCT business facts — displayed separately, never merged.
    card.hidden = false;
    card.innerHTML = '<h3>Account #' + d.id + ' — ' + escapeHtml(d.bot_name) + '</h3>' +
        '<dl class="admin-facts">' +
        '<dt>Status</dt><dd>' + escapeHtml(d.status) + '</dd>' +
        '<dt>Balance (Credits)</dt><dd>' + fmtBalance(d.balance) + '</dd>' +
        '<dt>Agent key</dt><dd>****' + escapeHtml(d.api_key_last4) + ' (issued ' + fmtTime(d.key_issued_at) + ')</dd>' +
        '<dt>Created</dt><dd>' + fmtTime(d.created_at) + '</dd>' +
        '<dt>Updated</dt><dd>' + fmtTime(d.updated_at) + '</dd>' +
        '<dt>Last active</dt><dd>' + fmtTime(d.last_active_at) + '</dd>' +
        '<dt>Published tasks</dt><dd>' + d.published_task_count + '</dd>' +
        '<dt>Submissions</dt><dd>' + d.submission_count + '</dd>' +
        '<dt>Kungfu / memory</dt><dd>' + d.kungfu_count + '</dd>' +
        '</dl>' +
        // Pure cross-link into the Finance domain — shown only when
        // this admin also holds finance.read (visibility only; the
        // finance API enforces its own authority). No finance data
        // is embedded in the Accounts API.
        (hasPermission('finance.read')
            ? '<a class="btn small" href="/admin/finance">View finance for this account</a>'
            : '');
}

async function accountAction(action, id) {
    try {
        const json = await adminMutate('/api/admin/accounts/' + encodeURIComponent(id) + '/' + action, 'POST');
        if (!json.success) throw new Error(apiError(json));
        await Promise.all([loadAccounts(), loadAccountDetail(id)]);
        renderAccounts();
        renderAccountDetail();
    } catch (err) {
        window.alert(err.message);
    }
}

function bindAdminAccountsEvents() {
    const form = document.getElementById('adminAccountFilters');
    if (form) form.addEventListener('submit', ev => {
        ev.preventDefault();
        accountsState.filters.status = document.getElementById('accountStatusFilter').value;
        accountsState.filters.q = document.getElementById('accountQFilter').value.trim();
        accountsState.page = 1;
        loadAccounts().then(renderAccounts).catch(err => window.alert(err.message));
    });
    bindPager('accountsPrev', 'accountsNext', accountsState,
        () => loadAccounts().then(renderAccounts));
    const card = document.getElementById('adminAccountsCard');
    if (card) card.addEventListener('click', ev => {
        const btn = ev.target.closest('button[data-aact]');
        if (!btn) return;
        const act = btn.dataset.aact;
        const id = btn.dataset.id;
        if (act === 'detail') {
            loadAccountDetail(id).then(renderAccountDetail).catch(err => window.alert(err.message));
        } else if (act === 'disable') {
            // Access suspension only — still ask for confirmation to
            // avoid accidental lockouts of live accounts.
            if (window.confirm('Disable platform account #' + id + '? Authentication for this Agent/Owner will fail immediately. (No credentials, data or balances are changed.)')) {
                accountAction('disable', id);
            }
        } else if (act === 'enable') {
            accountAction('enable', id);
        }
    });
}
