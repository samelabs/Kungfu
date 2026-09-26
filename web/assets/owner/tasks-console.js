// tasks-console: the Task 1.0 owner console. All server access goes
// through POST /api/owner/tool/{task_*} — the unified registry behind
// the same §8.2 envelope as MCP and /api/v1. No local business rules.

function tcvCall(tool, args) {
    return requestJson(`/api/owner/tool/${tool}`, {method: 'POST', body: JSON.stringify(args || {})})
        .then((json) => {
            if (!json.success) throw apiErrorFrom(json, 'js.task_load_failed');
            return json.data;
        });
}

const TCV_SAMPLE_CONTRACT = {
    title: "Summarize a page",
    objective: "A 3-bullet summary of the given page, for a newsletter.",
    inputs: "A public URL fetched by the executor.",
    output: {
        description: "One JSON object with the bullets.",
        schema: {
            type: "object",
            properties: {
                url: {type: "string"},
                bullets: {type: "array", items: {type: "string"}, minItems: 3, maxItems: 3}
            },
            required: ["url", "bullets"]
        }
    },
    acceptance: {
        mode: "async",
        review_window: 3600,
        criteria: [{id: "C1", kind: "rule", description: "bullets are exactly three sentences"}]
    },
    boundaries: ["no credential material"],
    examples: [{
        payload: {url: "https://example.com/a", bullets: ["s1", "s2", "s3"]},
        accepted: true
    }],
    price: 5
};

function tcvShowStatus(env) {
    const box = qs('#taskEditorStatus');
    if (!box) return;
    box.hidden = false;
    box.innerHTML = tcvStatusLine(env);
    if (env && env.ok) setTimeout(() => { box.hidden = true; }, 2500);
}

// ---- /owner/tasks: the list ----

function tcvLoadList() {
    tcvCall('task_list', {}).then((env) => {
        tcvShowStatus(env);
        tcvRenderTaskList(env.tasks || []);
        if (qs('#taskConsoleList')) {
            const inv = task.CheckInvariants; // placeholder: no-op in browser
            void inv;
        }
    }).catch((error) => {
        const box = qs('#taskConsoleList');
        if (box) box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

// ---- /owner/tasks/new and /owner/tasks/{code}: the editor ----

function tcvEditorInit() {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    if (SECTION === 'task_new') {
        tcvRenderEditor({code: null, contract: TCV_SAMPLE_CONTRACT, budget: 2000, status: 'new'});
        return;
    }
    const code = window.location.pathname.split('/').pop();
    tcvCall('task_get', {code}).then((env) => {
        if (!env.ok) { tcvRenderEditorError(env); return; }
        tcvRenderEditor({code, view: env, status: env.status});
    }).catch((error) => {
        root.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

function tcvRenderEditorError(env) {
    const root = qs('#taskEditorRoot');
    if (root) root.innerHTML = tcvStatusLine(env);
}

function tcvRenderEditor(state) {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    const isNew = state.code === null;
    const codeAttr = isNew ? '' : tcvEscapeHtml(state.code);
    const contractJSON = JSON.stringify(state.contract, null, 2);
    const v = state.view || {};
    const status = state.status || 'draft';
    root.innerHTML = `
    <div class="task-layout">
        <div class="panel">
            <h2>${isNew ? escapeHtml(t('tasks.new_task')) : `<span class="mono">${codeAttr}</span>`}</h2>
            <p class="muted">${escapeHtml(t('tasks.status'))}: <span class="badge">${tcvEscapeHtml(status)}</span>${v.version !== undefined ? ` · v${v.version}` : ''}</p>
            <label>${escapeHtml(t('tasks.budget'))}</label>
            <input id="tcvBudget" type="number" min="1000" value="${state.budget ?? 2000}" ${isNew ? '' : 'disabled'}>
            <label>Contract JSON</label>
            <textarea id="tcvContract" class="mono" rows="18" spellcheck="false">${tcvEscapeHtml(contractJSON)}</textarea>
            <div class="actions">
                <button class="btn primary" id="tcvSave" type="button">${escapeHtml(t('tasks.save_basics'))}</button>
                ${tcvActionButtons(status)}
            </div>
            <div class="actions">
                <input id="tcvFundAmount" type="number" min="1" placeholder="${escapeHtml(t('tasks.amount'))}" ${status === 'closed' ? 'disabled' : ''}>
                <button class="btn" id="tcvFund" type="button" ${status === 'closed' ? 'disabled' : ''}>${escapeHtml(t('tasks.add_budget_submit'))}</button>
                <button class="btn" id="tcvRefund" type="button" ${status === 'closed' ? '' : 'disabled'}>${escapeHtml(t('tasks.refund'))}</button>
            </div>
        </div>
        <div class="panel" id="tcvStatsPanel">${tcvStatsHTML(v)}</div>
    </div>
    <section class="panel" id="tcvSubmissionsPanel">
        <h2>${escapeHtml(t('tasks.delivered'))}</h2>
        <div class="actions" id="tcvSubFilters">
            <select id="tcvSubState">
                <option value="under_review">under_review</option>
                <option value="">${escapeHtml(t('logs.all')) || 'all'}</option>
                <option value="settled">settled</option>
                <option value="rejected">rejected</option>
                <option value="failed">failed</option>
            </select>
        </div>
        <div id="tcvSubmissions"><p class="muted">${escapeHtml(t('tasks.loading'))}</p></div>
        <div id="tcvVerdictBox" hidden></div>
    </section>`;
    tcvBindEditor(isNew ? null : codeAttr);
    if (!isNew) tcvLoadSubmissions(codeAttr, 'under_review');
}

function tcvActionButtons(status) {
    const btn = (id, label, show) => show
        ? `<button class="btn" id="${id}" type="button">${escapeHtml(label)}</button>` : '';
    return btn('tcvOpen', t('tasks.open'), status === 'draft' || status === 'paused')
        + btn('tcvPause', t('tasks.pause') || 'Pause', status === 'open')
        + btn('tcvClose', t('tasks.close'), status !== 'closed');
}

function tcvStatsHTML(v) {
    const rows = [
        ['budget_locked', v.budget_locked], ['settled', v.settled],
        ['reserved', v.reserved], ['refunded', v.refunded],
        ['available', v.available], ['slots', v.slots]
    ];
    if (rows.every(([, val]) => val === undefined)) {
        return `<p class="muted">${escapeHtml(t('tasks.select_hint'))}</p>`;
    }
    return `<h2>${escapeHtml(t('tasks.budget'))}</h2><dl class="sl-kv">` +
        rows.map(([k, val]) => `<dt>${k}</dt><dd>${Number(val ?? 0)}</dd>`).join('') + `</dl>`;
}

function tcvBindEditor(code) {
    const on = (id, fn) => { const el = qs(id); if (el) el.addEventListener('click', fn); };
    on('#tcvSave', () => tcvSave(code));
    on('#tcvOpen', () => tcvLifecycle(code, 'task_open'));
    on('#tcvPause', () => tcvLifecycle(code, 'task_pause'));
    on('#tcvClose', () => tcvLifecycle(code, 'task_close'));
    on('#tcvFund', () => {
        const amount = Number(qs('#tcvFundAmount').value || 0);
        tcvLifecycle(code, 'task_fund', {code, amount});
    });
    on('#tcvRefund', () => tcvLifecycle(code, 'task_refund'));
    const stateSel = qs('#tcvSubState');
    if (stateSel) stateSel.addEventListener('change', () => tcvLoadSubmissions(code, stateSel.value));
}

function tcvReadContract() {
    const raw = qs('#tcvContract').value;
    return JSON.parse(raw); // parse errors surface via the catch below
}

function tcvSave(code) {
    let contract;
    try { contract = tcvReadContract(); }
    catch (e) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: String(e)}}); return; }
    const args = {contract};
    if (!code) args.budget = Number(qs('#tcvBudget').value || 0);
    else args.code = code;
    tcvCall(code ? 'task_update' : 'task_create', args).then((env) => {
        tcvShowStatus(env);
        if (env.ok && !code && env.code) window.location.href = `/owner/tasks/${env.code}`;
    }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
}

function tcvLifecycle(code, tool, extra) {
    if (!code) return;
    tcvCall(tool, extra || {code}).then((env) => {
        tcvShowStatus(env);
        if (env.ok) tcvEditorInit();
    }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
}

// ---- submissions queue + verdict ----

function tcvLoadSubmissions(code, state) {
    const box = qs('#tcvSubmissions');
    if (!box) return;
    const args = {code, page: 1, page_size: 20};
    if (state) args.state = state;
    tcvCall('task_submissions', args).then((env) => {
        if (!env.ok) { box.innerHTML = tcvStatusLine(env); return; }
        tcvRenderSubmissions(env.submissions || []);
    }).catch((error) => {
        box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

function tcvRenderSubmissions(rows) {
    const box = qs('#tcvSubmissions');
    if (!rows.length) {
        box.innerHTML = `<p class="muted">${escapeHtml(t('tasks.empty'))}</p>`;
        return;
    }
    box.innerHTML = rows.map((r) => {
        const payload = JSON.stringify(r.payload, null, 2) || '';
        const short = payload.length > 200 ? payload.slice(0, 200) + '…' : payload;
        const actions = r.state === 'under_review'
            ? `<div class="actions">
                 <button class="btn primary" type="button" data-verdict-accept="${r.submission_id}">✔ Accept</button>
                 <button class="btn" type="button" data-verdict-reject="${r.submission_id}">✘ Reject</button>
               </div>` : '';
        return `<div class="task-item">
            <div class="task-facts">
                <span class="mono">#${r.submission_id}</span>
                <span class="badge">${tcvEscapeHtml(r.state)}</span>
                <span class="mono">${tcvEscapeHtml(r.agent_ref || '')}</span>
                <span>v${r.version}</span>
                <span>${tcvEscapeHtml(r.created_at || '')}</span>
            </div>
            <details><summary class="mono">${tcvEscapeHtml(short)}</summary><pre class="mono">${tcvEscapeHtml(payload)}</pre></details>
            ${actions}
        </div>`;
    }).join('');
    qsa('[data-verdict-accept]').forEach((btn) =>
        btn.addEventListener('click', () => tcvVerdict(Number(btn.dataset.verdictAccept), true)));
    qsa('[data-verdict-reject]').forEach((btn) =>
        btn.addEventListener('click', () => tcvVerdictForm(Number(btn.dataset.verdictReject))));
}

function tcvVerdict(submissionID, accepted) {
    tcvCall('task_verdict', {submission_id: submissionID, verdict: {accepted}})
        .then((env) => { tcvShowStatus(env); if (env.ok) { tcvEditorInit(); } });
}

function tcvVerdictForm(submissionID) {
    const box = qs('#tcvVerdictBox');
    if (!box) return;
    box.hidden = false;
    box.innerHTML = `
    <div class="panel">
        <h3>Reject #${submissionID}</h3>
        <label>criteria (comma-separated, e.g. C1)</label>
        <input id="tcvVCriteria" placeholder="C1">
        <label>reason (≤ 500)</label>
        <textarea id="tcvVReason" rows="2" maxlength="500"></textarea>
        <label><input type="checkbox" id="tcvVRetryable" checked> retryable</label>
        <div class="actions"><button class="btn danger" id="tcvVSubmit" type="button">Reject</button></div>
    </div>`;
    qs('#tcvVSubmit').addEventListener('click', () => {
        const criteria = qs('#tcvVCriteria').value.split(',').map((s) => s.trim()).filter(Boolean);
        const reason = qs('#tcvVReason').value.trim();
        if (!criteria.length || !reason) {
            tcvShowStatus({ok: false, error: {code: 'VERDICT_INVALID', message: 'criteria and reason are required'}});
            return;
        }
        tcvCall('task_verdict', {submission_id: submissionID, verdict: {
            accepted: false, criteria, reason, retryable: qs('#tcvVRetryable').checked
        }}).then((env) => { tcvShowStatus(env); if (env.ok) { box.hidden = true; tcvEditorInit(); } });
    });
}

// wire into the shared page lifecycle
const tcvOrigRenderPage = typeof renderPage === 'function' ? renderPage : null;
if (tcvOrigRenderPage) {
    renderPage = async function () {
        if (SECTION === 'tasks') { tcvLoadList(); return; }
        if (SECTION === 'task_new' || SECTION === 'task_detail') { tcvEditorInit(); return; }
        return tcvOrigRenderPage();
    };
}
