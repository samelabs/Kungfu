// renderPage renders the ACTIVE section through the shared section
// lifecycle (sectionBox). Each section owns exactly one box; its
// loader throws structured ApiErrors; state mapping happens here.
//
// sectionState holders persist across renders so Retry re-runs the
// same loader through loading → final state.
const sectionState = {};

function sectionLoadingKey(section) {
    return {rewards: 'rewards.loading', owner_credits: 'credits.loading', logs: 'js.state_loading'}[section] || 'js.state_loading';
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
    // state via emptyState ('empty' or 'unavailable') — Rewards/Tasks/
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
    if (SECTION === 'overview') return runTurnView();
    if (SECTION === 'key') renderKey();
    if (SECTION === 'threads') return runThreadsView();
    if (SECTION === 'thread_detail') return runThreadDetailView();
    if (SECTION === 'memories') return runMemoriesView();
    if (SECTION === 'memory_detail') return runMemoryDetailView();
    if (SECTION === 'logs') {
        // The logs table lives inside #logsTableWrap; the shared box
        // drives only the table area — summary/pagination render with
        // the section renderer once ready.
        return runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {
            isEmpty: () => !state.logs.items.length,
            emptyKey: 'js.state_logs_empty'
        });
    }
    if (SECTION === 'rewards') {
        renderRewards();
        return runSection('rewards', loadRewardsProducts, '#rewardsProducts', () => renderRewards(), {
            isEmpty: () => !state.rewards.products.length,
            emptyKey: 'rewards.empty'
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
    // Session and account are independent cookie-authenticated GETs:
    // account is FIRED before the session await so the boot chain costs
    // one round trip, not two. The session result still decides guest /
    // failure first (its branches are unchanged); the account result is
    // consumed only where it always was. The no-op catch keeps an
    // account rejection from becoming an unhandled rejection on the
    // paths that return before awaiting it — the awaited promise below
    // still throws.
    let accountP = null;
    try {
        accountP = requestJson('/api/account', {method: 'GET'});
        accountP.catch(() => {});
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
    // Balance on first paint comes from HERE — Rewards / Credits load
    // independently afterwards and must never gate it.
    try {
        const accountJson = await accountP;
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
    if (SECTION !== 'key') return;
    const originalRenderPage = renderPage;
    renderPage = async function () {
        await originalRenderPage();
        try {
            await loadOwnerKey();
            if (SECTION === 'key') renderKey();
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    };
}

function bindOwnerPage() {
    bindAuthHandlers();
    bindLogsHandlers();
    bindRewardsHandlers();
    bindCreditsEvents();
}

// The task console (tasks-console.js, loaded earlier) installs its
// renderPage wrap here, deterministically, before this file's own
// decoration chains onto it.
if (typeof tcvDecorateRenderPage === 'function') tcvDecorateRenderPage();
decorateRenderPage();
bindOwnerPage();
restoreSession();
