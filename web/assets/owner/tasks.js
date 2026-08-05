// ════════════════════════════════════════════════════════════
// tasks.js — Task create + budget handlers (refactored)
// Create: inline modal instead of page navigation
// ════════════════════════════════════════════════════════════

function bindTaskCreateForm() {
    const btn = qs('#newTaskBtn');
    if (!btn) return;
    btn.addEventListener('click', showCreateModal);
}

function showCreateModal() {
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.innerHTML = `
        <div class="modal-panel">
            <h2>${escapeHtml(t('owner.task_new.heading'))}</h2>
            <form id="taskForm" class="task-create-form" novalidate>
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
                    <button class="btn" type="button" data-modal-close>${escapeHtml(t('owner.task_new.cancel'))}</button>
                </div>
                <div id="taskCreateNotice" class="notice">${escapeHtml(t('owner.task_new.notice'))}</div>
            </form>
        </div>
    `;
    document.body.appendChild(overlay);

    // Close handlers
    const closeBtn = overlay.querySelector('[data-modal-close]');
    closeBtn.addEventListener('click', () => overlay.remove());
    overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.remove(); });

    // Submit
    const form = overlay.querySelector('#taskForm');
    form.addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(form);
        if (Number(data.budget) < 1000) {
            return setNotice('taskCreateNotice', t('tasks.create_budget_min'), 'error');
        }
        data.open_now = form.elements.open_now.checked;
        setNotice('taskCreateNotice', t('tasks.creating'));
        try {
            const json = await requestJson('/api/owner/tasks', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskCreateNotice', json.error || json, 'error');
            // Success: close modal, reload task list, select new task
            overlay.remove();
            await loadTasks();
            renderTasks();
            if (json.data?.task?.code) {
                await selectTask(json.data.task.code);
            }
            setNotice('taskNotice', json.message || t('tasks.updated'), 'ok');
        } catch (error) {
            setNotice('taskCreateNotice', String(error), 'error');
        }
    });

    // Focus first input
    form.elements.title.focus();
}

function bindTaskBudgetForm() {
    if (!qs('#budgetForm')) return;
    qs('#budgetForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        if (!state.selectedTask) return;
        const form = event.currentTarget;
        const data = payload(form);
        setNotice('taskNotice', t('tasks.adding_budget'));
        try {
            const json = await requestJson(`/api/owner/tasks/${state.selectedTask.code}/add-budget`, {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskNotice', json.error || json, 'error');
            form.reset();
            // Update from response — no double fetch
            state.selectedTask = json.data.task;
            renderTaskDetail(json.data.task, state.selectedTaskLogs);
            updateTaskInList(json.data.task);
            renderTasks();
            setNotice('taskNotice', t('tasks.budget_added'), 'ok');
        } catch (error) {
            setNotice('taskNotice', String(error), 'error');
        }
    });
}

function bindTaskHandlers() {
    bindTaskCreateForm();
    bindTaskBudgetForm();
}
