// render-credits.js — Owner Credits page rendering.
// Displays live fixed packages; the server remains the sole authority
// for prices and credits — the client never computes either.
// Every dynamic value reaching innerHTML goes through escapeHtml,
// matching the Store renderer style.

// Format an authoritative fiat minor-unit integer (canonical decimal
// STRING on the wire — never routed through JS Number) with EXACTLY
// two decimals: "1000" -> "10.00 USD", "1999" -> "19.99", "1" -> "0.01".
// Sign handled at the string level.
function creditsFormatAmount(amountMinor, currency) {
    if (typeof amountMinor !== 'string' || !/^-?(0|[1-9][0-9]*)$/.test(amountMinor)) return '';
    const cur = (currency || '').toUpperCase();
    const neg = amountMinor.startsWith('-');
    const digits = neg ? amountMinor.slice(1) : amountMinor;
    const padded = digits.padStart(3, '0');
    const text = padded.slice(0, -2) + '.' + padded.slice(-2);
    return (neg ? '-' : '') + text + ' ' + cur;
}

function renderCredits() {
    const wrap = qs('#creditsPackages');
    if (!wrap) return;
    const balanceEl = qs('#creditsBalance');
    // balance arrives as a canonical decimal string (economic integer
    // wire contract) — displayed verbatim, no Number() conversion.
    if (balanceEl && state.account && state.account.balance != null) {
        balanceEl.textContent = String(state.account.balance);
    }
    const pkgs = (state.credits && state.credits.packages) || [];
    // Loading/unavailable/error states are owned by the shared
    // section lifecycle (sectionBox). The renderer only paints the
    // ready state — a non-empty package list. PAYMENT_NOT_CONFIGURED
    // and packages=[] both map to unavailable upstream.
    if (!pkgs.length) {
        return;
    }
    wrap.innerHTML = pkgs.map((p) => `
        <div class="store-product" data-package-code="${escapeHtml(p.code)}">
            <div class="store-product-main">
                <strong>${escapeHtml(p.name)}</strong>
                <div class="muted">${escapeHtml(t('credits.price'))}: ${escapeHtml(creditsFormatAmount(p.amount_minor, p.currency))}</div>
                <div>${escapeHtml(t('credits.credits'))}: ${escapeHtml(String(p.credits))}</div>
            </div>
            <div class="store-product-actions">
                <button class="btn primary" type="button" data-buy-package="${escapeHtml(p.code)}" ${state.credits.buying ? 'disabled' : ''}>
                    ${escapeHtml(state.credits.buying === p.code ? t('credits.buying') : t('credits.buy'))}
                </button>
            </div>
        </div>`).join('');
}

function renderCreditsPaymentResult() {
    const box = qs('#creditsPaymentResult');
    if (!box) return;
    const pending = (state.credits && state.credits.lastPayment) || null;
    if (!pending) {
        box.hidden = true;
        return;
    }
    const statusKey = {
        pending: 'owner.credits.payment_pending',
        paid: 'owner.credits.payment_paid',
        failed: 'owner.credits.payment_failed',
        cancelled: 'owner.credits.payment_cancelled'
    }[pending.status];
    // i18n label when known, else the raw status value — both escaped.
    const label = statusKey ? t(statusKey) : String(pending.status);
    box.hidden = false;
    let html = `<div><strong>${escapeHtml(label)}</strong></div>`;
    if (pending.status === 'pending') {
        html += `<div class="muted">${escapeHtml(t('credits.pending_hint'))}</div>`;
        html += `<div class="actions"><button class="btn" type="button" id="creditsCheckStatus">${escapeHtml(t('credits.check_status'))}</button></div>`;
    }
    box.innerHTML = html;
}
