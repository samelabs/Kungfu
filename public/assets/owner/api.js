async function requestJson(url, options = {}) {
    const headers = Object.assign({'Content-Type': 'application/json'}, options.headers || {});
    const response = await fetch(url, Object.assign({}, options, {headers, credentials: 'same-origin'}));
    const text = await response.text();
    if (!text) {
        const error = new Error(t('js.empty_response', {status: response.status, url}));
        error.httpStatus = response.status;
        throw error;
    }
    try {
        const json = JSON.parse(text);
        if (json && typeof json === 'object') {
            Object.defineProperty(json, '_httpStatus', {value: response.status});
        }
        return json;
    } catch (error) {
        const preview = text.slice(0, 200);
        const parseError = new Error(t('js.invalid_json', {status: response.status, url, preview}));
        parseError.httpStatus = response.status;
        throw parseError;
    }
}

async function loadOwnerKey() {
    const json = await requestJson('/api/key', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || t('js.key_load_failed')));
    state.ownerKey = json.data.key || '';
}

async function loadAccount() {
    const json = await requestJson('/api/account', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || t('js.account_load_failed')));
    state.account = json.data;
    state.name = json.data.bot_name || state.name;
}

async function loadTasks() {
    const json = await requestJson('/api/owner/tasks', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || t('js.task_load_failed')));
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
    if (!json.success) throw new Error(noticeText(json.error || t('js.log_load_failed')));

    state.logs.items = json.data.items || [];
    state.logs.total = Number(json.data.pagination?.total || 0);
    state.logs.totalPages = Number(json.data.pagination?.total_pages || 1);
    state.logs.page = Number(json.data.pagination?.page || state.logs.page);
    state.logs.balance = Number(json.data.balance || 0);
    if (Array.isArray(json.data.tasks)) {
        state.logs.tasks = json.data.tasks;
    }
}
