/* Admin API helper: fetch wrapper with automatic CSRF header for
 * mutations and 401 → /admin/login redirect. */
'use strict';

async function adminFetch(path, options = {}) {
    const headers = Object.assign({}, options.headers || {});
    if (options.body && !headers['Content-Type']) {
        headers['Content-Type'] = 'application/json';
    }
    const method = (options.method || 'GET').toUpperCase();
    if (method !== 'GET' && method !== 'HEAD' && state.principal) {
        headers['X-CSRF-Token'] = state.principal.csrf_token;
    }
    const response = await fetch(path, Object.assign({}, options, {
        headers,
        credentials: 'same-origin'
    }));
    if (response.status === 401 && SECTION !== 'login') {
        window.location.assign('/admin/login');
        throw new Error('session expired');
    }
    const text = await response.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch (e) { /* fallthrough */ }
    if (!json) {
        throw new Error('Invalid server response (' + response.status + ')');
    }
    json._httpStatus = response.status;
    return json;
}

function apiError(json, fallback) {
    if (json && json.error && json.error.message) return json.error.message;
    return fallback || 'Request failed';
}

async function adminGet(path) { return adminFetch(path); }

async function adminMutate(path, method, body) {
    return adminFetch(path, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body)
    });
}
