// ════════════════════════════════════════════════════════════
// render-tasks.js — Task list + detail (v2)
// Rule: detail = read-only display + action bar.
//       ALL forms (create/edit/budget) go through modal overlay.
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
        renderTaskDetail(json.data.task);
        renderTasks();
        setNotice('taskNotice', '', '');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}

// ── Detail: pure display + action bar, ZERO inline forms ───
function renderTaskDetail(task) {
    const isClosed = task.status === 'closed';
    const isOpen = task.status === 'open';
    const canRefund = isClosed && Number(task.budget) > 0 && refundReady(task.closed_at);

    qs('#taskDetail').innerHTML = `
        <div class="task-detail-head">
            <h2>${escapeHtml(task.title)}</h2>
            <span class="badge ${escapeHtml(task.status)}">${escapeHtml(humanTaskStatus(task.status))}</span>
        </div>
        <div class="task-action-bar">
            ${isOpen ? `<button class="btn danger" type="button" data-act="close">${escapeHtml(t('tasks.close'))}</button>` : ''}
            ${isClosed ? `<button class="btn primary" type="button" data-act="open">${escapeHtml(t('tasks.open'))}</button>` : ''}
            ${isClosed ? `<button class="btn" type="button" data-act="edit">${escapeHtml(t('tasks.edit_basics'))}</button>` : ''}
            ${isOpen ? `<button class="btn" type="button" data-act="budget">${escapeHtml(t('tasks.add_budget'))}</button>` : ''}
            ${canRefund ? `<button class="btn" type="button" data-act="refund">${escapeHtml(t('tasks.refund'))}</button>` : ''}
            <button class="btn" type="button" data-copy="${escapeHtml(task.code)}">${escapeHtml(t('tasks.copy'))}</button>
        </div>
        <div class="task-code-box">
            <div>
                <b>${escapeHtml(t('tasks.task_code'))}</b>
                <code>${escapeHtml(task.code)}</code>
            </div>
        </div>
        <div class="task-detail-stats">
            <div class="task-stat"><b>${Number(task.price).toFixed(4)}</b><span>${escapeHtml(t('tasks.price'))}</span></div>
            <div class="task-stat"><b>${Number(task.budget).toFixed(4)}</b><span>${escapeHtml(t('tasks.budget'))}</span></div>
            <div class="task-stat"><b>${task.success_count || 0}</b><span>${escapeHtml(t('tasks.delivered'))}</span></div>
        </div>
        <div class="detail-box"><h3>${escapeHtml(t('tasks.requirements'))}</h3><p>${escapeHtml(task.requirements)}</p></div>
        <div class="detail-box"><h3>${escapeHtml(t('tasks.post_api'))}</h3><p class="mono">${escapeHtml(task.postapi || '')}</p></div>
    `;

    // Bind action bar
    qsa('[data-act]').forEach((btn) => {
        btn.addEventListener('click', () => {
            const act = btn.dataset.act;
            if (act === 'edit') return openTaskModal('edit', task);
            if (act === 'budget') return openTaskModal('budget', task);
            runTaskAction(task.code, act);
        });
    });

    // Copy
    const copyBtn = qs('[data-copy]');
    if (copyBtn) {
        copyBtn.addEventListener('click', async () => {
            try { await navigator.clipboard.writeText(copyBtn.dataset.copy || ''); setNotice('taskNotice', t('tasks.task_code_copied'), 'ok'); }
            catch (e) { setNotice('taskNotice', String(e), 'error'); }
        });
    }
}

// ── Status actions (open/close/refund) ─────────────────────
async function runTaskAction(code, action) {
    if (action === 'refund' && !window.confirm(t('tasks.refund_confirm'))) return;
    setNotice('taskNotice', t('tasks.action_progress', {action}));
    try {
        const json = await requestJson(`/api/owner/tasks/${code}/${action}`, {method: 'POST', body: '{}'});
        if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
        state.selectedTask = json.data.task;
        renderTaskDetail(json.data.task);
        updateTaskInList(json.data.task);
        renderTasks();
        setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
    } catch (error) {
        setNotice('taskNotice', String(error), 'error');
    }
}

// ── Modal: unified for create / edit / budget ──────────────
function openTaskModal(mode, task) {
    closeModal(); // ensure clean
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.id = 'taskModal';

    if (mode === 'create') {
        overlay.innerHTML = `
            <div class="modal-panel">
                <div class="modal-head">
                    <h2>${escapeHtml(t('owner.task_new.heading'))}</h2>
                    <button class="modal-close" type="button" data-modal-close>×</button>
                </div>
                <form id="taskForm" class="modal-form" novalidate>
                    <label>${escapeHtml(t('owner.task_new.title'))}</label>
                    <input name="title" required maxlength="128">
                    <label>${escapeHtml(t('owner.task_new.requirements'))}</label>
                    <textarea name="requirements" required maxlength="20000" rows="4"></textarea>
                    <label>${escapeHtml(t('owner.task_new.post_api'))}</label>
                    <input name="postapi" required maxlength="2048" placeholder="${escapeHtml(t('owner.task_new.post_api_placeholder'))}">
                    <div class="row">
                        <div><label>${escapeHtml(t('owner.task_new.budget'))}</label><input name="budget" type="number" step="0.0001" min="1000" required></div>
                        <div><label>${escapeHtml(t('owner.task_new.price'))}</label><input name="price" type="number" step="0.0001" min="0.0001" required></div>
                    </div>
                    <label class="checkline"><input name="open_now" type="checkbox"> ${escapeHtml(t('owner.task_new.open_now'))}</label>
                    <div class="actions form-actions">
                        <button class="btn primary" type="submit">${escapeHtml(t('owner.task_new.create'))}</button>
                    </div>
                    <div id="taskModalNotice" class="notice"></div>
                </form>
            </div>`;
    } else if (mode === 'edit') {
        overlay.innerHTML = `
            <div class="modal-panel">
                <div class="modal-head">
                    <h2>${escapeHtml(t('tasks.edit_basics'))}</h2>
                    <button class="modal-close" type="button" data-modal-close>×</button>
                </div>
                <form id="taskEditForm" class="modal-form" novalidate>
                    <label>${escapeHtml(t('tasks.title'))}</label>
                    <input name="title" required maxlength="128" value="${escapeHtml(task.title)}">
                    <label>${escapeHtml(t('tasks.requirements'))}</label>
                    <textarea name="requirements" required maxlength="20000" rows="5">${escapeHtml(task.requirements)}</textarea>
                    <label>${escapeHtml(t('tasks.post_api'))}</label>
                    <input name="postapi" required maxlength="2048" value="${escapeHtml(task.postapi || '')}">
                    <label>${escapeHtml(t('task_new.price'))}</label>
                    <input name="price" type="number" step="0.0001" min="0.0001" required value="${Number(task.price).toFixed(4)}">
                    <div class="actions form-actions">
                        <button class="btn primary" type="submit">${escapeHtml(t('tasks.save_basics'))}</button>
                    </div>
                    <div id="taskModalNotice" class="notice"></div>
                </form>
            </div>`;
    } else if (mode === 'budget') {
        overlay.innerHTML = `
            <div class="modal-panel">
                <div class="modal-head">
                    <h2>${escapeHtml(t('tasks.add_budget'))}</h2>
                    <button class="modal-close" type="button" data-modal-close>×</button>
                </div>
                <form id="budgetFormModal" class="modal-form" novalidate>
                    <p class="muted">${escapeHtml(task.title)}</p>
                    <label>${escapeHtml(t('tasks.amount'))}</label>
                    <input name="amount" type="number" step="0.0001" min="0" required autofocus>
                    <div class="actions form-actions">
                        <button class="btn primary" type="submit">${escapeHtml(t('tasks.add_budget_submit'))}</button>
                    </div>
                    <div id="taskModalNotice" class="notice"></div>
                </form>
            </div>`;
    }

    document.body.appendChild(overlay);
    document.body.classList.add('modal-open');

    // Close
    overlay.querySelectorAll('[data-modal-close]').forEach((b) => b.addEventListener('click', closeModal));
    overlay.addEventListener('click', (e) => { if (e.target === overlay) closeModal(); });

    // Submit
    const form = overlay.querySelector('form');
    if (mode === 'create') bindCreateSubmit(form, overlay);
    else if (mode === 'edit') bindEditSubmit(form, task);
    else if (mode === 'budget') bindBudgetSubmit(form, task);

    // Focus
    const first = form.querySelector('input, textarea');
    if (first) first.focus();
}

function closeModal() {
    const m = qs('#taskModal');
    if (m) m.remove();
    document.body.classList.remove('modal-open');
}

function bindCreateSubmit(form, overlay) {
    form.addEventListener('submit', async (e) => {
        e.preventDefault();
        const data = payload(form);
        if (Number(data.budget) < 1000) return setNotice('taskModalNotice', t('tasks.create_budget_min'), 'error');
        data.open_now = form.elements.open_now.checked;
        setNotice('taskModalNotice', t('tasks.creating'));
        try {
            const json = await requestJson('/api/owner/tasks', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskModalNotice', json.error || json, 'error');
            closeModal();
            await loadTasks();
            renderTasks();
            if (json.data?.task?.code) await selectTask(json.data.task.code);
            setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
        } catch (err) { setNotice('taskModalNotice', String(err), 'error'); }
    });
}

function bindEditSubmit(form, task) {
    form.addEventListener('submit', async (e) => {
        e.preventDefault();
        setNotice('taskModalNotice', t('tasks.saving'));
        const data = payload(form);
        try {
            const json = await requestJson(`/api/owner/tasks/${task.code}/edit`, {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskModalNotice', json.error || json, 'error');
            closeModal();
            state.selectedTask = json.data.task;
            renderTaskDetail(json.data.task);
            updateTaskInList(json.data.task);
            renderTasks();
            setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
        } catch (err) { setNotice('taskModalNotice', String(err), 'error'); }
    });
}

function bindBudgetSubmit(form, task) {
    form.addEventListener('submit', async (e) => {
        e.preventDefault();
        setNotice('taskModalNotice', t('tasks.adding_budget'));
        const data = payload(form);
        try {
            const json = await requestJson(`/api/owner/tasks/${task.code}/add-budget`, {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskModalNotice', json.error || json, 'error');
            closeModal();
            state.selectedTask = json.data.task;
            renderTaskDetail(json.data.task);
            updateTaskInList(json.data.task);
            renderTasks();
            setNotice('taskNotice', t('tasks.budget_added'), 'ok');
        } catch (err) { setNotice('taskModalNotice', String(err), 'error'); }
    });
}

// ── Helpers ────────────────────────────────────────────────
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
