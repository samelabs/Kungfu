// credits.js — Owner Credits page logic: load packages, buy (single
// checkout redirect), and one-shot pending-payment status on return.
//
// Invariants:
//   - The checkout request body is ONLY {"package": code}. Never units,
//     amount, credits, product_id, or custom_price — those are server
//     owned facts.
//   - payment.code is saved to sessionStorage BEFORE redirecting to the
//     provider checkout_url (which comes verbatim from the server).
//   - On return, the pending payment is checked exactly once via
//     GET /api/owner/payments/{code}; "Check status" re-checks on
//     demand. No polling, no websockets.
//   - Nothing here can grant credits: only the signed
//     checkout.completed webhook can.

const CREDITS_PENDING_KEY = 'kungfu_pending_payment_code';

function creditsReset() {
    state.credits = {
        packages: [],
        loaded: false,
        buying: null,
        lastPayment: null
    };
}

async function loadCreditsPackages() {
    const json = await requestJson('/api/owner/payments/packages', {method: 'GET'});
    if (!json.success) throw new Error(noticeText(json.error || t('credits.packages_failed')));
    state.credits.packages = (json.data && json.data.packages) || [];
    state.credits.loaded = true;
}

async function buyPackage(code) {
    if (state.credits.buying) return;
    state.credits.buying = code;
    renderCredits();
    // buying — silent (status panel shows it)
    try {
        const json = await requestJson('/api/owner/payments/checkout', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            // The ONLY client-controlled fact is the package code.
            body: JSON.stringify({package: code})
        });
        if (!json.success || !json.data || !json.data.checkout_url) {
            throw new Error(noticeText((json && json.error) || t('credits.checkout_failed')));
        }
        // Persist this payment's code before leaving: the return visit
        // resolves the payment status from it.
        try {
            sessionStorage.setItem(CREDITS_PENDING_KEY, json.data.payment.code);
        } catch (err) { /* storage unavailable: status check is simply skipped */ }
        window.location.assign(json.data.checkout_url);
    } catch (error) {
        state.credits.buying = null;
        renderCredits();
        showToast(noticeText(String(error)), 'error')
    }
}

async function checkCreditsPayment(code, {silent} = {}) {
    const json = await requestJson(`/api/owner/payments/${encodeURIComponent(code)}`, {method: 'GET'});
    if (!json.success || !json.data || !json.data.payment) {
        if (!silent) showToast(noticeText(json.error || t('credits.checkout_failed')), 'error')
        return null;
    }
    const payment = json.data.payment;
    state.credits.lastPayment = {code, status: payment.status};
    if (payment.status === 'paid' || payment.status === 'failed' || payment.status === 'cancelled') {
        creditsClearPending();
    }
    if (payment.status === 'paid') {
        // Refresh the balance from the existing account endpoint.
        try {
            await loadAccount();
        } catch (err) { /* balance refresh is best-effort; server is authority */ }
    }
    renderCredits();
    renderCreditsPaymentResult();
    return payment;
}

function creditsClearPending() {
    try {
        sessionStorage.removeItem(CREDITS_PENDING_KEY);
    } catch (err) { /* ignore */ }
}

async function initCreditsReturnStatus() {
    // Exactly one automatic check on page init — never polling.
    let code = null;
    try {
        code = sessionStorage.getItem(CREDITS_PENDING_KEY);
    } catch (err) { return; }
    if (!code) return;
    try {
        await checkCreditsPayment(code, {silent: true});
    } catch (error) { /* keep the pending code for the manual button */ }
}

function bindCreditsEvents() {
    document.addEventListener('click', (event) => {
        const buyBtn = event.target.closest('[data-buy-package]');
        if (buyBtn) {
            event.preventDefault();
            buyPackage(buyBtn.dataset.buyPackage);
            return;
        }
        if (event.target.id === 'creditsCheckStatus') {
            event.preventDefault();
            let code = null;
            try { code = sessionStorage.getItem(CREDITS_PENDING_KEY); } catch (err) { /* ignore */ }
            if (!code && state.credits.lastPayment) code = state.credits.lastPayment.code;
            if (code) checkCreditsPayment(code, {silent: false}).catch((error) => {
                showToast(noticeText(String(error)), 'error')
            });
        }
    });
}
