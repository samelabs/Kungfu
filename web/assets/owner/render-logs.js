function renderLogs() {
    const wrap = qs('#logsTableWrap');
    if (!wrap) return;

    const summary = qs('#logsSummary');
    const pageInfo = qs('#logsPageInfo');
    const prevBtn = qs('#logsPrevBtn');
    const nextBtn = qs('#logsNextBtn');
    const taskFilter = qs('#logTaskFilter');

    if (summary) {
        if (state.logs.type === 'credits') {
            summary.textContent = t('logs.balance_summary', {balance: state.logs.balance.toFixed(4), total: state.logs.total});
        } else if (state.logs.type === 'agent') {
            summary.textContent = t('logs.agent_summary', {total: state.logs.total});
        } else {
            const filter = state.logs.taskCode ? t('logs.task_filter_suffix', {taskCode: state.logs.taskCode}) : '';
            summary.textContent = t('logs.task_summary', {total: state.logs.total, filter});
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

    if (taskFilter) {
        if (state.logs.type === 'task') {
            taskFilter.hidden = false;
            const options = [`<option value="">${escapeHtml(t('logs.all_tasks'))}</option>`].concat(
                state.logs.tasks.map((task) => `<option value="${escapeHtml(task.code)}">${escapeHtml(task.code)} · ${escapeHtml(task.title)}</option>`)
            );
            taskFilter.innerHTML = options.join('');
            taskFilter.value = state.logs.taskCode;
        } else {
            taskFilter.hidden = true;
        }
    }

    if (!state.logs.items.length) {
        wrap.innerHTML = `<div class="logs-empty">${escapeHtml(t('logs.empty'))}</div>`;
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
                            <td>${escapeHtml(row.type)}</td>
                            <td>${Number(row.amount).toFixed(4)}</td>
                            <td>${Number(row.balance_after).toFixed(4)}</td>
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

    wrap.innerHTML = `
        <table class="logs-table">
            <thead>
                <tr>
                    <th>${escapeHtml(t('logs.th_id'))}</th><th>${escapeHtml(t('logs.th_task'))}</th><th>${escapeHtml(t('logs.th_action'))}</th><th>${escapeHtml(t('logs.th_result'))}</th><th>${escapeHtml(t('logs.th_detail'))}</th><th>${escapeHtml(t('logs.th_time'))}</th>
                </tr>
            </thead>
            <tbody>
                ${state.logs.items.map((row) => `
                    <tr>
                        <td>${row.id}</td>
                        <td>${escapeHtml(row.task_code)}</td>
                        <td>${escapeHtml(humanLogAction(row.action || '-'))}</td>
                        <td>${row.success ? escapeHtml(t('logs.ok')) : escapeHtml(t('logs.error', {code: row.error_code || 'UNKNOWN'}))}</td>
                        <td>${escapeHtml(row.error_message || row.response_body || (row.payload_json ? JSON.stringify(row.payload_json) : '-'))}</td>
                        <td>${escapeHtml(row.created_at)}</td>
                    </tr>
                `).join('')}
            </tbody>
        </table>
    `;
}
