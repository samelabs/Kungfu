function bindLoginForm() {
    if (!qs('#loginForm')) return;
    qs('#loginForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data);
        if (error) return showToast(noticeText(error), 'error');
        // transitional: silent
        try {
            const json = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            await activateSession();
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

function bindRegisterForm() {
    if (!qs('#registerForm')) return;
    qs('#registerForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data, true);
        if (error) return showToast(noticeText(error), 'error');
        // transitional: silent
        try {
            const json = await requestJson('/api/register', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            // ONE-TIME disclosure: display the raw key from the
            // registration response on THIS page. No /api/key
            // recovery flow, no storage. Navigation destroys it.
            state.newKeyOnce = json.data.key || '';
            const box = qs('#newKeyBox');
            const continueBtn = document.createElement('button');
            continueBtn.type = 'button';
            continueBtn.className = 'btn primary';
            continueBtn.id = 'continueAfterKeyBtn';
            continueBtn.textContent = t('auth.continue_to_owner');
            const keyLine = document.createElement('div');
            keyLine.className = 'keybox';
            keyLine.id = 'registerKeyBox';
            keyLine.textContent = state.newKeyOnce;
            const warn = document.createElement('p');
            warn.textContent = t('auth.key_one_time_warning');
            const form = qs('#registerForm');
            if (form) {
                form.after(warn);
                warn.after(keyLine);
                keyLine.after(continueBtn);
                const copyBtn = document.createElement('button');
                copyBtn.type = 'button';
                copyBtn.className = 'btn';
                copyBtn.id = 'copyNewKeyBtn';
                copyBtn.textContent = t('key.copy_new');
                copyBtn.addEventListener('click', async () => {
                    await navigator.clipboard.writeText(state.newKeyOnce);
                    showToast(noticeText(t('auth.key_copied')), 'ok');
                });
                keyLine.after(copyBtn);
            }
            continueBtn.addEventListener('click', async () => {
                // The user explicitly continues; the session is created
                // here (not before disclosure) and the raw key is gone.
                state.newKeyOnce = '';
                const sessionJson = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify({name: data.name, password: data.password})});
                if (!sessionJson.success) return showToast(noticeText(sessionJson.error || sessionJson), 'error');
                await activateSession();
            });
            showToast(noticeText(t('auth.registered_key_below')), 'ok');
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

function bindPasswordForm() {
    if (!qs('#passwordForm')) return;
    qs('#passwordForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validatePassword(data.password) || validatePassword(data.new_password, 'new_password');
        if (error) return showToast(noticeText(error), 'error');
        if (data.password === data.new_password) return showToast(noticeText(t('auth.new_password_diff')), 'error');
        // transitional: silent
        try {
            const json = await requestJson('/api/change-password', {
                method: 'POST',
                body: JSON.stringify({password: data.password, new_password: data.new_password})
            });
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            event.currentTarget.reset();
            showToast(noticeText(t('auth.password_changed')), 'ok');
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

// Reset requires the user to MANUALLY supply the current raw key —
// the stored key is not recoverable from the server and is never
// auto-loaded. On success the NEW key is shown once (state.newKeyOnce)
// and only that new key is copyable.
function bindResetKey() {
    const form = qs('#resetKeyForm');
    if (!form) return;
    form.addEventListener('submit', async (event) => {
        event.preventDefault();
        const currentKey = (new FormData(form).get('current_key') || '').trim();
        if (!currentKey) return showToast(noticeText(t('auth.reset_key_required')), 'error');
        // transitional: silent
        try {
            const json = await requestJson('/api/reset-key', {
                method: 'POST',
                body: JSON.stringify({current_key: currentKey})
            });
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            state.newKeyOnce = json.data.new_key || '';
            form.reset();
            renderKey();
            showToast(noticeText(t('auth.key_reset')), 'ok');
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

// Copy applies ONLY to the one-time newly issued key. There is no
// copy-current-key action anywhere.
function bindCopyNewKey() {
    const btn = qs('#copyNewKeyBtn');
    if (!btn) return;
    btn.addEventListener('click', async () => {
        if (!state.newKeyOnce) return;
        await navigator.clipboard.writeText(state.newKeyOnce);
        showToast(noticeText(t('auth.key_copied')), 'ok');
    });
}

function bindReload() {
    if (!qs('#reloadBtn')) return;
    qs('#reloadBtn').addEventListener('click', async () => {
        try {
            await loadAccount();
            renderOverview();
            showToast(noticeText(t('auth.reloaded')), 'ok');
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
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
        state.keyMasked = '';
    state.newKeyOnce = '';
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
    bindCopyNewKey();
    bindReload();
    bindLogout();
}
