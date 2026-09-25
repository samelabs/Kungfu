function renderLogs() {
    const wrap = qs('#logsTableWrap');
    if (!wrap) return;

    const summary = qs('#logsSummary');
    const pageInfo = qs('#logsPageInfo');
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');

    if (summary) {
        if (state.logs.type === 'credits') {
            summary.textContent = t('logs.balance_summary', {balance: state.logs.balance, total: state.logs.total});
        } else {
            summary.textContent = t('logs.agent_summary', {total: state.logs.total});
        }
    }

    if (pageInfo) {
        pageInfo.textContent = t('logs.page_info', {
            page: state.logs.page,
            totalPages: Math.max(state.logs.totalPages, 1),
            total: state.logs.total
        });
    }
    if (prevBtn) prevBtn.disabled = state.logs.page <= 1;
    if (nextBtn) nextBtn.disabled = state.logs.page >= state.logs.totalPages;

    qsa('[data-log-type]').forEach((button) => {
        const isActive = button.dataset.logType === state.logs.type;
        button.classList.toggle('primary', isActive);
    });

    // Loading/empty/error states are owned by the shared section
    // lifecycle (sectionBox). The renderer only paints ready tables;
    // an initial load error never renders as "no data".
    if (!state.logs.items.length) {
        return;
    }

    if (state.logs.type === 'credits') {
        wrap.innerHTML = `
            <table class="logs-table">
                <thead>
                    <tr>
                        <th>${escapeHtml(t('logs.th_id'))}</th><th>${escapeHtml(t('logs.th_type'))}</th><th>${escapeHtml(t('logs.th_amount'))}</th><th>${escapeHtml(t('logs.th_balance'))}</th><th>${escapeHtml(t('logs.th_ref'))}</th><th>${escapeHtml(t('logs.th_time'))}</th>
                    </tr>
                </thead>
                <tbody>
                    ${state.logs.items.map((row) => `
                        <tr>
                            <td>${row.id}</td>
                            <td>${escapeHtml(ledgerTypeLabel(row.type))}</td>
                            <td>${escapeHtml(String(row.amount))}</td>
                            <td>${escapeHtml(String(row.balance_after))}</td>
                            <td>${escapeHtml([row.ref_type, row.ref_id].filter(Boolean).join(':') || '-')}</td>
                            <td>${escapeHtml(row.created_at)}</td>
                        </tr>
                    `).join('')}
                </tbody>
            </table>
        `;
        return;
    }

    if (state.logs.type === 'agent') {
        wrap.innerHTML = `
            <table class="logs-table">
                <thead>
                    <tr>
                        <th>${escapeHtml(t('logs.th_id'))}</th><th>${escapeHtml(t('logs.th_action'))}</th><th>${escapeHtml(t('logs.th_target'))}</th><th>${escapeHtml(t('logs.th_source'))}</th><th>${escapeHtml(t('logs.th_result'))}</th><th>${escapeHtml(t('logs.th_data'))}</th><th>${escapeHtml(t('logs.th_time'))}</th>
                    </tr>
                </thead>
                <tbody>
                    ${state.logs.items.map((row) => `
                        <tr>
                            <td>${row.id}</td>
                            <td>${escapeHtml(humanLogAction(row.action))}</td>
                            <td>${escapeHtml([row.target_type, row.target_id].filter(Boolean).join(':') || '-')}</td>
                            <td>${escapeHtml([row.ip_address, row.user_agent].filter(Boolean).join(' | ') || '-')}</td>
                            <td>${row.success ? escapeHtml(t('logs.ok')) : escapeHtml(t('logs.error', {code: row.error_code || 'UNKNOWN'}))}</td>
                            <td>${escapeHtml(row.request_data ? JSON.stringify(row.request_data) : '-')}</td>
                            <td>${escapeHtml(row.created_at)}</td>
                        </tr>
                    `).join('')}
                </tbody>
            </table>
        `;
        return;
    }

}

// ledgerTypeLabel turns an internal ledger type into owner-facing text;
// unknown types fall back to the raw value.
function ledgerTypeLabel(type) {
    const key = `logs.tx_${String(type || '')}`;
    const label = t(key);
    return label === key ? String(type || '') : label;
}
