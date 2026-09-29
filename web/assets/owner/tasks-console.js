// tasks-console: the Task 1.0 owner console. All server access goes
// through POST /api/owner/tool/{task_*} (plus the read-only
// memory_list for the harness picker) — the unified registry behind
// the same §8.2 envelope as MCP and /api/v1. No local business rules:
// the server validates and decides; this layer only renders.

function tcvCall(tool, args) {
    return requestJson(`/api/owner/tool/${tool}`, {method: 'POST', body: JSON.stringify(args || {})})
        .then((json) => {
            // The tool bridge speaks the §8.2 envelope: ok / error —
            // there is no success/data wrapper. The raw envelope IS the
            // data (callers read env.tasks, env.contract, env.ok...),
            // and a false ok is data for the caller to display, not a
            // transport failure. Only a reply that is not an envelope
            // (not an object, or no boolean ok) fails the load.
            if (!json || typeof json !== 'object' || typeof json.ok !== 'boolean') {
                throw apiErrorFrom(json, 'js.task_load_failed');
            }
            return json;
        });
}

// ---- shared projections ----

// tcvT reads one owner.tasks.* key.
function tcvT(key, vars) { return t(`tasks.${key}`, vars || {}); }

// tcvTrans returns the translated value for a code family, falling
// back to the raw code when no translation exists (unknown reasons
// stay visible verbatim).
function tcvTrans(prefix, code) {
    const key = `tasks.${prefix}${String(code || '')}`;
    const value = t(key);
    return value === key ? String(code || '') : value;
}

function tcvStateText(state) { return tcvTrans('state_', state); }
function tcvStatusText(status) { return tcvTrans('status_', status); }
function tcvFailureText(code) { return code ? tcvTrans('fail_', code) : ''; }

// tcvPauseReasonText: RECEIVER_FAULT carries a full explanatory note;
// any other reason code shows verbatim.
function tcvPauseReasonText(reason) {
    return reason ? tcvTrans('reason_', reason) : '';
}

function tcvStatusBadge(status, extra) {
    const cls = ['draft', 'open', 'paused', 'closed'].includes(status) ? ` ${status}` : '';
    return `<span class="badge${cls}">${tcvEscapeHtml(tcvStatusText(status))}${extra ? tcvEscapeHtml(extra) : ''}</span>`;
}

function tcvFmtDate(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return String(iso);
    // follow the page language (set by the server on <html lang>);
    // fall back to the browser locale when it is absent
    const locale = document.documentElement.lang || undefined;
    try {
        return d.toLocaleString(locale, {dateStyle: 'medium', timeStyle: 'short'});
    } catch (e) {
        return d.toISOString().slice(0, 16).replace('T', ' ');
    }
}

function tcvShowStatus(env) {
    const box = qs('#taskEditorStatus');
    if (!box) return;
    box.hidden = false;
    box.innerHTML = tcvStatusLine(env);
    if (env && env.ok) setTimeout(() => { box.hidden = true; }, 2500);
}

// ---- the unified contract form (F1) ----
//
// One form serves the create page and the detail page's contract
// area, in editable or read-only mode. Known fields are edited;
// UNKNOWN fields of the source contract are carried through to every
// read() so the server can reject them by name (never silently
// dropped here). The "Advanced (JSON)" toggle is a two-way view of
// the same data: form → JSON serializes the current form, JSON →
// form parses back into the fields; unparsable JSON keeps the JSON
// view with a message.

function tcvClone(v) { return v === undefined || v === null ? undefined : JSON.parse(JSON.stringify(v)); }

function tcvPretty(v) { return JSON.stringify(v, null, 2); }

function tcvContractForm(mount, opts) {
    const editable = opts.editable !== false;
    let src = tcvClone(opts.contract) || {};
    if (typeof src !== 'object' || Array.isArray(src)) src = {};
    let harness = Array.isArray(src.harness_refs) ? src.harness_refs.slice(0, 10) : [];
    let jsonMode = false;
    let memoriesRequested = false;
    const disabled = editable ? '' : ' disabled';
    const memTitles = {}; // code → title, from memory_list

    mount.innerHTML = `
    <div class="tcv-mode">
        <button class="btn" id="tcvAdvancedBtn" type="button" hidden>${escapeHtml(t('tasks.advanced'))}</button>
        <button class="btn" id="tcvFormModeBtn" type="button" hidden>${escapeHtml(tcvT('form_mode'))}</button>
    </div>
    <div id="tcvFormArea">
        <p class="tcv-privacy" role="note">${escapeHtml(tcvT('privacy_notice'))}</p>
        <label for="tcvFTitle">${escapeHtml(tcvT('f_title'))}</label>
        <input id="tcvFTitle" maxlength="128"${disabled}>
        <div class="tcv-counter muted" id="tcvTitleCount"></div>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_title_hint'))}</p>

        <label for="tcvFRequirements">${escapeHtml(tcvT('f_requirements'))}</label>
        <textarea id="tcvFRequirements" rows="5" maxlength="20000"${disabled}></textarea>
        <div class="tcv-counter muted" id="tcvRequirementsCount"></div>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_requirements_hint'))}</p>

        <label>${escapeHtml(tcvT('f_harness'))}</label>
        <p class="tcv-hint tcv-harness-visible">${escapeHtml(tcvT('f_harness_visibility'))}</p>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_harness_hint'))}</p>
        <div id="tcvHarnessChips" class="tcv-chips"></div>
        <p class="tcv-hint muted" id="tcvHarnessMsg"></p>
        <details id="tcvHarnessPick" class="tcv-picker">
            <summary>${escapeHtml(tcvT('f_harness_pick'))} <span class="muted">${escapeHtml(tcvT('f_harness_max'))}</span></summary>
            <div id="tcvHarnessOptions"><p class="muted">${escapeHtml(tcvT('f_harness_loading'))}</p></div>
        </details>

        <label for="tcvSReceiver">${escapeHtml(tcvT('f_receiver'))}</label>
        <input id="tcvSReceiver" type="url" placeholder="https://"${disabled}>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_receiver_hint'))}</p>

        <label for="tcvSSample">${escapeHtml(tcvT('f_sample'))}</label>
        <textarea id="tcvSSample" class="mono" rows="4" spellcheck="false"${disabled}></textarea>
        <p class="tcv-err" id="tcvSampleError" hidden></p>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_sample_hint'))}</p>

        <details id="tcvSchemaWrap" class="tcv-picker">
            <summary>${escapeHtml(tcvT('f_schema'))}</summary>
            <label for="tcvFSchema">${escapeHtml(tcvT('f_schema'))}</label>
            <textarea id="tcvFSchema" class="mono" rows="6" spellcheck="false"${disabled}></textarea>
            <p class="tcv-hint muted">${escapeHtml(tcvT('f_schema_hint'))}</p>
        </details>

        <details id="tcvRulesWrap" class="tcv-picker">
            <summary>${escapeHtml(tcvT('f_rules'))}</summary>
            <label for="tcvFMaxRejected">${escapeHtml(tcvT('f_max_rejected'))} <span class="muted">${escapeHtml(tcvT('default_value', {n: 5}))}</span></label>
            <input id="tcvFMaxRejected" type="number" min="1" max="50" placeholder="5"${disabled}>
            <p class="tcv-hint muted">${escapeHtml(tcvT('f_max_rejected_hint'))}</p>
            <label class="tcv-criterion"><input type="checkbox" id="tcvFClaimRequired"${disabled}> ${escapeHtml(tcvT('f_claim_required'))}</label>
            <p class="tcv-hint muted">${escapeHtml(tcvT('f_claim_required_hint'))}</p>
            <div class="tcv-pair">
                <div>
                    <label for="tcvFClaimTtl">${escapeHtml(tcvT('f_claim_ttl'))} <span class="muted">${escapeHtml(tcvT('default_value', {n: 1800}))}</span></label>
                    <input id="tcvFClaimTtl" type="number" min="300" max="7200" placeholder="1800"${disabled}>
                    <p class="tcv-hint muted">${escapeHtml(tcvT('f_claim_ttl_hint'))}</p>
                </div>
                <div>
                    <label for="tcvFClaimMaxDur">${escapeHtml(tcvT('f_claim_max_duration'))} <span class="muted">${escapeHtml(tcvT('default_value', {n: 7200}))}</span></label>
                    <input id="tcvFClaimMaxDur" type="number" min="600" max="86400" placeholder="7200"${disabled}>
                    <p class="tcv-hint muted">${escapeHtml(tcvT('f_claim_max_duration_hint'))}</p>
                </div>
            </div>
        </details>

        <label for="tcvFPrice">${escapeHtml(tcvT('f_price'))}</label>
        <input id="tcvFPrice" type="number" min="1" step="1"${disabled}>
        <p class="tcv-hint muted">${escapeHtml(tcvT('f_price_hint'))}</p>
    </div>
    <div id="tcvJsonArea" hidden>
        <label for="tcvContract">${escapeHtml(tcvT('contract_json'))}</label>
        <textarea id="tcvContract" class="mono" rows="18" spellcheck="false"></textarea>
        <p class="tcv-err" id="tcvJsonError" hidden></p>
    </div>`;

    const on = (sel, ev, fn) => { const el = qs(sel); if (el) el.addEventListener(ev, fn); };
    const counters = () => {
        const title = qs('#tcvFTitle');
        const req = qs('#tcvFRequirements');
        if (title) qs('#tcvTitleCount').textContent = `${title.value.length} / 128`;
        if (req) qs('#tcvRequirementsCount').textContent = `${req.value.length} / 20000`;
    };
    const changed = () => { counters(); if (opts.onChange) opts.onChange(); };

    // -- field ↔ object --

    function fillForm() {
        qs('#tcvFTitle').value = src.title || '';
        qs('#tcvFRequirements').value = src.requirements || '';
        harness = Array.isArray(src.harness_refs) ? src.harness_refs.slice(0, 10) : [];
        renderChips();
        qs('#tcvSReceiver').value = (src.receiver && src.receiver.url) || '';
        qs('#tcvSSample').value = src.sample === undefined ? '' : tcvPretty(src.sample);
        const schema = src.output && src.output.schema;
        qs('#tcvFSchema').value = schema === undefined ? '' : tcvPretty(schema);
        qs('#tcvFMaxRejected').value = (src.limits && src.limits.max_rejected_per_agent) || '';
        qs('#tcvFClaimRequired').checked = !!(src.claim && src.claim.required);
        qs('#tcvFClaimTtl').value = (src.claim && src.claim.ttl) || '';
        qs('#tcvFClaimMaxDur').value = (src.claim && src.claim.max_duration) || '';
        qs('#tcvFPrice').value = src.price !== undefined ? src.price : 5;
        syncPicker();
        counters();
    }

    // collect(strict) merges the form fields into a deep clone of src
    // (unknown fields ride along). strict=false keeps prior values on
    // parse/typing errors — the lenient shape used to serialize into
    // JSON mode; strict=true reports them for the caller to display.
    function collect(strict) {
        const c = tcvClone(src) || {};
        const bad = (msg) => ({ok: false, error: msg});
        const title = qs('#tcvFTitle').value.trim();
        if (title) c.title = title;
        else if (strict) return bad(tcvT('need_title'));
        else if (src.title !== undefined) c.title = src.title;
        const requirements = qs('#tcvFRequirements').value.trim();
        if (requirements) c.requirements = requirements;
        else if (strict) return bad(tcvT('need_requirements'));
        else if (src.requirements !== undefined) c.requirements = src.requirements;
        if (harness.length) c.harness_refs = harness.slice();
        else delete c.harness_refs;
        const url = qs('#tcvSReceiver').value.trim();
        if (url) c.receiver = Object.assign({}, c.receiver, {url});
        else if (strict) return bad(tcvT('need_receiver'));
        else if (c.receiver) c.receiver.url = (src.receiver && src.receiver.url) || '';
        const sampleRaw = qs('#tcvSSample').value.trim();
        if (sampleRaw) {
            try {
                const sample = JSON.parse(sampleRaw);
                if (sample && typeof sample === 'object' && !Array.isArray(sample)) c.sample = sample;
                else if (strict) return bad(tcvT('need_sample'));
            } catch (e) {
                if (strict) return bad(tcvT('need_sample'));
            }
        } else if (strict) return bad(tcvT('need_sample'));
        const schemaRaw = qs('#tcvFSchema').value.trim();
        if (schemaRaw) {
            try {
                const schema = JSON.parse(schemaRaw);
                if (schema && typeof schema === 'object' && !Array.isArray(schema)) {
                    c.output = Object.assign({}, c.output, {schema});
                } else if (strict) return bad(tcvT('need_schema'));
            } catch (e) {
                if (strict) return bad(tcvT('need_schema'));
            }
        } else if (c.output) {
            delete c.output.schema;
            if (!c.output || !Object.keys(c.output).length) delete c.output;
        }
        // rules: only non-default values are written into the contract
        // (an empty field means the default — it is omitted entirely)
        const numberField = (val) => {
            const raw = String(val).trim();
            if (raw === '') return undefined;
            const n = Number(raw);
            return Number.isFinite(n) ? n : undefined;
        };
        c.limits = tcvClone(c.limits) || {};
        const maxRej = numberField(qs('#tcvFMaxRejected').value);
        if (maxRej !== undefined && maxRej !== 5) c.limits.max_rejected_per_agent = maxRej;
        else delete c.limits.max_rejected_per_agent;
        if (!Object.keys(c.limits).length) delete c.limits;
        c.claim = tcvClone(c.claim) || {};
        if (qs('#tcvFClaimRequired').checked) c.claim.required = true;
        else delete c.claim.required;
        const ttl = numberField(qs('#tcvFClaimTtl').value);
        if (ttl !== undefined && ttl !== 1800) c.claim.ttl = ttl;
        else delete c.claim.ttl;
        const maxDur = numberField(qs('#tcvFClaimMaxDur').value);
        if (maxDur !== undefined && maxDur !== 7200) c.claim.max_duration = maxDur;
        else delete c.claim.max_duration;
        if (!Object.keys(c.claim).length) delete c.claim;
        const price = Number(qs('#tcvFPrice').value || 0);
        if (Number.isInteger(price) && price >= 1) c.price = price;
        else if (strict) return bad(tcvT('need_price'));
        else if (src.price !== undefined) c.price = src.price;
        return {ok: true, contract: c};
    }

    // -- mode switching --

    function showMode() {
        qs('#tcvFormArea').hidden = jsonMode;
        qs('#tcvJsonArea').hidden = !jsonMode;
        qs('#tcvAdvancedBtn').hidden = jsonMode || !editable;
        qs('#tcvFormModeBtn').hidden = !jsonMode || !editable;
    }

    function toJSONMode() {
        const r = collect(false);
        qs('#tcvContract').value = tcvPretty(r.contract);
        qs('#tcvJsonError').hidden = true;
        jsonMode = true;
        showMode();
    }

    function toFormMode() {
        let parsed;
        try { parsed = JSON.parse(qs('#tcvContract').value); } catch (e) { parsed = null; }
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
            const err = qs('#tcvJsonError');
            err.hidden = false;
            err.textContent = tcvT('json_invalid');
            return;
        }
        src = parsed;
        qs('#tcvJsonError').hidden = true;
        jsonMode = false;
        showMode();
        fillForm();
    }

    // -- harness picker (memory_list, own memories only) --

    function renderChips() {
        const box = qs('#tcvHarnessChips');
        if (!harness.length) { box.innerHTML = ''; return; }
        box.innerHTML = harness.map((code) => {
            const title = memTitles[code] ? ` — ${tcvEscapeHtml(memTitles[code])}` : '';
            const rm = editable
                ? ` <button class="tcv-chip-x" type="button" data-tcv-rm="${tcvEscapeHtml(code)}" title="${escapeHtml(tcvT('f_harness_remove'))}" aria-label="${escapeHtml(tcvT('f_harness_remove'))}">×</button>` : '';
            return `<span class="tcv-chip"><span class="mono">${tcvEscapeHtml(code)}</span>${title}${rm}</span>`;
        }).join('');
    }

    function syncPicker() {
        qsa('#tcvHarnessOptions input[type="checkbox"]').forEach((cb) => {
            cb.checked = harness.includes(cb.value);
        });
    }

    function loadMemories() {
        if (memoriesRequested) return;
        memoriesRequested = true;
        const box = qs('#tcvHarnessOptions');
        if (!editable) { box.innerHTML = `<p class="muted">${escapeHtml(tcvT('f_harness_hint'))}</p>`; return; }
        tcvCall('memory_list', {limit: 100}).then((env) => {
            if (!env.ok) { box.innerHTML = tcvStatusLine(env); return; }
            const items = env.kungfus || [];
            if (!items.length) {
                box.innerHTML = `<p class="muted">${escapeHtml(tcvT('f_harness_none'))}
                    <a href="/llms.txt" target="_blank" rel="noopener">${escapeHtml(tcvT('f_harness_docs'))}</a></p>`;
                return;
            }
            box.innerHTML = items.map((m) => {
                const code = tcvEscapeHtml(m.code || '');
                const title = tcvEscapeHtml(m.title || m.code || '');
                return `<label class="tcv-criterion"><input type="checkbox" value="${code}"> <span class="mono">${code}</span> ${title}</label>`;
            }).join('');
            items.forEach((m) => { if (m.code && m.title) memTitles[m.code] = m.title; });
            qsa('#tcvHarnessOptions input[type="checkbox"]').forEach((cb) => {
                cb.checked = harness.includes(cb.value);
                cb.addEventListener('change', () => {
                    const msg = qs('#tcvHarnessMsg');
                    if (msg) msg.textContent = '';
                    if (cb.checked) {
                        if (harness.length >= 10) {
                            cb.checked = false;
                            if (msg) msg.textContent = tcvT('f_harness_max');
                            return;
                        }
                        harness.push(cb.value);
                    } else {
                        harness = harness.filter((c) => c !== cb.value);
                    }
                    renderChips();
                    if (opts.onChange) opts.onChange();
                });
            });
            renderChips();
        }).catch((error) => {
            box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
        });
    }

    on('#tcvHarnessChips', 'click', (ev) => {
        const btn = ev.target.closest('[data-tcv-rm]');
        if (!btn) return;
        harness = harness.filter((c) => c !== btn.getAttribute('data-tcv-rm'));
        renderChips();
        syncPicker();
        if (opts.onChange) opts.onChange();
    });

    // sample must be a JSON object — checked as the field loses focus
    on('#tcvSSample', 'blur', () => {
        const raw = qs('#tcvSSample').value.trim();
        const err = qs('#tcvSampleError');
        if (!err) return;
        if (!raw) { err.hidden = true; return; }
        let v = null;
        try { v = JSON.parse(raw); } catch (e) { v = null; }
        if (!v || typeof v !== 'object' || Array.isArray(v)) {
            err.hidden = false;
            err.textContent = tcvT('need_sample');
        } else {
            err.hidden = true;
        }
    });

    ['#tcvFTitle', '#tcvFRequirements', '#tcvSReceiver', '#tcvSSample', '#tcvFSchema',
        '#tcvFMaxRejected', '#tcvFClaimRequired', '#tcvFClaimTtl', '#tcvFClaimMaxDur', '#tcvFPrice'
    ].forEach((sel) => on(sel, 'input', changed));
    on('#tcvAdvancedBtn', 'click', toJSONMode);
    on('#tcvFormModeBtn', 'click', toFormMode);

    fillForm();
    // the memory list loads lazily (WO-20): the picker fetches it on
    // first expand — the create/edit pages then make zero tool calls
    // beyond their own until the picker is actually opened
    const pick = qs('#tcvHarnessPick');
    if (pick) {
        pick.addEventListener('toggle', () => {
            if (pick.open && !memoriesRequested) loadMemories();
        });
    }
    showMode();

    // read() returns the contract as it should be sent: the JSON text
    // when the user is in JSON mode, the merged form otherwise.
    return {
        read() {
            if (jsonMode) {
                let parsed;
                try { parsed = JSON.parse(qs('#tcvContract').value); } catch (e) { parsed = null; }
                if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
                    return {ok: false, error: tcvT('json_invalid')};
                }
                return {ok: true, contract: parsed};
            }
            return collect(true);
        },
        getPrice() {
            if (jsonMode) {
                try {
                    const v = JSON.parse(qs('#tcvContract').value);
                    if (v && Number.isInteger(v.price) && v.price >= 1) return v.price;
                } catch (e) { /* fall through */ }
                return null;
            }
            const price = Number(qs('#tcvFPrice').value || 0);
            return Number.isInteger(price) && price >= 1 ? price : null;
        }
    };
}

// ---- /owner/tasks: the list (F4 + WO-19 Q3) — rendering lives in
// render-tasks-console.js; this loader owns the URL-backed query
// state (?q=&status=&page=, kept across refreshes) and fetches one
// server-filtered, server-paged page. ----

const tcvListState = {
    q: '',
    status: 'all', // all | draft | open | paused | closed
    page: 1,
    pageSize: 20,
    pages: 1
};

function tcvReadListStateFromURL() {
    const params = new URLSearchParams(window.location.search);
    tcvListState.q = (params.get('q') || '').trim();
    const status = params.get('status') || '';
    tcvListState.status = ['draft', 'open', 'paused', 'closed'].includes(status) ? status : 'all';
    const page = Number(params.get('page'));
    tcvListState.page = Number.isInteger(page) && page >= 1 ? page : 1;
}

function tcvWriteListStateToURL() {
    const params = new URLSearchParams();
    if (tcvListState.q) params.set('q', tcvListState.q);
    if (tcvListState.status !== 'all') params.set('status', tcvListState.status);
    if (tcvListState.page > 1) params.set('page', String(tcvListState.page));
    const lang = new URLSearchParams(window.location.search).get('lang');
    if (lang) params.set('lang', lang);
    const query = params.toString();
    history.replaceState(null, '', '/owner/tasks' + (query ? `?${query}` : ''));
}

function tcvLoadList() {
    tcvReadListStateFromURL();
    tcvCall('task_list', {
        q: tcvListState.q || undefined,
        status: tcvListState.status === 'all' ? undefined : tcvListState.status,
        page: tcvListState.page,
        page_size: tcvListState.pageSize
    }).then((env) => {
        tcvShowStatus(env);
        if (!env.ok) {
            // a failed listing shows the tool error where the list
            // would be — never an empty "no tasks yet" list
            const box = qs('#taskConsoleList');
            if (box) box.innerHTML = tcvStatusLine(env);
            return;
        }
        tcvRenderTaskList(env);
    }).catch((error) => {
        const box = qs('#taskConsoleList');
        if (box) box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

// ---- /owner/tasks/new: create (F2) ----

function tcvRenderSimpleForm() {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    root.innerHTML = `
    <div class="panel">
        <div class="section-head">
            <div class="section-head-copy">
                <h2>${escapeHtml(tcvT('new_task'))}</h2>
            </div>
        </div>
        <div id="tcvFormMount"></div>
        <div class="tcv-pair">
            <div>
                <label for="tcvSUnits">${escapeHtml(tcvT('f_units'))}</label>
                <input id="tcvSUnits" type="number" min="1" step="1" value="1">
            </div>
            <div>
                <label>${escapeHtml(tcvT('f_total'))}</label>
                <div class="keybox mono" id="tcvSTotal">—</div>
            </div>
        </div>
        <dl class="sl-kv">
            <dt>${escapeHtml(tcvT('f_balance'))}</dt><dd id="tcvSBalance">…</dd>
        </dl>
        <label class="tcv-criterion"><input type="checkbox" id="tcvSOpen" checked> ${escapeHtml(tcvT('open_now'))}</label>
        <p class="tcv-hint muted">${escapeHtml(tcvT('open_note'))}</p>
        <div class="actions">
            <button class="btn primary" id="tcvSPublish" type="button">${escapeHtml(tcvT('publish'))}</button>
        </div>
    </div>`;

    const total = () => {
        const price = form.getPrice();
        const units = Math.max(1, Math.trunc(Number(qs('#tcvSUnits').value || 1)) || 1);
        qs('#tcvSTotal').textContent = price === null ? '—' : String(price * units);
    };
    const form = tcvContractForm(qs('#tcvFormMount'), {contract: {}, editable: true, onChange: total});
    qs('#tcvSUnits').addEventListener('input', total);
    total();

    requestJson('/api/account', {method: 'GET'}).then((json) => {
        const b = qs('#tcvSBalance');
        if (b && json && json.success) b.textContent = String(json.data && json.data.balance !== undefined ? json.data.balance : '0');
    }).catch(() => {});

    qs('#tcvSPublish').addEventListener('click', () => {
        const r = form.read();
        if (!r.ok) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: r.error}}); return; }
        const price = r.contract.price;
        const units = Math.max(1, Math.trunc(Number(qs('#tcvSUnits').value || 1)) || 1);
        const budget = price * units;
        // budget = price × units must stay a safe integer: task money
        // is capped at 2^53-1 server-side (task.MaxAmount); anything
        // larger is silently corrupted by JS Number, so refuse it here.
        if (!Number.isSafeInteger(budget)) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: tcvT('total_too_large')}}); return; }
        tcvCall('task_create', {
            contract: r.contract,
            budget,
            open: qs('#tcvSOpen').checked
        }).then((env) => {
            tcvShowStatus(env);
            if (env.ok && env.code) window.location.href = `/owner/tasks/${env.code}`;
        }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
    });
}

// ---- the task workspace pages (WO-20): one section per page. The
// overview (/) and edit (/edit) pages load task_get only; the
// deliveries page (/deliveries) loads task_submissions only; nothing
// else is requested on any of them. ----

// tcvTaskCodeFromPath reads {code} out of /owner/tasks/{code}[/edit |
// /deliveries].
function tcvTaskCodeFromPath() {
    const parts = window.location.pathname.split('/').filter(Boolean);
    return parts.length >= 3 ? parts[2] : '';
}

function tcvEditorInit() {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    if (SECTION === 'task_new') {
        tcvRenderSimpleForm();
        return;
    }
    const code = tcvTaskCodeFromPath();
    if (SECTION === 'task_deliveries') {
        tcvRenderDeliveriesPage(code);
        return;
    }
    if (SECTION === 'task_edit') {
        tcvRenderEditPage(code);
        return;
    }
    // task_detail — the overview
    tcvCall('task_get', {code}).then((env) => {
        if (!env.ok) { tcvRenderEditorError(env); return; }
        tcvRenderOverview(code, env);
    }).catch((error) => {
        root.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

function tcvRenderEditorError(env) {
    const root = qs('#taskEditorRoot');
    if (root) root.innerHTML = tcvStatusLine(env);
}

function tcvHeaderHTML(code, view) {
    const status = view.status || 'draft';
    const reason = status === 'paused' && view.paused_reason
        ? tcvPauseReasonText(view.paused_reason)
        : (status === 'closed' && view.closed_reason ? String(view.closed_reason) : '');
    return `
    <h2>${escapeHtml(view.title || code)}</h2>
    <p class="task-facts">
        <span class="mono">${tcvEscapeHtml(code)}</span>
        ${tcvStatusBadge(status)}
        <span>${escapeHtml(tcvT('f_version'))} <span class="mono">v${Number(view.version ?? 0)}</span></span>
        <span class="muted">${escapeHtml(tcvFmtDate(view.created_at))}</span>
    </p>
    ${reason ? `<p class="tcv-err">${escapeHtml(reason)}</p>` : ''}`;
}

function tcvFundsHTML(view) {
    const closed = view.status === 'closed';
    const rows = [
        [tcvT('f_price'), Number(view.price ?? 0)],
        [tcvT('m_locked'), Number(view.budget_locked ?? 0)],
        [tcvT('m_settled'), Number(view.settled ?? 0)],
        [tcvT('m_reserved'), Number(view.reserved ?? 0)],
        [tcvT('m_refunded'), Number(view.refunded ?? 0)],
        [tcvT('m_available'), Number(view.available ?? 0)],
        [tcvT('m_slots'), Number(view.slots ?? 0)]
    ];
    return `<h2>${escapeHtml(tcvT('budget'))}</h2>
    <dl class="sl-kv">${rows.map(([k, v]) => `<dt>${escapeHtml(k)}</dt><dd>${v}</dd>`).join('')}</dl>
    ${closed ? '' : `
    <label for="tcvFundAmount">${escapeHtml(tcvT('amount'))}</label>
    <input id="tcvFundAmount" type="number" min="1" step="1">
    <div class="actions">
        <button class="btn" id="tcvFund" type="button">${escapeHtml(tcvT('add_budget_submit'))}</button>
    </div>`}`;
}

function tcvFmtPercent(v) { return v === null || v === undefined ? '—' : `${Math.round(v * 100)}%`; }

function tcvFmtSeconds(v) {
    if (v === null || v === undefined) return '—';
    return v > 60 ? tcvT('s_minutes', {n: Math.round(v / 60)}) : tcvT('s_seconds', {n: Math.round(v)});
}

function tcvStatsHTML(view) {
    const stats = view.stats || {};
    const rows = [
        [tcvT('s_accept'), tcvFmtPercent(stats.accept_rate)],
        [tcvT('s_median'), tcvFmtSeconds(stats.median_reply_seconds)],
        [tcvT('s_failure'), tcvFmtPercent(stats.failure_rate)],
        [tcvT('s_submissions'), String(Number(stats.submissions_30d ?? 0))],
        [tcvT('s_claims'), String(Number(stats.active_claims ?? 0))]
    ];
    return `<h2>${escapeHtml(tcvT('stats_heading'))}</h2>
    <dl class="sl-kv">${rows.map(([k, v]) => `<dt>${escapeHtml(k)}</dt><dd>${escapeHtml(String(v))}</dd>`).join('')}</dl>`;
}

function tcvLifecycleButtons(view) {
    const status = view.status;
    const btn = (id, key, show) => show
        ? `<button class="btn" id="${id}" type="button">${escapeHtml(tcvT(key))}</button>` : '';
    return btn('tcvOpen', 'open', status === 'draft' || status === 'paused')
        + btn('tcvPause', 'pause', status === 'open')
        + btn('tcvClose', 'close', status !== 'closed')
        + btn('tcvRefund', 'refund', status === 'closed' && Number(view.available ?? 0) > 0);
}

// tcvAskConfirm renders the inline second-click confirmation under the
// lifecycle buttons (no browser dialogs).
function tcvAskConfirm(message, fn) {
    const box = qs('#tcvConfirm');
    if (!box) { fn(); return; }
    box.innerHTML = `<p class="muted">${escapeHtml(message)}</p>
        <div class="actions">
            <button class="btn primary" id="tcvConfirmYes" type="button">${escapeHtml(tcvT('confirm_yes'))}</button>
            <button class="btn" id="tcvConfirmNo" type="button">${escapeHtml(tcvT('confirm_cancel'))}</button>
        </div>`;
    box.hidden = false;
    qs('#tcvConfirmYes').addEventListener('click', () => { box.hidden = true; box.innerHTML = ''; fn(); });
    qs('#tcvConfirmNo').addEventListener('click', () => { box.hidden = true; box.innerHTML = ''; });
}

// tcvContractSummaryHTML is the overview's read-only digest of the
// EFFECTIVE contract: the first 280 runes of requirements, the
// receiver endpoint, the price, the harness size and the execution
// rules — plus the full contract as an expandable read-only JSON.
function tcvContractSummaryHTML(view) {
    const c = view.contract || {};
    const excerpt = String(c.requirements || '');
    const rules = [
        c.claim && c.claim.required ? tcvT('sum_claim_required') : '',
        c.claim && c.claim.ttl ? tcvT('sum_claim_ttl', {n: c.claim.ttl}) : '',
        c.claim && c.claim.max_duration ? tcvT('sum_claim_max', {n: c.claim.max_duration}) : '',
        c.limits && c.limits.max_rejected_per_agent ? tcvT('sum_max_rejected', {n: c.limits.max_rejected_per_agent}) : ''
    ].filter(Boolean).join(' · ');
    return `<dl class="sl-kv">
        <dt>${escapeHtml(tcvT('f_requirements'))}</dt><dd>${escapeHtml(excerpt.length > 280 ? excerpt.slice(0, 280) + '…' : excerpt)}</dd>
        <dt>${escapeHtml(tcvT('f_receiver'))}</dt><dd class="mono">${tcvEscapeHtml(c.receiver && c.receiver.url ? c.receiver.url : '')}</dd>
        <dt>${escapeHtml(tcvT('f_price'))}</dt><dd>${Number(c.price ?? 0)}</dd>
        <dt>${escapeHtml(tcvT('f_harness'))}</dt><dd>${Number((c.harness_refs || []).length)}</dd>
        <dt>${escapeHtml(tcvT('f_rules'))}</dt><dd>${escapeHtml(rules || '—')}</dd>
    </dl>
    <details class="tcv-picker"><summary>${escapeHtml(tcvT('view_contract'))}</summary>
        <pre class="mono tcv-pre">${tcvEscapeHtml(tcvPretty(c))}</pre></details>`;
}

// tcvRenderOverview is /owner/tasks/{code}: ONLY task_get. Header,
// funds and stats, the read-only contract summary with the two entry
// buttons, and the lifecycle actions with their confirmations. No
// contract form, no memory_list, no task_submissions.
function tcvRenderOverview(code, view) {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    const status = view.status || 'draft';
    const editable = status === 'draft' || status === 'paused';
    const pending = view.draft_pending === true;
    const submissions30d = Number((view.stats || {}).submissions_30d ?? 0);

    root.innerHTML = `
    <div class="task-layout">
        <div>
            <div class="panel">${tcvFundsHTML(view)}</div>
            <div class="panel">${tcvStatsHTML(view)}</div>
        </div>
        <div class="panel">
            ${tcvHeaderHTML(code, view)}
            ${pending ? `<div class="keybox tcv-note">${escapeHtml(tcvT('draft_pending_note'))} <a href="/owner/tasks/${tcvEscapeHtml(code)}/edit">${escapeHtml(tcvT('edit_contract'))}</a></div>` : ''}
            <div class="actions">
                ${editable ? `<a class="btn primary" href="/owner/tasks/${tcvEscapeHtml(code)}/edit">${escapeHtml(tcvT('edit_contract'))}</a>` : ''}
                ${status === 'open' ? `<span class="muted">${escapeHtml(tcvT('open_readonly_note'))}</span>` : ''}
                <a class="btn" href="/owner/tasks/${tcvEscapeHtml(code)}/deliveries">${escapeHtml(tcvT('deliveries'))} (${submissions30d})</a>
            </div>
            <h3>${escapeHtml(tcvT('f_contract'))}</h3>
            ${tcvContractSummaryHTML(view)}
            <div class="actions">${tcvLifecycleButtons(view)}</div>
            <div id="tcvConfirm" hidden></div>
        </div>
    </div>`;

    const on = (id, fn) => { const el = qs(id); if (el) el.addEventListener('click', fn); };
    on('#tcvOpen', () => tcvAskConfirm(tcvT('confirm_open'), () => tcvLifecycle(code, 'task_open')));
    on('#tcvPause', () => tcvAskConfirm(tcvT('confirm_pause'), () => tcvLifecycle(code, 'task_pause')));
    on('#tcvClose', () => tcvAskConfirm(tcvT('confirm_close'), () => tcvLifecycle(code, 'task_close')));
    on('#tcvRefund', () => tcvAskConfirm(
        tcvT('confirm_refund', {amount: Number(view.available ?? 0)}),
        () => tcvLifecycle(code, 'task_refund')));
    on('#tcvFund', () => {
        const amount = Number(qs('#tcvFundAmount').value || 0);
        tcvLifecycle(code, 'task_fund', {code, amount});
    });

    // a save on the edit page bounces back here with ?saved=1
    const params = new URLSearchParams(window.location.search);
    if (params.get('saved') === '1') {
        showToast(tcvT('saved_notice'), 'ok');
        params.delete('saved');
        const query = params.toString();
        history.replaceState(null, '', window.location.pathname + (query ? `?${query}` : ''));
    }
}

// tcvRenderEditPage is /owner/tasks/{code}/edit: ONLY task_get, then
// the contract form when the task is draft or paused. Anything else
// explains why editing is unavailable.
function tcvRenderEditPage(code) {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    const back = `<p><a class="btn" href="/owner/tasks/${tcvEscapeHtml(code)}">${escapeHtml(tcvT('back_to_overview'))}</a></p>`;
    tcvCall('task_get', {code}).then((env) => {
        if (!env.ok) { root.innerHTML = back + tcvStatusLine(env); return; }
        const status = env.status || 'draft';
        if (status !== 'draft' && status !== 'paused') {
            const why = status === 'closed' ? tcvT('edit_blocked_closed') : tcvT('edit_blocked_open');
            root.innerHTML = `
            <div class="panel">
                <h2>${escapeHtml(tcvT('edit_contract'))}</h2>
                <p class="tcv-err">${escapeHtml(why)}</p>
                ${back}
            </div>`;
            return;
        }
        root.innerHTML = `
        <div class="panel">
            <div class="section-head">
                <div class="section-head-copy"><h2>${escapeHtml(tcvT('edit_contract'))}</h2></div>
                <div class="section-head-actions"><a class="btn" href="/owner/tasks/${tcvEscapeHtml(code)}">${escapeHtml(tcvT('back_to_overview'))}</a></div>
            </div>
            <div id="tcvFormMount"></div>
            <div class="actions">
                <button class="btn primary" id="tcvSave" type="button">${escapeHtml(tcvT('save_basics'))}</button>
            </div>
        </div>`;
        // with a saved draft (draft/paused) the form edits the draft —
        // the contract the next open applies
        const form = tcvContractForm(qs('#tcvFormMount'), {contract: env.draft || env.contract || {}, editable: true});
        qs('#tcvSave').addEventListener('click', () => {
            const r = form.read();
            if (!r.ok) { tcvShowStatus({ok: false, error: {code: 'VALIDATION_FAILED', message: r.error}}); return; }
            tcvCall('task_update', {code, contract: r.contract}).then((saveEnv) => {
                if (!saveEnv.ok) { tcvShowStatus(saveEnv); return; }
                // back to the overview with the saved notice
                window.location.href = `/owner/tasks/${code}?saved=1`;
            }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
        });
    }).catch((error) => {
        root.innerHTML = back + `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

// tcvRenderDeliveriesPage is /owner/tasks/{code}/deliveries: ONLY
// task_submissions, with the state filter and page carried in the URL
// (?state=&page=). The API returns no title, so the code heads the
// page.
function tcvRenderDeliveriesPage(code) {
    const root = qs('#taskEditorRoot');
    if (!root) return;
    const params = new URLSearchParams(window.location.search);
    const state = params.get('state') || '';
    const page = Math.max(1, Number(params.get('page')) || 1);

    root.innerHTML = `
    <section class="panel">
        <div class="section-head">
            <div class="section-head-copy"><h2 class="mono">${tcvEscapeHtml(code)}</h2></div>
            <div class="section-head-actions"><a class="btn" href="/owner/tasks/${tcvEscapeHtml(code)}">${escapeHtml(tcvT('back_to_overview'))}</a></div>
        </div>
        <div class="actions">
            <select id="tcvSubState" aria-label="${escapeHtml(tcvT('all_states'))}">
                <option value="">${escapeHtml(tcvT('all_states'))}</option>
                <option value="delivering"${state === 'delivering' ? ' selected' : ''}>${escapeHtml(tcvT('state_delivering'))}</option>
                <option value="uncertain"${state === 'uncertain' ? ' selected' : ''}>${escapeHtml(tcvT('state_uncertain'))}</option>
                <option value="settled"${state === 'settled' ? ' selected' : ''}>${escapeHtml(tcvT('state_settled'))}</option>
                <option value="rejected"${state === 'rejected' ? ' selected' : ''}>${escapeHtml(tcvT('state_rejected'))}</option>
                <option value="failed"${state === 'failed' ? ' selected' : ''}>${escapeHtml(tcvT('state_failed'))}</option>
            </select>
        </div>
        <div id="tcvSubmissions"><p class="muted">${escapeHtml(tcvT('sub_loading'))}</p></div>
        <div class="actions" id="tcvSubPager">
            <button class="btn" id="tcvSubPrev" type="button">${escapeHtml(tcvT('page_prev'))}</button>
            <div class="mono" id="tcvSubPageInfo"></div>
            <button class="btn" id="tcvSubNext" type="button">${escapeHtml(tcvT('page_next'))}</button>
        </div>
    </section>`;

    const stateSel = qs('#tcvSubState');
    if (stateSel) stateSel.addEventListener('change', () => tcvLoadSubmissions(code, stateSel.value, 1));
    tcvLoadSubmissions(code, state, page);
}

function tcvLifecycle(code, tool, extra) {
    if (!code) return;
    tcvCall(tool, extra || {code}).then((env) => {
        tcvShowStatus(env);
        if (env.ok) tcvEditorInit();
    }).catch((error) => tcvShowStatus({ok: false, error: {code: 'NETWORK', message: noticeText(error)}}));
}

// ---- submissions: the delivery record (state + the receiver's reply) ----

function tcvLoadSubmissions(code, state, page) {
    const box = qs('#tcvSubmissions');
    if (!box) return;
    if (page < 1) page = 1;
    // the filter and page live in the URL (?state=&page=) so a reload
    // or a shared link reproduces the same view
    const url = new URLSearchParams();
    if (state) url.set('state', state);
    if (page > 1) url.set('page', String(page));
    const lang = new URLSearchParams(window.location.search).get('lang');
    if (lang) url.set('lang', lang);
    const query = url.toString();
    history.replaceState(null, '', `/owner/tasks/${code}/deliveries` + (query ? `?${query}` : ''));
    const args = {code, page, page_size: 20};
    if (state) args.state = state;
    tcvCall('task_submissions', args).then((env) => {
        if (!env.ok) { box.innerHTML = tcvStatusLine(env); return; }
        const rows = env.submissions || [];
        const total = Number(env.total ?? rows.length);
        const pages = Math.max(1, Math.ceil(total / 20));
        box.innerHTML = rows.length ? rows.map(tcvSubmissionRow).join('')
            : `<p class="muted">${escapeHtml(tcvT('empty_deliveries'))}</p>`;
        const info = qs('#tcvSubPageInfo');
        if (info) info.textContent = tcvT('page_info', {page, pages, total});
        // onclick (not addEventListener): the pager buttons survive
        // each row re-render, so listeners must not accumulate
        const prev = qs('#tcvSubPrev');
        const next = qs('#tcvSubNext');
        if (prev) {
            prev.disabled = page <= 1;
            prev.onclick = () => tcvLoadSubmissions(code, state, page - 1);
        }
        if (next) {
            next.disabled = page >= pages;
            next.onclick = () => tcvLoadSubmissions(code, state, page + 1);
        }
    }).catch((error) => {
        box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
    });
}

// wire into the shared page lifecycle. Defined as a plain function
// and mounted by init.js right before its own decorateRenderPage()
// call — deterministic script order instead of load-time typeof
// probes (init.js, which declares renderPage, loads after this file).
function tcvDecorateRenderPage() {
    if (typeof renderPage !== 'function') return;
    const tcvOrigRenderPage = renderPage;
    renderPage = async function () {
        if (SECTION === 'tasks') { tcvLoadList(); return; }
        if (SECTION === 'task_new' || SECTION === 'task_detail' || SECTION === 'task_edit' || SECTION === 'task_deliveries') { tcvEditorInit(); return; }
        return tcvOrigRenderPage();
    };
}
