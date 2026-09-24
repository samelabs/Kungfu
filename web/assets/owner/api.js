async function requestJson(url, options = {}) {
    const headers = Object.assign({'Content-Type': 'application/json'}, options.headers || {});
    const response = await fetch(url, Object.assign({}, options, {headers, credentials: 'same-origin'}));
    const text = await response.text();
    if (!text) {
        const error = apiError('EMPTY_RESPONSE', t('js.empty_response', {status: response.status, url}), response.status);
        throw error;
    }
    let json;
    try {
        json = JSON.parse(text);
    } catch (error) {
        const preview = text.slice(0, 200);
        throw apiError('INVALID_JSON', t('js.invalid_json', {status: response.status, url, preview}), response.status);
    }
    if (json && typeof json === 'object') {
        Object.defineProperty(json, '_httpStatus', {value: response.status});
    }
    return json;
}

// apiErrorFrom converts a server error envelope ({success:false,
// error:{code,message,details}}) into a structured ApiError carrying
// the machine code and HTTP status — so lifecycle layers branch on
// error.code, never on message strings.
function apiErrorFrom(json, fallbackMessageKey) {
    const env = json && typeof json === 'object' ? json : {};
    const err = env.error && typeof env.error === 'object' ? env.error : env;
    const code = String(err.code || env.code || '');
    const message = err.message || t(fallbackMessageKey);
    const httpStatus = (env._httpStatus || env.httpStatus || 0);
    return apiError(code, message, httpStatus, err.details || null);
}

// loadOwnerKey fetches MASKED key metadata only — the full current
// key is not recoverable from the server; nothing here can ever
// repopulate a raw credential.
async function loadOwnerKey() {
    const json = await requestJson('/api/key', {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'js.key_load_failed');
    state.keyMasked = json.data.key_masked || '';
}

async function loadAccount() {
    const json = await requestJson('/api/account', {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'js.account_load_failed');
    state.account = json.data;
    state.name = json.data.bot_name || state.name;
}

async function loadTasks() {
    const json = await requestJson('/api/owner/tasks', {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'js.task_load_failed');
    state.tasks = json.data.tasks || [];
}

async function loadLogs() {
    const params = new URLSearchParams({
        type: state.logs.type,
        page: String(state.logs.page),
        page_size: String(state.logs.pageSize)
    });
    if (state.logs.type === 'task' && state.logs.taskCode) {
        params.set('task_code', state.logs.taskCode);
    }
    const json = await requestJson(`/api/owner/logs?${params.toString()}`, {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'js.log_load_failed');

    state.logs.items = json.data.items || [];
    state.logs.total = Number(json.data.pagination?.total || 0);
    state.logs.totalPages = Number(json.data.pagination?.total_pages || 1);
    state.logs.page = Number(json.data.pagination?.page || state.logs.page);
    // balance is a canonical decimal string (economic integer wire
    // contract) — preserved verbatim, never Number()-converted.
    state.logs.balance = typeof json.data.balance === 'string' ? json.data.balance : String(json.data.balance || 0);
    if (Array.isArray(json.data.tasks)) {
        state.logs.tasks = json.data.tasks;
    }
}
