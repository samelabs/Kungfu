// render-credits.js — Owner Credits page rendering.
// Displays live fixed packages; the server remains the sole authority
// for prices and credits — the client never computes either.
// Every dynamic value reaching innerHTML goes through escapeHtml,
// matching the Store renderer style.

function creditsFormatAmount(amountMinor, currency) {
    if (typeof amountMinor !== 'number' || !Number.isFinite(amountMinor)) return '';
    const cur = (currency || '').toUpperCase();
    const major = amountMinor / 100;
    const formatted = Number.isInteger(major) ? String(major) : major.toFixed(2);
    return `${formatted} ${cur}`;
}

function renderCredits() {
    const wrap = qs('#creditsPackages');
    if (!wrap) return;
    const balanceEl = qs('#creditsBalance');
    if (balanceEl && state.account && typeof state.account.balance === 'number') {
        balanceEl.textContent = state.account.balance;
    }
    const pkgs = (state.credits && state.credits.packages) || [];
    if (!state.credits || !state.credits.loaded) {
        wrap.innerHTML = `<div class="muted">${escapeHtml(t('owner.credits.loading'))}</div>`;
        return;
    }
    if (!pkgs.length) {
        wrap.innerHTML = `<div class="muted">${escapeHtml(t('owner.credits.unavailable'))}</div>`;
        return;
    }
    wrap.innerHTML = pkgs.map((p) => `
        <div class="store-product" data-package-code="${escapeHtml(p.code)}">
            <div class="store-product-main">
                <strong>${escapeHtml(p.name)}</strong>
                <div class="muted">${escapeHtml(t('owner.credits.price'))}: ${escapeHtml(creditsFormatAmount(p.amount_minor, p.currency))}</div>
                <div>${escapeHtml(t('owner.credits.credits'))}: ${escapeHtml(String(p.credits))}</div>
            </div>
            <div class="store-product-actions">
                <button class="btn primary" type="button" data-buy-package="${escapeHtml(p.code)}" ${state.credits.buying ? 'disabled' : ''}>
                    ${escapeHtml(state.credits.buying === p.code ? t('owner.credits.buying') : t('owner.credits.buy'))}
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
        html += `<div class="muted">${escapeHtml(t('owner.credits.pending_hint'))}</div>`;
        html += `<div class="actions"><button class="btn" type="button" id="creditsCheckStatus">${escapeHtml(t('owner.credits.check_status'))}</button></div>`;
    }
    box.innerHTML = html;
}
