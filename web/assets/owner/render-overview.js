function renderOverview() {
    const account = state.account || {};
    const stats = account.stats || {};
    qs('#ownerName').textContent = `@${account.bot_name || state.name}`;
    qs('#ownerMeta').textContent = t('js.status_line', {status: humanStatus(account.status || 'active')});
    qs('#statsGrid').innerHTML = `
        <div class="stat"><b>${formatCredits(account.balance)}</b><span>${escapeHtml(t('overview.balance'))}</span></div>
        <div class="stat"><b>${stats.kungfu_count ?? 0}</b><span>${escapeHtml(t('overview.kungfu'))}</span></div>
        <div class="stat"><b>${stats.public_kungfu_count ?? 0}</b><span>${escapeHtml(t('overview.public'))}</span></div>
        <div class="stat"><b>${stats.platform_task_count ?? 0}</b><span>${escapeHtml(t('overview.tasks'))}</span></div>
    `;
    const keyBox = qs('#keyBox');
    if (keyBox) {
        keyBox.textContent = state.keyMasked || t('js.owner_key_hidden');
        keyBox.classList.toggle('is-empty', !state.keyMasked);
    }
    // passive overview state — no feedback toast
}

// renderKey shows MASKED metadata. The one-time raw disclosure
// (state.newKeyOnce, from registration/reset response only) is never
// rendered here — after navigation only masked metadata remains.
function renderKey() {
    const keyBox = qs('#keyBox');
    if (keyBox) {
        keyBox.textContent = state.keyMasked || t('js.owner_key_hidden');
        keyBox.classList.toggle('is-empty', !state.keyMasked);
    }
    const newKeyBox = qs('#newKeyBox');
    if (newKeyBox) {
        newKeyBox.textContent = state.newKeyOnce || '';
        newKeyBox.classList.toggle('is-empty', !state.newKeyOnce);
        const copyBtn = qs('#copyNewKeyBtn');
        if (copyBtn) copyBtn.style.display = state.newKeyOnce ? '' : 'none';
    }
}
