async function loadStoreProducts() {
    const json = await requestJson('/api/owner/store/products', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || t('owner.store.load_failed')));
    state.store.products = json.data.products || [];
}

// redeemStoreProduct redeems one product. The request_key is the Store
// Core idempotency contract: generated once per redeem() call and held
// fixed for the duration of that fetch — a new user action gets a new key.
// Format satisfies the backend contract [A-Za-z0-9_-]{1,64}.
let storeKeyCounter = 0;
function newStoreRequestKey() {
    storeKeyCounter += 1;
    return 'web_' + Date.now() + '_' + Math.random().toString(36).slice(2, 10) + '_' + storeKeyCounter;
}

async function redeemStoreProduct(productCode) {
    const requestKey = newStoreRequestKey();
    const json = await requestJson('/api/owner/store/redemptions', {
        method: 'POST',
        body: JSON.stringify({product_code: productCode, request_key: requestKey})
    });
    if (!json.success) throw new Error(noticeText(json.error || t('owner.store.redeem_failed')));
    state.store.lastRedemption = json.data.redemption || null;
    return state.store.lastRedemption;
}

// Store page event binding: one Redeem click = one request (button
// disabled until the request settles; the backend idempotency remains
// the final defense, this is UX only).
function bindStoreHandlers() {
    const wrap = qs('#storeProducts');
    if (!wrap) return;
    wrap.addEventListener('click', async (event) => {
        const btn = event.target.closest('[data-redeem-code]');
        if (!btn || btn.disabled) return;
        const code = btn.getAttribute('data-redeem-code');
        btn.disabled = true;
        try {
            await redeemStoreProduct(code);
            setNotice('storeNotice', t('owner.store.result_created'), 'ok');
        } catch (error) {
            setNotice('storeNotice', error, 'error');
        } finally {
            // Balance is refreshed from the server fact either way.
            try { await loadAccount(); } catch (_) { /* keep server-fact refresh best-effort */ }
            renderStore();
        }
    });
}
