function bindLoginForm() {
    if (!qs('#loginForm')) return;
    qs('#loginForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data);
        if (error) return showToast(noticeText(error), 'error');
        try {
            const json = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify(data)});
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            await activateSession();
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

// buildRegisterNextSteps (WO-19 R2): the post-registration card —
// ① the key filled into a copyable MCP config plus a curl example,
// ② the llms.txt pointer, ③ publish or earn. The snippets carry the
// JUST-ISSUED one-time key (state.newKeyOnce) as plain textContent;
// copy buttons reuse the key-copy mechanism (clipboard + toast).
function buildRegisterNextSteps() {
    const mcpSnippet = JSON.stringify({
        mcpServers: {
            kungfu: {
                url: 'https://kungfu.md/mcp',
                headers: {Authorization: `Bearer ${state.newKeyOnce}`}
            }
        }
    });
    const curlSnippet = `curl -s https://kungfu.md/api/v1/work_list -H 'Content-Type: application/json' -H "Authorization: Bearer ${state.newKeyOnce}" -d '{}'`;
    const card = document.createElement('section');
    card.className = 'next-steps';
    card.id = 'registerNextSteps';
    const h3 = document.createElement('h3');
    h3.textContent = t('auth.next_heading');
    card.appendChild(h3);
    const steps = document.createElement('ol');
    steps.className = 'start-steps';
    steps.innerHTML = `
        <li><b>${escapeHtml(t('auth.next_key_title'))}</b><span>${escapeHtml(t('auth.next_key_body'))}</span>
            <div class="next-snippets">
                <span class="muted">${escapeHtml(t('auth.next_mcp_label'))}</span>
                <div class="keybox mono" id="mcpConfigBox"></div>
                <button type="button" class="btn" id="copyMcpBtn">${escapeHtml(t('auth.next_copy_mcp'))}</button>
                <span class="muted">${escapeHtml(t('auth.next_curl_label'))}</span>
                <div class="keybox mono" id="curlExampleBox"></div>
                <button type="button" class="btn" id="copyCurlBtn">${escapeHtml(t('auth.next_copy_curl'))}</button>
            </div>
        </li>
        <li><b>${escapeHtml(t('auth.next_llms_title'))}</b><span>${escapeHtml(t('auth.next_llms_body'))}</span></li>
        <li><b>${escapeHtml(t('auth.next_go_title'))}</b><span>${escapeHtml(t('auth.next_go_body'))}</span></li>`;
    card.appendChild(steps);
    const mcpBox = steps.querySelector('#mcpConfigBox');
    const curlBox = steps.querySelector('#curlExampleBox');
    if (mcpBox) mcpBox.textContent = mcpSnippet;
    if (curlBox) curlBox.textContent = curlSnippet;
    const bindCopy = (id, text) => {
        const btn = steps.querySelector(id);
        if (!btn) return;
        btn.addEventListener('click', async () => {
            await navigator.clipboard.writeText(text);
            showToast(noticeText(t('auth.key_copied')), 'ok');
        });
    };
    bindCopy('#copyMcpBtn', mcpSnippet);
    bindCopy('#copyCurlBtn', curlSnippet);
    return card;
}

function bindRegisterForm() {
    if (!qs('#registerForm')) return;
    qs('#registerForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        const data = payload(event.currentTarget);
        const error = validateCredentials(data, true);
        if (error) return showToast(noticeText(error), 'error');
        try {
            const json = await requestJson('/api/owner/register', {method: 'POST', body: JSON.stringify(data)});
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
                // The account exists now: retire the form so only the
                // one-time key, the next steps and the way forward
                // remain.
                form.hidden = true;
                const heading = form.parentElement && form.parentElement.querySelector('h2');
                if (heading) heading.textContent = t('auth.registered_heading');
                warn.className = 'key-warning';
                keyLine.classList.add('key-reveal');
                form.after(warn);
                warn.after(keyLine);
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
                // Next steps (WO-19 R2): the key filled into a
                // copyable MCP config and a curl example, the docs
                // pointer, and the publish/earn split. Same clipboard
                // mechanism as the key copy; no inline handlers.
                copyBtn.after(continueBtn);
                copyBtn.after(buildRegisterNextSteps());
            }
            continueBtn.addEventListener('click', async () => {
                // The user explicitly continues; the session is created
                // here (not before disclosure) and the raw key is gone.
                state.newKeyOnce = '';
                const sessionJson = await requestJson('/api/owner/session', {method: 'POST', body: JSON.stringify({name: data.name, password: data.password})});
                if (!sessionJson.success) return showToast(noticeText(sessionJson.error || sessionJson), 'error');
                await activateSession();
            });
            continueBtn.focus();
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

function bindPasswordForm() {
    if (!qs('#passwordForm')) return;
    qs('#passwordForm').addEventListener('submit', async (event) => {
        event.preventDefault();
        // capture before the first await: event.currentTarget is null
        // by the time the request settles
        const form = event.currentTarget;
        const data = payload(form);
        const error = validatePassword(data.password) || validatePassword(data.new_password, 'new_password');
        if (error) return showToast(noticeText(error), 'error');
        if (data.password === data.new_password) return showToast(noticeText(t('auth.new_password_diff')), 'error');
        try {
            const json = await requestJson('/api/change-password', {
                method: 'POST',
                body: JSON.stringify({password: data.password, new_password: data.new_password})
            });
            if (!json.success) return showToast(noticeText(json.error || json), 'error');
            form.reset();
            showToast(noticeText(t('auth.password_changed')), 'ok');
        } catch (error) {
            showToast(noticeText(String(error)), 'error');
        }
    });
}

// Reset needs only the signed-in OWNER session — the current raw key
// is never demanded (it is unrecoverable from the server, so an owner
// who lost it must still be able to reset). A confirm() guards the
// click; on success the NEW key is shown once (state.newKeyOnce) and
// only that new key is copyable.
function bindResetKey() {
    const form = qs('#resetKeyForm');
    if (!form) return;
    form.addEventListener('submit', async (event) => {
        event.preventDefault();
        if (!confirm(t('key.reset_confirm'))) return;
        try {
            const json = await requestJson('/api/reset-key', {
                method: 'POST',
                body: '{}'
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
        showAuth();
    });
}

function bindAuthHandlers() {
    bindLoginForm();
    bindRegisterForm();
    bindPasswordForm();
    bindResetKey();
    bindCopyNewKey();
    bindLogout();
}
