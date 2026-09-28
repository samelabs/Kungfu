// render-tasks-console: DOM projections for the Task 1.0 console
// (list rows, editor, stats, submissions queue). Pure rendering; all
// data arrives from /api/owner/tool/{task_*} calls.

function tcvEscapeHtml(s) {
    return String(s ?? '').replace(/[&<>"']/g, (c) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
}

function tcvTaskRow(task) {
    const code = tcvEscapeHtml(task.code);
    const title = tcvEscapeHtml(task.title || code);
    const status = tcvEscapeHtml(task.status);
    const v = task.version ?? 0;
    const locked = Number(task.budget_locked ?? 0);
    const settled = Number(task.settled ?? 0);
    const reserved = Number(task.reserved ?? 0);
    const available = Number(task.available ?? 0);
    const paused = task.paused_reason ? ` · ${tcvEscapeHtml(task.paused_reason)}` : '';
    return `<div class="task-item" data-task-code="${code}">
        <div class="task-title"><a href="/owner/tasks/${code}">${title}</a></div>
        <div class="task-facts">
            <span class="mono">${code}</span>
            <span class="badge">${status}${paused}</span>
            <span>v${v}</span>
            <span>🔒 ${locked}</span>
            <span>✔ ${settled}</span>
            <span>⏳ ${reserved}</span>
            <span>💰 ${available}</span>
        </div>
    </div>`;
}

function tcvRenderTaskList(tasks) {
    const box = qs('#taskConsoleList');
    if (!box) return;
    if (!Array.isArray(tasks) || tasks.length === 0) {
        box.innerHTML = `<p class="muted">${escapeHtml(t('tasks.empty'))}</p>`;
        return;
    }
    box.innerHTML = tasks.map(tcvTaskRow).join('');
}

function tcvStatusLine(env) {
    if (!env) return '';
    if (env.ok) return `<p class="tcv-ok">${escapeHtml(env.message || '')}</p>`;
    const err = env.error || {};
    let fields = '';
    if (Array.isArray((err.details || {}).errors)) {
        fields = err.details.errors.map((e) =>
            `<li><span class="mono">${escapeHtml(e.field || '')}</span>: ${escapeHtml(e.message || '')}</li>`).join('');
    }
    // single-fact details (e.g. TEST_DELIVERY_FAILED's status_code)
    let status = '';
    const code = (err.details || {}).status_code;
    if (code !== undefined && code !== null && code !== 0) {
        status = ` <span class="mono">HTTP ${escapeHtml(String(code))}</span>`;
    }
    return `<div class="task-error"><p class="tcv-err"><b>${escapeHtml(err.code || 'ERROR')}</b>${status} ${escapeHtml(err.message || '')}</p>` +
        (fields ? `<ul class="task-error-fields">${fields}</ul>` : '') + `</div>`;
}
