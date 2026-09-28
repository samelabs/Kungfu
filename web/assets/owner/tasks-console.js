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
    requirements: "Fetch the given page and return exactly three summary bullets for a newsletter.",
    output: {
        schema: {
            type: "object",
            properties: {
                url: {type: "string"},
                bullets: {type: "array", items: {type: "string"}, minItems: 3, maxItems: 3}
            },
            required: ["url", "bullets"]
        }
    },
    receiver: {url: "https://example.com/kungfu/receiver"},
    sample: {url: "https://example.com/a", bullets: ["s1", "s2", "s3"]},
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
        tcvRenderSimpleForm();
        return;
    }
    const code = window.location.pathname.split('/').pop();
    tcvCall('task_get', {code}).then((env) => {
        if (!env.ok) { tcvRenderEditorError(env); return; }
        tcvRenderEditor({code, view: env, status: env.status, contract: env.contract});
    }).catch((error) => {
        root.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

// ---- simple mode (task_new default): title / requirements / receiver
// URL / sample / price / units (+ open now). The required contract
// fields go to task_create with budget = price * units and open as
// chosen (opening test-delivers the sample to the receiver).
function tcvRenderSimpleForm() {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    root.innerHTML = `
    <div class="panel">
        <div class="section-head">
            <div class="section-head-copy">
                <h2>${escapeHtml(t('tasks.mode_title'))}</h2>
            </div>
            <div class="section-head-actions">
                <button class="btn" id="tcvAdvancedBtn" type="button">${escapeHtml(t('tasks.advanced'))}</button>
            </div>
        </div>
        <div id="taskEditorStatus" class="keybox" hidden></div>
        <label>${escapeHtml(t('tasks.f_title'))}</label>
        <input id="tcvSTitle" maxlength="128">
        <label>${escapeHtml(t('tasks.f_requirements'))}</label>
        <textarea id="tcvSRequirements" rows="5" maxlength="20000"></textarea>
        <label>${escapeHtml(t('tasks.f_receiver'))}</label>
        <input id="tcvSReceiver" type="url" placeholder="https://">
        <label>${escapeHtml(t('tasks.f_sample'))}</label>
        <textarea id="tcvSSample" class="mono" rows="3" spellcheck="false">{"result": "example"}</textarea>
        <div class="tcv-pair">
            <div>
                <label>${escapeHtml(t('tasks.f_price'))}</label>
                <input id="tcvSPrice" type="number" min="1" value="5">
            </div>
            <div>
                <label>${escapeHtml(t('tasks.f_units'))}</label>
                <input id="tcvSUnits" type="number" min="1" value="1">
            </div>
        </div>
        <dl class="sl-kv">
            <dt>${escapeHtml(t('tasks.f_total'))}</dt><dd id="tcvSTotal">5</dd>
            <dt>${escapeHtml(t('tasks.f_balance'))}</dt><dd id="tcvSBalance">…</dd>
        </dl>
        <label><input type="checkbox" id="tcvSOpen" checked> ${escapeHtml(t('tasks.open_now'))}</label>
        <div class="actions">
            <button class="btn primary" id="tcvSPublish" type="button">${escapeHtml(t('tasks.publish'))}</button>
        </div>
    </div>`;
    const total = () => {
        const v = Math.max(1, Number(qs('#tcvSUnits').value || 0)) * Math.max(0, Number(qs('#tcvSPrice').value || 0));
        qs('#tcvSTotal').textContent = String(v);
    };
    qs('#tcvSUnits').addEventListener('input', total);
    qs('#tcvSPrice').addEventListener('input', total);
    qs('#tcvAdvancedBtn').addEventListener('click', () => {
        tcvRenderEditor({code: null, contract: TCV_SAMPLE_CONTRACT, budget: 2000, status: 'new'});
    });
    requestJson('/api/owner/logs?log_type=credits&page=1&page_size=1', {method: 'GET'})
        .then((json) => {
            const b = qs('#tcvSBalance');
            if (b && json && json.success) b.textContent = String(json.data.balance ?? '0');
        }).catch(() => {});
    qs('#tcvSPublish').addEventListener('click', () => {
        const title = qs('#tcvSTitle').value.trim();
        const requirements = qs('#tcvSRequirements').value.trim();
        const receiverURL = qs('#tcvSReceiver').value.trim();
        let sample = null;
        try { sample = JSON.parse(qs('#tcvSSample').value); } catch (e) { sample = null; }
        const price = Number(qs('#tcvSPrice').value || 0);
        const units = Math.max(1, Number(qs('#tcvSUnits').value || 1));
        if (!title) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.need_title')}}); return; }
        if (!requirements) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.need_requirements')}}); return; }
        if (!receiverURL) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.need_receiver')}}); return; }
        if (!sample || typeof sample !== 'object' || Array.isArray(sample)) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.need_sample')}}); return; }
        if (!Number.isInteger(price) || price < 1) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.need_price')}}); return; }
        const totalBudget = price * units;
        // budget = price × units must stay a safe integer: task money
        // is capped at 2^53-1 server-side (task.MaxAmount); anything
        // larger is silently corrupted by JS Number, so refuse it here.
        if (!Number.isSafeInteger(totalBudget)) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: t('tasks.total_too_large')}}); return; }
        tcvCall('task_create', {
            contract: {title, requirements, receiver: {url: receiverURL}, sample, price},
            budget: totalBudget,
            open: qs('#tcvSOpen').checked
        }).then((env) => {
            tcvShowStatus(env);
            if (env.ok && env.code) window.location.href = `/owner/tasks/${env.code}`;
        }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
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
            <input id="tcvBudget" type="number" min="1" value="${state.budget ?? 5}" ${isNew ? '' : 'disabled'}>
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
        <h2>${escapeHtml(t('tasks.deliveries'))}</h2>
        <div class="actions" id="tcvSubFilters">
            <select id="tcvSubState">
                <option value="">${escapeHtml(t('tasks.all_states'))}</option>
                <option value="settled">settled</option>
                <option value="rejected">rejected</option>
                <option value="failed">failed</option>
            </select>
        </div>
        <div id="tcvSubmissions"><p class="muted">${escapeHtml(t('tasks.loading'))}</p></div>
    </section>`;
    tcvBindEditor(isNew ? null : codeAttr);
    if (!isNew) tcvLoadSubmissions(codeAttr, '');
}

function tcvActionButtons(status) {
    const btn = (id, label, show) => show
        ? `<button class="btn" id="${id}" type="button">${escapeHtml(label)}</button>` : '';
    return btn('tcvOpen', t('tasks.open'), status === 'draft' || status === 'paused')
        + btn('tcvPause', t('tasks.pause'), status === 'open')
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

// ---- submissions: the delivery record (state + the receiver's reply) ----

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
        const reply = r.reply ? `HTTP ${r.reply.status} ${r.reply.body || ''}` : (r.failure || '');
        const short = reply.length > 200 ? reply.slice(0, 200) + '…' : reply;
        return `<div class="task-item">
            <div class="task-facts">
                <span class="mono">#${r.submission_id}</span>
                <span class="badge">${tcvEscapeHtml(r.state)}</span>
                <span class="mono">${tcvEscapeHtml(r.agent_ref || '')}</span>
                <span>v${r.version}</span>
                <span>${tcvEscapeHtml(r.created_at || '')}</span>
            </div>
            <details><summary class="mono">${tcvEscapeHtml(short)}</summary><pre class="mono">${tcvEscapeHtml(reply)}</pre></details>
        </div>`;
    }).join('');
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
