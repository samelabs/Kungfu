async function loadRewardsProducts() {
    const json = await requestJson('/api/owner/rewards/products', {method: 'GET'});
    if (!json.success) throw apiErrorFrom(json, 'rewards.load_failed');
    state.rewards.products = json.data.products || [];
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
// the final defense, this is UX only).
function bindRewardsHandlers() {
    const wrap = qs('#rewardsProducts');
    if (!wrap) return;
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
            // Balance is refreshed from the server fact either way.
            try { await loadAccount(); } catch (_) { /* keep server-fact refresh best-effort */ }
            renderRewards();
        }
    });
}
