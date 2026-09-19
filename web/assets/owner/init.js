async function renderPage() {
    if (SECTION === 'overview') renderOverview();
    if (SECTION === 'key') renderKey();
    if (SECTION === 'tasks') {
        try {
            await loadTasks();
            renderTasks();
        } catch (error) {
            renderTasks();
            showToast(noticeText(String(error)), 'error')
        }
    }
    if (SECTION === 'logs') {
        try {
            await loadLogs();
            renderLogs();
            // page-load facts produce no feedback (silent)
        } catch (error) {
            renderLogs();
            showToast(noticeText(String(error)), 'error')
        }
    }
    if (SECTION === 'store') {
        renderStore();
        try {
            await loadStoreProducts();
        } catch (error) {
            showToast(noticeText(error), 'error');
        }
        renderStore();
    }
    if (SECTION === 'owner_credits') {
        renderCredits();
        await initCreditsReturnStatus();
        try {
            await loadCreditsPackages();
        } catch (error) {
            showToast(noticeText(String(error)), 'error')
        }
        renderCredits();
        renderCreditsPaymentResult();
    }
}

async function activateSession() {
    await loadAccount();
    if (SECTION === 'login' || SECTION === 'register') {
        window.location.href = `/owner?lang=${encodeURIComponent(window.APP_LOCALE || document.body.dataset.locale || 'en')}`;
        return;
    }
    showApp();
    await renderPage();
}

async function restoreSession() {
    try {
        const sessionJson = await requestJson('/api/owner/session', {method: 'GET'});
        if (!sessionJson.success) {
            if (isOwnerLoginRequired(sessionJson)) {
                showAuth();
                return;
            }
            throw new Error(noticeText(sessionJson.error || t('js.owner_session_failed')));
        }
        state.name = sessionJson.data.bot_name || '';
        const accountJson = await requestJson('/api/account', {method: 'GET'});
        if (!accountJson.success) {
            if (isOwnerLoginRequired(accountJson)) {
                showAuth();
                return;
            }
            throw new Error(noticeText(accountJson.error || t('js.account_load_failed')));
        }
        state.account = accountJson.data;
        state.name = accountJson.data.bot_name || state.name;
        if (SECTION === 'login' || SECTION === 'register') {
            window.location.href = `/owner?lang=${encodeURIComponent(window.APP_LOCALE || document.body.dataset.locale || 'en')}`;
            return;
        }
        showApp();
    } catch (error) {
        if (isOwnerLoginRequired(error)) {
            showAuth();
            return;
        }
        showApp();
        showToast(noticeText(error), 'error');
        return;
    }

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
