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
        // balance is a canonical decimal string — verbatim display.
        const balance = state.account && state.account.balance != null ? String(state.account.balance) : null;
        balanceEl.textContent = balance != null ? balance : '—';
    }

    const wrap = qs('#storeProducts');
    if (wrap) {
        // Loading/empty/unavailable/error states are owned by the
        // shared section lifecycle (sectionBox). The renderer only
        // paints the ready state — a non-empty product list.
        if (state.store.products.length) {
            const balanceStr = state.account && state.account.balance != null ? String(state.account.balance) : null;
            const shortBy = (price) => {
                if (balanceStr == null || !/^[0-9]+$/.test(balanceStr) || !/^[0-9]+$/.test(String(price))) return null;
                const gap = BigInt(String(price)) - BigInt(balanceStr);
                return gap > 0n ? gap.toString() : null;
            };
            wrap.innerHTML = state.store.products.map((p) => { const gap = shortBy(p.credits_price); return `
                <div class="store-product" data-product-code="${escapeHtml(p.code)}">
                    <div class="store-product-body">
                        <div class="store-product-title">${escapeHtml(p.title)}</div>
                        ${p.description ? `<div class="muted store-product-desc">${escapeHtml(p.description)}</div>` : ''}
                        <div class="store-product-price">${escapeHtml(String(p.credits_price) === '1' ? t('store.price_one', {price: p.credits_price}) : t('store.price', {price: p.credits_price}))}</div>
                    </div>
                    <div class="store-product-actions">
                        <button class="btn primary" type="button" data-redeem-code="${escapeHtml(p.code)}" ${gap ? 'disabled' : ''}>${escapeHtml(t('store.redeem'))}</button>
                        ${gap ? `<span class="muted store-shortfall">${escapeHtml(t('store.short_by', {gap}))}</span>` : ''}
                    </div>
                </div>`; }).join('');
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
            <span class="muted">${escapeHtml(r.product_title || '')}</span><span>${escapeHtml(String(r.credits_cost))}</span>
            <span class="muted">${escapeHtml(t('store.status'))}</span><span>${escapeHtml(storeStatusText(r.status))}</span>
        </div>`;
}
