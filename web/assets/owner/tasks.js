function bindTaskCreateForm() {
    if (!qs('#taskForm')) return;
    qs('#taskForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        if (Number(data.budget) < 1000) {
            return setNotice('taskCreateNotice', t('tasks.create_budget_min'), 'error');
        }
        data.open_now = event.currentTarget.elements.open_now.checked;
        setNotice('taskCreateNotice', t('tasks.creating'));
        try {
            const json = await requestJson('/api/owner/tasks', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('taskCreateNotice', json.error || json, 'error');
            window.location.href = ownerUrl('/owner/tasks');
        } catch (error) {
            setNotice('taskCreateNotice', String(error), 'error');
        }
    });
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
            await loadTasks();
            await selectTask(state.selectedTask.code);
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
