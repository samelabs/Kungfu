// renderPage renders the ACTIVE section through the shared section
// lifecycle (sectionBox). Each section owns exactly one box; its
// loader throws structured ApiErrors; state mapping happens here.
//
// sectionState holders persist across renders so Retry re-runs the
// same loader through loading → final state.
const sectionState = {};

function sectionLoadingKey(section) {
    return {store: 'store.loading', owner_credits: 'credits.loading', tasks: 'js.state_loading', logs: 'js.state_loading'}[section] || 'js.state_loading';
}

async function runSection(section, loader, boxSelector, readyRender, options = {}) {
    const key = boxSelector;
    if (!sectionState[key]) {
        sectionState[key] = sectionBox(boxSelector, () => runSection(section, loader, boxSelector, readyRender, options), {
            onReady: readyRender
        });
    }
    const box = sectionState[key];
    box.render('loading', {loadingKey: sectionLoadingKey(section)});
    try {
        await loader();
    } catch (error) {
        const stateName = mapErrorToSectionState(error);
        const payload = stateName === 'unavailable'
            ? {unavailableKey: options.unavailableKey}
            : {message: noticeText(error)};
        box.render(stateName, payload);
        return {ok: false};
    }
    // Success: empty vs ready is decided by the section's own
    // empty-check — an initial load failure NEVER renders as empty.
    // Success with an empty result: the section chooses its target
    // state via emptyState ('empty' or 'unavailable') — Store/Tasks/
    // Logs 0 rows are empty; Credits packages=[] is unavailable.
    if (options.isEmpty && options.isEmpty()) {
        box.render(options.emptyState || 'empty', {
            emptyKey: options.emptyKey,
            unavailableKey: options.unavailableKey
        });
        return;
    }
    box.render('ready');
    return {ok: true};
}

function renderPage() {
    if (SECTION === 'overview') renderOverview();
    if (SECTION === 'key') renderKey();
    if (SECTION === 'tasks' || SECTION === 'task_new') {
        // /owner/tasks/new is the same Tasks UI; the canonical create
        // modal opens automatically once the section renders.
        const outcome = runSection('tasks', loadTasks, '#taskList', () => renderTasks(), {
            isEmpty: () => !state.tasks.length,
            emptyKey: 'tasks.empty'
        });
        if (SECTION === 'task_new') openTaskModal('create');
        return outcome;
    }
    if (SECTION === 'logs') {
        // The logs table lives inside #logsTableWrap; the shared box
        // drives only the table area — summary/pagination render with
        // the section renderer once ready.
        return runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {
            isEmpty: () => !state.logs.items.length,
            emptyKey: 'js.state_logs_empty'
        });
    }
    if (SECTION === 'store') {
        renderStore();
        return runSection('store', loadStoreProducts, '#storeProducts', () => renderStore(), {
            isEmpty: () => !state.store.products.length,
            emptyKey: 'store.empty'
        });
    }
    if (SECTION === 'owner_credits') {
        renderCredits();
        return (async () => {
            await initCreditsReturnStatus();
            await runSection('owner_credits', loadCreditsPackages, '#creditsPackages', () => renderCredits(), {
                isEmpty: () => !state.credits.packages.length,
                emptyState: 'unavailable',
                unavailableKey: 'credits.unavailable'
            });
            renderCreditsPaymentResult();
        })();
    }
}

// activateSession is called after a successful explicit login /
// registration. Account hydration uses the SAME semantics as
// restoreSession: auth failure → guest; non-auth failure →
// persistent shellError with Retry; the authed shell is revealed
// only after /api/account succeeds. Failures never bubble as a
// toast-only path.
async function activateSession() {
    try {
        await loadAccount();
    } catch (error) {
        if (isOwnerLoginRequired(error)) {
            shellGuest();
            return;
        }
        shellError(t('js.account_load_failed'), activateSession);
        return;
    }
    if (SECTION === 'login' || SECTION === 'register') {
        window.location.href = `/owner?lang=${encodeURIComponent(window.APP_LOCALE || document.body.dataset.locale || 'en')}`;
        return;
    }
    shellAuthed();
    shellClearError();
    await renderPage();
}

// restoreSession — the shell lifecycle. Identity first:
//   OWNER_LOGIN_REQUIRED / 401  → guest
//   session ok                  → authenticated flow
// Unknown session/account failures are NEVER treated as guest: they
// surface as a persistent shell error with Retry.
async function restoreSession() {
    try {
        const sessionJson = await requestJson('/api/owner/session', {method: 'GET'});
        if (!sessionJson.success) {
            if (isOwnerLoginRequired(sessionJson)) {
                shellGuest();
                return;
            }
            throw apiErrorFrom(sessionJson, 'js.owner_session_failed');
        }
        state.name = sessionJson.data.bot_name || '';
    } catch (error) {
        if (isOwnerLoginRequired(error)) {
            shellGuest();
            return;
        }
        shellError(t('js.owner_session_failed'), restoreSession);
        return;
    }

    // Authenticated: /api/account is the authoritative fact source.
    // Balance on first paint comes from HERE — Store / Credits load
    // independently afterwards and must never gate it.
    try {
        const accountJson = await requestJson('/api/account', {method: 'GET'});
        if (!accountJson.success) {
            if (isOwnerLoginRequired(accountJson)) {
                shellGuest();
                return;
            }
            throw apiErrorFrom(accountJson, 'js.account_load_failed');
        }
        state.account = accountJson.data;
        state.name = accountJson.data.bot_name || state.name;
    } catch (error) {
        if (isOwnerLoginRequired(error)) {
            shellGuest();
            return;
        }
        shellError(t('js.account_load_failed'), restoreSession);
        return;
    }

    if (SECTION === 'login' || SECTION === 'register') {
        window.location.href = `/owner?lang=${encodeURIComponent(window.APP_LOCALE || document.body.dataset.locale || 'en')}`;
        return;
    }

    shellAuthed();
    shellClearError();
    await renderPage();
}

function decorateRenderPage() {
    if (SECTION !== 'overview' && SECTION !== 'key') return;
    const originalRenderPage = renderPage;
    renderPage = async function () {
        await originalRenderPage();
        try {
            await loadOwnerKey();
            if (SECTION === 'overview') renderOverview();
            if (SECTION === 'key') renderKey();
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    };
}

function bindOwnerPage() {
    bindAuthHandlers();
    bindTaskHandlers();
    bindLogsHandlers();
    bindStoreHandlers();
    bindCreditsEvents();
}

decorateRenderPage();
bindOwnerPage();
restoreSession();
