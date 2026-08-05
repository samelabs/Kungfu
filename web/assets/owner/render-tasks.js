// ════════════════════════════════════════════════════════════
// render-tasks.js — Task list + detail rendering (refactored)
// Design: clean separation between list, detail, edit, budget
// ════════════════════════════════════════════════════════════

function renderTasks() {
    const list = qs('#taskList');
    if (!list) return;
    if (!state.tasks.length) {
        list.innerHTML = `<p class="muted">${escapeHtml(t('tasks.empty'))}</p>`;
        return;
    }
    list.innerHTML = state.tasks.map((task) => `
        <button class="task-item ${state.selectedTask?.code === task.code ? 'active' : ''}" type="button" data-code="${escapeHtml(task.code)}">
            <div class="task-title">
                <span>${escapeHtml(task.title)}</span>
                <span class="badge ${escapeHtml(task.status)}">${escapeHtml(humanTaskStatus(task.status))}</span>
            </div>
            <div class="task-meta">
                <span>${Number(task.price).toFixed(2)} ${escapeHtml(t('tasks.price'))}</span>
                <span>${Number(task.budget).toFixed(2)} ${escapeHtml(t('tasks.budget'))}</span>
                <span>${task.success_count || 0} ${escapeHtml(t('tasks.delivered'))}</span>
            </div>
        </button>
    `).join('');
    qsa('[data-code]').forEach((button) => button.addEventListener('click', () => selectTask(button.dataset.code)));
}

async function selectTask(code) {
    setNotice('taskNotice', t('tasks.loading'));
    try {
        const json = await requestJson(`/api/owner/tasks/${code}`, {method: 'GET'});
        if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
        state.selectedTask = json.data.task;
        state.selectedTaskLogs = json.data.logs || [];
        renderTaskDetail(json.data.task, json.data.logs || []);
        renderTasks();
        setNotice('taskNotice', '', '');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}

// ── Detail render: clean sections, no inline edit form ──────
function renderTaskDetail(task, logs) {
    const isClosed = task.status === 'closed';
    const canRefund = isClosed && Number(task.budget) > 0 && refundReady(task.closed_at);

    const refundHint = !isClosed
        ? t('tasks.refund_close_first')
        : Number(task.budget) <= 0
            ? t('tasks.refund_zero')
            : canRefund ? '' : t('tasks.refund_wait');

    qs('#budgetForm').hidden = isClosed; // budget only for open tasks
    qs('#taskDetail').innerHTML = `
        <div class="task-detail-head">
            <h2>${escapeHtml(task.title)}</h2>
            <span class="badge ${escapeHtml(task.status)}">${escapeHtml(humanTaskStatus(task.status))}</span>
        </div>
        <div class="task-code-box">
            <div>
                <b>${escapeHtml(t('tasks.task_code'))}</b>
                <code>${escapeHtml(task.code)}</code>
            </div>
            <button class="btn" type="button" data-copy="${escapeHtml(task.code)}">${escapeHtml(t('tasks.copy'))}</button>
        </div>
        <div class="task-detail-stats">
            <div class="task-stat"><b>${Number(task.price).toFixed(4)}</b><span>${escapeHtml(t('tasks.price'))}</span></div>
            <div class="task-stat"><b>${Number(task.budget).toFixed(4)}</b><span>${escapeHtml(t('tasks.budget'))}</span></div>
            <div class="task-stat"><b>${task.success_count || 0}</b><span>${escapeHtml(t('tasks.delivered'))}</span></div>
        </div>
        <div class="detail-box"><h3>${escapeHtml(t('tasks.requirements'))}</h3><p>${escapeHtml(task.requirements)}</p></div>
        <div class="detail-box"><h3>${escapeHtml(t('tasks.post_api'))}</h3><p class="mono">${escapeHtml(task.postapi || '')}</p></div>

        <div class="task-detail-actions">
            <button class="btn primary" type="button" data-task-action="open" ${task.status === 'open' ? 'disabled' : ''}>${escapeHtml(t('tasks.open'))}</button>
            <button class="btn danger" type="button" data-task-action="close" ${isClosed ? 'disabled' : ''}>${escapeHtml(t('tasks.close'))}</button>
            ${isClosed ? `<button class="btn" type="button" data-task-action="edit">${escapeHtml(t('tasks.edit_basics'))}</button>` : ''}
            <button class="btn" type="button" data-task-action="refund" ${canRefund ? '' : 'disabled'}>${escapeHtml(t('tasks.refund'))}</button>
        </div>
        ${refundHint ? `<p class="muted">${escapeHtml(refundHint)}</p>` : ''}
        <div id="taskEditContainer"></div>
    `;

    // Copy code
    const copyBtn = qs('[data-copy]');
    if (copyBtn) {
        copyBtn.addEventListener('click', async () => {
            try {
                await navigator.clipboard.writeText(copyBtn.dataset.copy || '');
                setNotice('taskNotice', t('tasks.task_code_copied'), 'ok');
            } catch (e) { setNotice('taskNotice', String(e), 'error'); }
        });
    }

    // Action buttons
    qsa('[data-task-action]').forEach((btn) => {
        btn.addEventListener('click', () => {
            const action = btn.dataset.taskAction;
            if (action === 'edit') return showEditForm(task);
            runTaskAction(task.code, action);
        });
    });
}

// ── Inline edit form (shown only when "Edit" clicked) ──────
function showEditForm(task) {
    const container = qs('#taskEditContainer');
    container.innerHTML = `
        <form id="taskEditForm" class="detail-box" novalidate>
            <h3>${escapeHtml(t('tasks.edit_basics'))}</h3>
            <label>${escapeHtml(t('tasks.title'))}</label>
            <input name="title" required maxlength="128" value="${escapeHtml(task.title)}">
            <label>${escapeHtml(t('tasks.requirements'))}</label>
            <textarea name="requirements" required maxlength="20000" rows="5">${escapeHtml(task.requirements)}</textarea>
            <label>${escapeHtml(t('tasks.post_api'))}</label>
            <input name="postapi" required maxlength="2048" value="${escapeHtml(task.postapi || '')}">
            <label>${escapeHtml(t('task_new.price'))}</label>
            <input name="price" type="number" step="0.0001" min="0.0001" required value="${Number(task.price).toFixed(4)}">
            <div class="actions">
                <button class="btn primary" type="submit">${escapeHtml(t('tasks.save_basics'))}</button>
                <button class="btn" type="button" data-edit-cancel>${escapeHtml(t('task_new.cancel'))}</button>
            </div>
        </form>
    `;
    container.scrollIntoView({behavior: 'smooth', block: 'nearest'});

    qs('#taskEditForm').addEventListener('submit', async (e) => {
        e.preventDefault();
        setNotice('taskNotice', t('tasks.saving'));
        const data = payload(e.currentTarget);
        try {
            const json = await requestJson(`/api/owner/tasks/${task.code}/edit`, {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
            // Use response data directly — no double fetch
            state.selectedTask = json.data.task;
            renderTaskDetail(json.data.task, state.selectedTaskLogs);
            updateTaskInList(json.data.task);
            renderTasks();
            setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
        } catch (err) { setNotice('taskNotice', String(err), 'error'); }
    });
    qs('[data-edit-cancel]').addEventListener('click', () => {
        container.innerHTML = '';
    });
}

// ── Task actions: use response data, no double fetch ───────
async function runTaskAction(code, action) {
    if (action === 'refund' && !window.confirm(t('tasks.refund_confirm'))) return;
    setNotice('taskNotice', t('tasks.action_progress', {action}));
    try {
        const json = await requestJson(`/api/owner/tasks/${code}/${action}`, {method: 'POST', body: '{}'});
        if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
        // Update from response — single source of truth
        state.selectedTask = json.data.task;
        renderTaskDetail(json.data.task, state.selectedTaskLogs);
        updateTaskInList(json.data.task);
        renderTasks();
        setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}

// ── Patch task summary in list without full reload ─────────
function updateTaskInList(task) {
    const idx = state.tasks.findIndex((t) => t.code === task.code);
    if (idx >= 0) {
        state.tasks[idx].title = task.title;
        state.tasks[idx].status = task.status;
        state.tasks[idx].budget = task.budget;
        state.tasks[idx].price = task.price;
        state.tasks[idx].closed_at = task.closed_at;
        state.tasks[idx].opened_at = task.opened_at;
    }
}

function refundReady(closedAt) {
    if (!closedAt) return false;
    const closed = new Date(closedAt.replace(' ', 'T'));
    if (Number.isNaN(closed.getTime())) return false;
    return (Date.now() - closed.getTime()) >= (7 * 24 * 60 * 60 * 1000);
}
