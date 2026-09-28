// Rewards section rendering: products, balance (server fact via
// state.account), and the redemption result of the last redeem action.
// Status values are displayed with a pure presentation translation — the
// underlying backend status fact is always shown.

function rewardsStatusText(status) {
    const known = ['pending_review', 'approved', 'rejected', 'fulfilled', 'cancelled'];
    if (!known.includes(status)) return String(status || '');
    return t('rewards.' + status);
}

function renderRewards() {
    const balanceEl = qs('#rewardsBalance');
    if (balanceEl) {
        // balance is a canonical decimal string — verbatim display.
        const balance = state.account && state.account.balance != null ? String(state.account.balance) : null;
        balanceEl.textContent = balance != null ? balance : '—';
    }

    const wrap = qs('#rewardsProducts');
    if (wrap) {
        // Loading/empty/unavailable/error states are owned by the
        // shared section lifecycle (sectionBox). The renderer only
        // paints the ready state — a non-empty product list.
        if (state.rewards.products.length) {
            const balanceStr = state.account && state.account.balance != null ? String(state.account.balance) : null;
            const shortBy = (price) => {
                if (balanceStr == null || !/^[0-9]+$/.test(balanceStr) || !/^[0-9]+$/.test(String(price))) return null;
                const gap = BigInt(String(price)) - BigInt(balanceStr);
                return gap > 0n ? gap.toString() : null;
            };
            wrap.innerHTML = state.rewards.products.map((p) => { const gap = shortBy(p.credits_price); return `
                <div class="rewards-product" data-product-code="${escapeHtml(p.code)}">
                    <div class="rewards-product-body">
                        <div class="rewards-product-title">${escapeHtml(p.title)}</div>
                        ${p.description ? `<div class="muted rewards-product-desc">${escapeHtml(p.description)}</div>` : ''}
                        <div class="rewards-product-price">${escapeHtml(String(p.credits_price) === '1' ? t('rewards.price_one', {price: p.credits_price}) : t('rewards.price', {price: p.credits_price}))}</div>
                    </div>
                    <div class="rewards-product-actions">
                        <button class="btn primary" type="button" data-redeem-code="${escapeHtml(p.code)}" ${gap ? 'disabled' : ''}>${escapeHtml(t('rewards.redeem'))}</button>
                        ${gap ? `<span class="muted rewards-shortfall">${escapeHtml(t('rewards.short_by', {gap}))}</span>` : ''}
                    </div>
                </div>`; }).join('');
        }
    }

    renderRedemptionResult();

    // A1: the own-redemption history loads once per page visit (and
    // after a redeem); its pager and expansion live in rewards.js.
    if (state.rewards.history && !state.rewards.history.loaded && qs('#rewardsHistory')) {
        loadRewardsHistory(1);
    }
}

// renderRewardsHistory paints the paged own-redemption list (A1).
function renderRewardsHistory() {
    const box = qs('#rewardsHistory');
    if (!box) return;
    const h = state.rewards.history;
    if (!h.items.length) {
        box.innerHTML = `<p class="muted">${escapeHtml(t('rewards.history_empty'))}</p>`;
    } else {
        box.innerHTML = h.items.map((r) => `
            <div class="task-item" data-rh-code="${escapeHtml(r.code)}">
                <div class="task-facts">
                    <span class="badge">${escapeHtml(rewardsStatusText(r.status))}</span>
                    <span>${escapeHtml(r.product_title || r.code)}</span>
                    <span>${escapeHtml(String(r.credits_cost))}</span>
                    <span class="muted">${escapeHtml(tcvFmtDate(r.created_at))}</span>
                </div>
                <div class="rh-detail detail-box" hidden></div>
            </div>`).join('');
    }
    const info = qs('#rewardsHistoryPageInfo');
    if (info) info.textContent = t('rewards.page_info', {page: h.page, pages: h.pages, total: h.total});
    const prev = qs('#rewardsHistoryPrev');
    const next = qs('#rewardsHistoryNext');
    if (prev) prev.disabled = h.page <= 1;
    if (next) next.disabled = h.page >= h.pages;
}

function renderRedemptionResult() {
    const box = qs('#rewardsResult');
    if (!box) return;
    const r = state.rewards.lastRedemption;
    if (!r) {
        box.hidden = true;
        return;
    }
    const createdFlag = r.created === true ? t('rewards.result_created')
        : r.created === false ? t('rewards.result_replay') : '';
    box.hidden = false;
    box.innerHTML = `
        <h3>${escapeHtml(t('rewards.result_heading'))}</h3>
        ${createdFlag ? `<p class="muted">${escapeHtml(createdFlag)}</p>` : ''}
        <div class="rewards-result-grid">
            <span class="muted">${escapeHtml(t('rewards.code'))}</span><span class="mono">${escapeHtml(r.code || '')}</span>
            <span class="muted">${escapeHtml(r.product_title || '')}</span><span>${escapeHtml(String(r.credits_cost))}</span>
            <span class="muted">${escapeHtml(t('rewards.status'))}</span><span>${escapeHtml(rewardsStatusText(r.status))}</span>
        </div>`;
}
