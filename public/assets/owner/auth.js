function bindLoginForm() {
    if (!qs('#loginForm')) return;
    qs('#loginForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data);
        if (error) return setNotice('loginNotice', error, 'error');
        setNotice('loginNotice', t('auth.working'));
        try {
            const json = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('loginNotice', json.error || json, 'error');
            await activateSession();
        } catch (error) {
            setNotice('loginNotice', String(error), 'error');
        }
    });
}

function bindRegisterForm() {
    if (!qs('#registerForm')) return;
    qs('#registerForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data, true);
        if (error) return setNotice('registerNotice', error, 'error');
        setNotice('registerNotice', t('auth.working'));
        try {
            const json = await requestJson('/api/register', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return setNotice('registerNotice', json.error || json, 'error');
            const sessionJson = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify({name: data.name, password: data.password})});
            if (!sessionJson.success) return setNotice('registerNotice', sessionJson.error || sessionJson, 'error');
            await activateSession();
        } catch (error) {
            setNotice('registerNotice', String(error), 'error');
        }
    });
}

function bindPasswordForm() {
    if (!qs('#passwordForm')) return;
    qs('#passwordForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validatePassword(data.password) || validatePassword(data.new_password, 'new_password');
        if (error) return setNotice('passwordNotice', error, 'error');
        if (data.password === data.new_password) return setNotice('passwordNotice', t('auth.new_password_diff'), 'error');
        setNotice('passwordNotice', t('auth.working'));
        try {
            const json = await requestJson('/api/change-password', {
                method: 'POST',
                body: JSON.stringify({password: data.password, new_password: data.new_password})
            });
            if (!json.success) return setNotice('passwordNotice', json.error || json, 'error');
            event.currentTarget.reset();
            setNotice('passwordNotice', t('auth.password_changed'), 'ok');
        } catch (error) {
            setNotice('passwordNotice', String(error), 'error');
        }
    });
}

function bindResetKey() {
    if (!qs('#resetKeyBtn')) return;
    qs('#resetKeyBtn').addEventListener('click', async () => {
        setNotice('resetNotice', t('auth.resetting_key'));
        try {
            if (!state.ownerKey) {
                await loadOwnerKey();
            }
            const json = await requestJson('/api/reset-key', {
                method: 'POST',
                body: JSON.stringify({current_key: state.ownerKey})
            });
            if (!json.success) return setNotice('resetNotice', json.error || json, 'error');
            state.ownerKey = json.data.new_key;
            renderKey();
            setNotice('resetNotice', t('auth.key_reset'), 'ok');
        } catch (error) {
            setNotice('resetNotice', String(error), 'error');
        }
    });
}

function bindCopyKey() {
    if (!qs('#copyKeyBtn')) return;
    qs('#copyKeyBtn').addEventListener('click', async () => {
        const noticeId = SECTION === 'key' ? 'resetNotice' : 'overviewNotice';
        if (!state.ownerKey) {
            try {
                await loadOwnerKey();
                if (SECTION === 'overview' || SECTION === 'key') {
                    if (SECTION === 'overview') renderOverview();
                    if (SECTION === 'key') renderKey();
                }
            } catch (error) {
                return setNotice(noticeId, String(error), 'error');
            }
        }
        await navigator.clipboard.writeText(state.ownerKey);
        setNotice(noticeId, t('auth.key_copied'), 'ok');
    });
}

function bindReload() {
    if (!qs('#reloadBtn')) return;
    qs('#reloadBtn').addEventListener('click', async () => {
        try {
            await loadAccount();
            renderOverview();
            setNotice('overviewNotice', t('auth.reloaded'), 'ok');
        } catch (error) {
            setNotice('overviewNotice', String(error), 'error');
        }
    });
}

function bindLogout() {
    if (!qs('#logoutBtn')) return;
    qs('#logoutBtn').addEventListener('click', async () => {
        try {
            await requestJson('/api/owner/session', {method: 'DELETE'});
        } catch (error) {
        }
        state.name = '';
        state.ownerKey = '';
        state.account = null;
        state.tasks = [];
        showAuth();
    });
}

function bindAuthHandlers() {
    bindLoginForm();
    bindRegisterForm();
    bindPasswordForm();
    bindResetKey();
    bindCopyKey();
    bindReload();
    bindLogout();
}
