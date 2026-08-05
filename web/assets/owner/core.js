const SECTION = document.body.dataset.section;
const OWNER_I18N = window.OWNER_I18N || {};
const RESERVED_BOT_NAMES = new Set(['admin', 'root', 'system', 'api', 'web']);
const API_KEY_PATTERN = /kf_live_[a-f0-9]{64}/i;
const state = {
    name: '',
    ownerKey: '',
    account: null,
    tasks: [],
    selectedTask: null,
    logs: {
        type: 'credits',
        page: 1,
        pageSize: 20,
        totalPages: 1,
        total: 0,
        taskCode: '',
        tasks: [],
        items: [],
        balance: 0
    }
};

function currentLocale() {
    return window.APP_LOCALE || document.body.dataset.locale || 'en';
}

function ownerUrl(path) {
    const url = new URL(path, window.location.origin);
    url.searchParams.set('lang', currentLocale());
    return `${url.pathname}${url.search}`;
}

function qs(selector) { return document.querySelector(selector); }
function qsa(selector) { return Array.from(document.querySelectorAll(selector)); }
function t(key, vars = {}) {
    const parts = String(key).split('.');
    let value = OWNER_I18N;
    for (const part of parts) {
        if (!value || typeof value !== 'object' || !(part in value)) {
            return key;
        }
        value = value[part];
    }
    if (typeof value !== 'string') return key;
    return value.replace(/\{\{(\w+)\}\}/g, (_, name) => String(vars[name] ?? ''));
}
function humanStatus(value) {
    const key = `status.${String(value || '').toLowerCase()}`;
    const translated = t(key);
    return translated === key ? String(value || '') : translated;
}
function humanTaskStatus(value) {
    const key = `tasks.status_${String(value || '').toLowerCase()}`;
    const translated = t(key);
    return translated === key ? humanStatus(value) : translated;
}
function humanLogAction(value) {
    const raw = String(value || '');
    const map = {
        'Task check': 'logs.action_task_check',
        'Delivery accepted': 'logs.action_delivery_accepted',
        'Delivery failed': 'logs.action_delivery_failed'
    };
    const key = map[raw];
    if (!key) return raw;
    const translated = t(key);
    return translated === key ? raw : translated;
}
function payload(form) {
    const data = {};
    for (const [key, value] of new FormData(form).entries()) data[key] = value;
    return data;
}
let _toastTimer = null;
function setNotice(id, data, kind = '') {
    const text = noticeText(data);
    // Empty = hide inline notice + dismiss toast
    const inline = document.getElementById(id);
    if (inline) {
        if (!inline.dataset.a11yInit) {
            inline.setAttribute('role', 'status');
            inline.setAttribute('aria-live', 'polite');
            inline.dataset.a11yInit = '1';
        }
        inline.classList.toggle('error', kind === 'error');
        inline.classList.toggle('ok', kind === 'ok');
        inline.classList.toggle('pending', kind !== 'error' && kind !== 'ok' && text);
        inline.textContent = text;
    }
    // Non-empty with kind = show fixed toast (for task actions feedback)
    if (text && (kind === 'ok' || kind === 'error')) {
        showToast(text, kind);
    }
}
function showToast(text, kind = 'ok') {
    let toast = qs('#globalToast');
    if (!toast) {
        toast = document.createElement('div');
        toast.id = 'globalToast';
        toast.className = 'global-toast';
        document.body.appendChild(toast);
    }
    toast.textContent = text;
    toast.className = 'global-toast ' + kind;
    toast.classList.add('show');
    if (_toastTimer) clearTimeout(_toastTimer);
    _toastTimer = setTimeout(() => toast.classList.remove('show'), 2500);
}
function noticeText(data) {
    if (typeof data === 'string') return data;
    if (data instanceof Error) return data.message;
    if (data && typeof data === 'object') {
        const rawCode = data.code || data.error?.code || '';
        if (rawCode === 'TASKS_NOT_READY') {
            return t('js.task_not_ready');
        }
        if (rawCode === 'LOGS_NOT_READY') {
            return t('js.logs_not_ready');
        }
        const code = data.code ? `${data.code}: ` : '';
        const message = data.message || data.error?.message || '';
        const details = data.details || data.error?.details;
        if (message) {
            return details ? `${code}${message}\n${JSON.stringify(details, null, 2)}` : `${code}${message}`;
        }
    }
    return JSON.stringify(data, null, 2);
}
function showApp() { document.body.className = 'authed'; }
function showAuth() { document.body.className = 'guest'; }
function isOwnerLoginRequired(error) {
    const code = error?.code || error?.error?.code || '';
    return code === 'OWNER_LOGIN_REQUIRED' || error?.httpStatus === 401 || error?._httpStatus === 401;
}
function validateBotName(name) {
    const value = String(name ?? '').trim();
    if (!value) return t('js.missing_kungfu_id');
    if (value.length < 6) return t('js.kungfu_id_short');
    if (value.length > 32) return t('js.kungfu_id_long');
    if (!/^[a-zA-Z0-9_.-]+$/.test(value)) return t('js.kungfu_id_invalid');
    if (RESERVED_BOT_NAMES.has(value.toLowerCase())) return t('js.kungfu_id_reserved');
    return '';
}
function validatePassword(password, field = 'password') {
    const value = String(password ?? '');
    if (!value) {
        const fieldKey = `js.field_${field}`;
        const label = t(fieldKey);
        return t('js.missing_field', {field: label === fieldKey ? field : label});
    }
    const len = new TextEncoder().encode(value).length;
    if (len < 6) return t('js.password_short');
    if (len > 128) return t('js.password_long');
    if (API_KEY_PATTERN.test(value)) return t('js.password_api_key');
    return '';
}
function validateCredentials(data, withConfirm = false) {
    const nameError = validateBotName(data.name);
    if (nameError) return nameError;
    const passwordError = validatePassword(data.password);
    if (passwordError) return passwordError;
    if (withConfirm) {
        const confirmError = validatePassword(data.confirm_password, 'confirm_password');
        if (confirmError) return confirmError;
        if (data.password !== data.confirm_password) return t('js.confirm_mismatch');
    }
    return '';
}
function escapeHtml(value) {
    return String(value ?? '').replace(/[&<>"']/g, (char) => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;'
    }[char]));
}
