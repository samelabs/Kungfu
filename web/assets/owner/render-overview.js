function renderOverview() {
    const account = state.account || {};
    const stats = account.stats || {};
    qs('#ownerName').textContent = `@${account.bot_name || state.name}`;
    qs('#ownerMeta').textContent = t('js.status_line', {status: humanStatus(account.status || 'active')});
    qs('#statsGrid').innerHTML = `
        <div class="stat"><b>${Number(account.balance || 0).toFixed(2)}</b><span>${escapeHtml(t('overview.balance'))}</span></div>
        <div class="stat"><b>${stats.kungfu_count ?? 0}</b><span>${escapeHtml(t('overview.kungfu'))}</span></div>
        <div class="stat"><b>${stats.public_kungfu_count ?? 0}</b><span>${escapeHtml(t('overview.public'))}</span></div>
        <div class="stat"><b>${stats.platform_task_count ?? 0}</b><span>${escapeHtml(t('overview.tasks'))}</span></div>
    `;
    const keyBox = qs('#keyBox');
    if (keyBox) {
        keyBox.textContent = state.ownerKey || t('js.owner_key_hidden');
        keyBox.classList.toggle('is-empty', !state.ownerKey);
    }
    setNotice('overviewNotice', t('overview.notice'), 'ok');
}

function renderKey() {
    const keyBox = qs('#keyBox');
    if (keyBox) keyBox.textContent = state.ownerKey;
}
