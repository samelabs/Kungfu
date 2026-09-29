// render-tasks-console: DOM projections for the Task 1.0 console
// (list rows + status filter, editor status lines, submissions rows).
// Pure rendering; all data arrives from /api/owner/tool/{task_*} calls
// in tasks-console.js.

function tcvEscapeHtml(s) {
    return String(s ?? '').replace(/[&<>"']/g, (c) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
}

// ---- the list (F4 + WO-19 Q3): server-side search box, status
// filter and pager. Query state (?q=&status=&page=) lives in the URL
// via tcvListState (tasks-console.js); this layer only renders and
// re-triggers tcvLoadList on changes. ----

function tcvFilterRow(active) {
    const items = [['all', 'filter_all'], ['open', 'status_open'],
        ['paused', 'status_paused'], ['closed', 'status_closed']];
    return `<div class="actions tcv-filter" id="tcvFilter">` + items.map(([value, key]) =>
        `<button class="btn${value === active ? ' primary' : ''}" type="button" data-tcv-filter="${value}">${escapeHtml(tcvT(key))}</button>`
    ).join('') + `</div>`;
}

// tcvPagerRow: prev/next plus "page x of y · total n"; buttons outside
// the page range are disabled, clicks re-fetch the target page.
function tcvPagerRow(total, page, pages) {
    if (total <= 0) return '';
    const prev = `<button class="btn" type="button" data-tcv-page="${page - 1}"${page <= 1 ? ' disabled' : ''}>${escapeHtml(tcvT('pager_prev'))}</button>`;
    const next = `<button class="btn" type="button" data-tcv-page="${page + 1}"${page >= pages ? ' disabled' : ''}>${escapeHtml(tcvT('pager_next'))}</button>`;
    return `<div class="actions tcv-pager" id="tcvPager">${prev}` +
        `<span class="mono">${escapeHtml(tcvT('pager_page', {page, pages}))} · ${escapeHtml(tcvT('pager_total', {total}))}</span>` +
        `${next}</div>`;
}

// tcvFact is one labeled value of a row (text labels, no emoji).
function tcvFact(label, value) {
    return `<span><span class="muted">${escapeHtml(label)}</span> ${tcvEscapeHtml(value)}</span>`;
}

function tcvTaskRow(task) {
    const code = tcvEscapeHtml(task.code);
    const title = tcvEscapeHtml(task.title || task.code);
    const status = String(task.status || '');
    const reason = status === 'paused' && task.paused_reason
        ? ` · ${tcvEscapeHtml(tcvPauseReasonText(task.paused_reason))}`
        : (status === 'closed' && task.closed_reason ? ` · ${tcvEscapeHtml(String(task.closed_reason))}` : '');
    const badge = tcvStatusBadge(status, reason);
    return `<div class="task-item" data-task-code="${code}">
        <div class="task-title"><a href="/owner/tasks/${code}">${title}</a>
            <span class="muted">${escapeHtml(tcvT('r_created'))} ${tcvEscapeHtml(tcvFmtDate(task.created_at))}</span></div>
        <div class="task-facts">
            <span class="mono">${code}</span>
            ${badge}
            ${tcvFact(tcvT('r_price'), String(Number(task.price ?? 0)))}
            ${tcvFact(tcvT('r_slots'), String(Number(task.slots ?? 0)))}
            ${tcvFact(tcvT('r_available'), String(Number(task.available ?? 0)))}
            ${tcvFact(tcvT('r_locked'), String(Number(task.budget_locked ?? 0)))}
        </div>
    </div>`;
}

function tcvRenderTaskList(env) {
    const box = qs('#taskConsoleList');
    if (!box) return;
    const tasks = Array.isArray(env.tasks) ? env.tasks : [];
    const total = Number(env.total ?? tasks.length);
    const page = Number.isInteger(Number(env.page)) && Number(env.page) >= 1 ? Number(env.page) : 1;
    const pageSize = Number.isInteger(Number(env.page_size)) && Number(env.page_size) >= 1 ? Number(env.page_size) : 20;
    const pages = Math.max(1, Math.ceil(total / pageSize));
    tcvListState.page = page;
    tcvListState.pages = pages;

    // empty states differ: a filter that matches nothing is "no match";
    // a truly empty account keeps the "no tasks yet" call to action.
    const filtered = Boolean(tcvListState.q) || tcvListState.status !== 'all';
    const emptyBlock = tasks.length
        ? `<div class="task-list">${tasks.map(tcvTaskRow).join('')}</div>`
        : (filtered
            ? `<p class="muted">${escapeHtml(tcvT('no_match'))}</p>`
            : `<p class="muted">${escapeHtml(tcvT('empty'))}</p>
    <div class="actions"><a class="btn primary" href="${ownerUrl('/owner/tasks/new')}">${escapeHtml(tcvT('new_task'))}</a></div>`);

    box.innerHTML = `
    <form class="tcv-search" id="tcvSearchForm" role="search">
        <input type="search" id="tcvSearchInput" value="${tcvEscapeHtml(tcvListState.q)}"
            placeholder="${escapeHtml(tcvT('search_placeholder'))}" aria-label="${escapeHtml(tcvT('search_placeholder'))}">
        <button class="btn primary" type="submit">${escapeHtml(tcvT('search_submit'))}</button>
    </form>
    ${tcvFilterRow(tcvListState.status)}
    ${emptyBlock}
    ${tcvPagerRow(total, page, pages)}`;

    const form = qs('#tcvSearchForm');
    if (form) {
        form.addEventListener('submit', (ev) => {
            ev.preventDefault();
            tcvListState.q = (qs('#tcvSearchInput').value || '').trim();
            tcvListState.page = 1;
            tcvWriteListStateToURL();
            tcvLoadList();
        });
    }
    const filter = qs('#tcvFilter');
    if (filter) {
        filter.addEventListener('click', (ev) => {
            const btn = ev.target.closest('[data-tcv-filter]');
            if (!btn) return;
            tcvListState.status = btn.getAttribute('data-tcv-filter') || 'all';
            tcvListState.page = 1;
            tcvWriteListStateToURL();
            tcvLoadList();
        });
    }
    const pager = qs('#tcvPager');
    if (pager) {
        pager.addEventListener('click', (ev) => {
            const btn = ev.target.closest('[data-tcv-page]');
            if (!btn || btn.disabled) return;
            const target = Number(btn.getAttribute('data-tcv-page'));
            if (!Number.isInteger(target) || target < 1 || target > pages || target === page) return;
            tcvListState.page = target;
            tcvWriteListStateToURL();
            tcvLoadList();
        });
    }
}

// ---- the delivery record rows (F3): time, state, agent_ref, version,
// amount, reply status; the full reply body expands ----

function tcvSubmissionRow(r, showVersion) {
    const state = String(r.state || '');
    const reply = r.reply || null;
    const replyStatus = reply ? `HTTP ${tcvEscapeHtml(String(reply.status))}` : '—';
    const failure = r.failure ? `<p class="tcv-err">${escapeHtml(tcvFailureText(r.failure))}</p>` : '';
    const body = reply && reply.body ? String(reply.body) : '';
    const preview = body.length > 160 ? `${body.slice(0, 160)}…` : body;
    const expandable = body
        ? `<details><summary class="mono">${tcvEscapeHtml(preview)}</summary><pre class="mono tcv-pre">${tcvEscapeHtml(body)}</pre></details>`
        : '';
    return `<div class="task-item">
        <div class="task-facts">
            <span class="badge">${escapeHtml(tcvStateText(state))}</span>
            <span class="mono">${tcvEscapeHtml(r.agent_ref || '')}</span>
            
            ${tcvFact(tcvT('r_price'), String(Number(r.amount ?? 0)))}
            <span class="mono">${replyStatus}</span>
            <span class="muted">${tcvEscapeHtml(tcvFmtDate(r.created_at))}</span>
        </div>
        ${failure}
        ${expandable}
    </div>`;
}

// ---- shared status line (tool envelope → display) ----

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
