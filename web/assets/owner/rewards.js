async function loadRewardsProducts() {
    const json = await requestJson('/api/owner/rewards/products', {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'rewards.load_failed');
    state.rewards.products = json.data.products || [];
}

// ---- A1: the owner's own redemption history (paged, expandable) ----

// loadRewardsHistory fetches one page of the session bot's redemptions.
// Failures render in place — the products list above is unaffected.
function loadRewardsHistory(page) {
    state.rewards.history.page = Math.max(1, page || 1);
    const params = new URLSearchParams({page: String(state.rewards.history.page), page_size: '10'});
    return requestJson(`/api/owner/rewards/redemptions?${params.toString()}`, {method: 'GET'})
        .then((json) => {
            if (!json.success) throw apiErrorFrom(json, 'rewards.load_failed');
            const h = state.rewards.history;
            h.items = json.data.redemptions || [];
            const pg = json.data.pagination || {};
            h.total = Number(pg.total ?? h.items.length);
            h.pages = Math.max(1, Number(pg.total_pages ?? 1));
            h.page = Number(pg.page ?? h.page);
            h.loaded = true;
            renderRewardsHistory();
        })
        .catch((error) => {
            const box = qs('#rewardsHistory');
            if (box) box.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
        });
}

// expandRedemption fills one row's detail area using the SINGLE
// redemption endpoint (A1: the list stays lean; details load on click).
function expandRedemption(code, detailEl) {
    return requestJson(`/api/owner/rewards/redemptions/${encodeURIComponent(code)}`, {method: 'GET'})
        .then((json) => {
            if (!json.success) throw apiErrorFrom(json, 'rewards.load_failed');
            const r = json.data.redemption || {};
            const rows = [
                [t('rewards.code'), r.code],
                [t('rewards.status'), rewardsStatusText(r.status)],
                [t('rewards.request_key'), r.request_key]
            ];
            if (r.review_note) rows.push([t('rewards.review_note'), r.review_note]);
            if (r.fulfillment_note) rows.push([t('rewards.fulfillment_note'), r.fulfillment_note]);
            if (r.updated_at) rows.push([t('rewards.updated'), tcvFmtDate(r.updated_at)]);
            detailEl.innerHTML = `<div class="rewards-result-grid">` + rows.map(([k, v]) =>
                `<span class="muted">${escapeHtml(k)}</span><span class="mono">${escapeHtml(String(v ?? ''))}</span>`).join('') + `</div>`;
            detailEl.hidden = false;
        })
        .catch((error) => {
            detailEl.innerHTML = `<p class="tcv-err">${escapeHtml(noticeText(error))}</p>`;
            detailEl.hidden = false;
        });
}

// redeemRewardsProduct redeems one product. The request_key is the Rewards
// Core idempotency contract: generated once per redeem() call and held
// fixed for the duration of that fetch — a new user action gets a new key.
// Format satisfies the backend contract [A-Za-z0-9_-]{1,64}.
let rewardsKeyCounter = 0;
function newRewardsRequestKey() {
    rewardsKeyCounter += 1;
    return 'web_' + Date.now() + '_' + Math.random().toString(36).slice(2, 10) + '_' + rewardsKeyCounter;
}

async function redeemRewardsProduct(productCode) {
    const requestKey = newRewardsRequestKey();
    const json = await requestJson('/api/owner/rewards/redemptions', {
        method: 'POST',
        body: JSON.stringify({product_code: productCode, request_key: requestKey})
    });
    if (!json.success) throw apiErrorFrom(json, 'rewards.redeem_failed');
    state.rewards.lastRedemption = json.data.redemption || null;
    return state.rewards.lastRedemption;
}

// Rewards page event binding: one Redeem click = one request (button
// disabled until the request settles; the backend idempotency remains
// the final defense, this is UX only). The history pager and row
// expansion bind once on the page shells (A1).
function bindRewardsHandlers() {
    const wrap = qs('#rewardsProducts');
    if (wrap) {
        wrap.addEventListener('click', async (event) => {
            const btn = event.target.closest('[data-redeem-code]');
            if (!btn || btn.disabled) return;
            const code = btn.getAttribute('data-redeem-code');
            btn.disabled = true;
            try {
                await redeemRewardsProduct(code);
                showToast(noticeText(t('rewards.result_created')), 'ok');
            } catch (error) {
                showToast(noticeText(error), 'error');
            } finally {
                // Balance is refreshed from the server fact either way,
                // and the history re-loads so the new row shows up.
                try { await loadAccount(); } catch (_) { /* keep server-fact refresh best-effort */ }
                renderRewards();
                loadRewardsHistory(1);
            }
        });
    }

    const prev = qs('#rewardsHistoryPrev');
    const next = qs('#rewardsHistoryNext');
    if (prev) prev.addEventListener('click', () => loadRewardsHistory(state.rewards.history.page - 1));
    if (next) next.addEventListener('click', () => loadRewardsHistory(state.rewards.history.page + 1));

    const history = qs('#rewardsHistory');
    if (history) {
        history.addEventListener('click', (event) => {
            const row = event.target.closest('[data-rh-code]');
            if (!row) return;
            const detail = row.querySelector('.rh-detail');
            if (!detail) return;
            if (!detail.hidden) { detail.hidden = true; return; }
            if (detail.childElementCount) { detail.hidden = false; return; }
            expandRedemption(row.getAttribute('data-rh-code'), detail);
        });
    }
}
