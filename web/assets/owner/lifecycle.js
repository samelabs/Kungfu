// lifecycle.js — shared Owner shell + section lifecycle.
//
// Responsibilities (strict):
//   transport  → api.js requestJson (HTTP/JSON, structured errors)
//   loader     → server facts into state
//   lifecycle  → THIS FILE: state machine + rendering orchestration
//   renderer   → render-*.js pure display
//   action     → */handlers: user actions
//
// Shell lifecycle:
//   /api/owner/session OWNER_LOGIN_REQUIRED/401 → guest (login page)
//   session ok → authenticated flow → /api/account MUST succeed first;
//   a non-auth failure there (or in hydration) is a PERSISTENT page
//   error with Retry — never booting forever, never a 2.5s toast,
//   never masquerading as guest.
//
// Section lifecycle: one shared machine —
//   loading | ready | empty | unavailable | error — mapped from the
//   structured API error code, never from message strings.

// ── Structured API errors ─────────────────────────────────────
// apiError carries the server error code + HTTP status through to the
// lifecycle layer. Created in api.js loaders (apiErrorFrom), so the
// UI can branch on code — no string parsing.
function apiError(code, message, httpStatus, details) {
    const e = new Error(message || code || 'ERROR');
    e.name = 'ApiError';
    e.code = code || '';
    e.httpStatus = httpStatus || 0;
    e.details = details || null;
    return e;
}

function isApiError(e) {
    return !!e && e.name === 'ApiError' && typeof e.code === 'string';
}

// mapErrorToSectionState maps a failed section load to a section
// state. Known service-configuration / precondition codes are
// "unavailable" (not a failure); everything else (network, 5xx,
// unexpected) is "error".
const UNAVAILABLE_CODES = new Set([
    'PAYMENT_NOT_CONFIGURED',
    'TASKS_NOT_READY',
    'LOGS_NOT_READY'
]);

function mapErrorToSectionState(error) {
    if (isApiError(error) && UNAVAILABLE_CODES.has(error.code)) return 'unavailable';
    return 'error';
}

// ── Shell lifecycle ───────────────────────────────────────────
const shellState = {
    phase: 'booting' // booting → guest | authed | error
};

function shellGuest() {
    shellState.phase = 'guest';
    document.body.className = 'guest';
}

function shellAuthed() {
    shellState.phase = 'authed';
    document.body.className = 'authed';
}

// shellError renders the persistent, page-level hydration error with
// Retry. The shell content area keeps showing it until a retry
// succeeds — it never falls back to guest and never resolves by
// itself.
function shellError(message, retry) {
    shellState.phase = 'error';
    document.body.className = 'authed boot-failed';
    const host = qs('#shellStatus');
    if (host) {
        host.innerHTML = `
            <div class="section-state state-error" role="alert" aria-live="assertive">
                <div class="section-state-icon" aria-hidden="true">⚠</div>
                <p class="section-state-text">${escapeHtml(message)}</p>
                <button class="btn section-state-retry" type="button">${escapeHtml(t('js.state_retry'))}</button>
            </div>`;
        const btn = host.querySelector('.section-state-retry');
        if (btn) btn.addEventListener('click', () => { retry(); });
    }
}

function shellClearError() {
    const host = qs('#shellStatus');
    if (host) host.innerHTML = '';
}

// ── Section lifecycle ─────────────────────────────────────────
// sectionBox is the per-section state machine. Sections do NOT
// hand-roll loading/error HTML anymore; they call:
//   const box = sectionBox('#storeProducts', reload);
//   box.render('loading' | 'ready' | 'empty' | 'unavailable' | 'error');
// 'ready' hands rendering back to the section's own renderer via
// box.onReady.
function sectionBox(selector, retryLoader, options = {}) {
    const host = qs(selector);
    if (!host) return null;
    const box = {
        state: 'loading',
        onReady: options.onReady || null,
        render(next, payload) {
            this.state = next;
            if (next === 'ready') {
                if (this.onReady) this.onReady(host);
                return;
            }
            const view = sectionStateView(next, payload);
            host.innerHTML = view;
            const retryBtn = host.querySelector('.section-state-retry');
            if (retryBtn) {
                retryBtn.addEventListener('click', () => {
                    // Retry re-executes THIS section loader and passes
                    // through loading → final state again.
                    this.render('loading');
                    Promise.resolve()
                        .then(() => retryLoader())
                        .catch(() => { /* loader already re-rendered error state */ });
                });
            }
        }
    };
    box.render('loading');
    return box;
}

// sectionStateView builds the shared state markup. Retry exists only
// on error. Visual classes distinguish error (alert semantics) from
// unavailable (informational) from empty (plain muted).
function sectionStateView(state, payload) {
    if (state === 'loading') {
        return `<div class="section-state state-loading" role="status" aria-live="polite">
            <span class="section-state-spinner" aria-hidden="true"></span>
            <p class="section-state-text">${escapeHtml(t(payload?.loadingKey || 'js.state_loading'))}</p>
        </div>`;
    }
    if (state === 'empty') {
        return `<div class="section-state state-empty" role="status">
            <p class="section-state-text">${escapeHtml(t(payload?.emptyKey || 'js.state_empty'))}</p>
        </div>`;
    }
    if (state === 'unavailable') {
        return `<div class="section-state state-unavailable" role="status">
            <div class="section-state-icon" aria-hidden="true">◌</div>
            <p class="section-state-text">${escapeHtml(t(payload?.unavailableKey || 'js.state_unavailable'))}</p>
        </div>`;
    }
    // error
    return `<div class="section-state state-error" role="alert" aria-live="assertive">
        <div class="section-state-icon" aria-hidden="true">⚠</div>
        <p class="section-state-text">${escapeHtml(payload?.message || t('js.state_error'))}</p>
        <button class="btn section-state-retry" type="button">${escapeHtml(t('js.state_retry'))}</button>
    </div>`;
}
