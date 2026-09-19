// Store section rendering: products, balance (server fact via
// state.account), and the redemption result of the last redeem action.
// Status values are displayed with a pure presentation translation — the
// underlying backend status fact is always shown.

function storeStatusText(status) {
    const known = ['pending_review', 'approved', 'rejected', 'fulfilled', 'cancelled'];
    if (!known.includes(status)) return String(status || '');
    return t('store.' + status);
}

function renderStore() {
    const balanceEl = qs('#storeBalance');
    if (balanceEl) {
        const balance = state.account && state.account.balance != null ? Number(state.account.balance) : null;
        balanceEl.textContent = balance != null ? balance.toFixed(4) : '—';
    }

    const wrap = qs('#storeProducts');
    if (wrap) {
        if (!state.store.products.length) {
            wrap.innerHTML = '<div class="muted">' + escapeHtml(t('store.empty')) + '</div>';
        } else {
            wrap.innerHTML = state.store.products.map((p) => `
                <div class="store-product" data-product-code="${escapeHtml(p.code)}">
                    <div class="store-product-body">
                        <div class="store-product-title">${escapeHtml(p.title)}</div>
                        ${p.description ? `<div class="muted store-product-desc">${escapeHtml(p.description)}</div>` : ''}
                        <div class="store-product-price">${escapeHtml(t('store.price', {price: Number(p.credits_price)}))}</div>
                    </div>
                    <button class="btn primary" type="button" data-redeem-code="${escapeHtml(p.code)}">${escapeHtml(t('store.redeem'))}</button>
                </div>`).join('');
        }
    }

    renderRedemptionResult();
}

function renderRedemptionResult() {
    const box = qs('#storeResult');
    if (!box) return;
    const r = state.store.lastRedemption;
    if (!r) {
        box.hidden = true;
        return;
    }
    const createdFlag = r.created === true ? t('store.result_created')
        : r.created === false ? t('store.result_replay') : '';
    box.hidden = false;
    box.innerHTML = `
        <h3>${escapeHtml(t('store.result_heading'))}</h3>
        ${createdFlag ? `<p class="muted">${escapeHtml(createdFlag)}</p>` : ''}
        <div class="store-result-grid">
            <span class="muted">${escapeHtml(t('store.code'))}</span><span class="mono">${escapeHtml(r.code || '')}</span>
            <span class="muted">${escapeHtml(r.product_title || '')}</span><span>${escapeHtml(String(Number(r.credits_cost)))}</span>
            <span class="muted">${escapeHtml(t('store.status'))}</span><span>${escapeHtml(storeStatusText(r.status))}</span>
        </div>`;
}
