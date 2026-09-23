/* 012 Finance Admin UI — READ-ONLY.
 * Payments / Payment Adjustments / Credits Ledger / local
 * reconciliation. No mutation controls exist here by design; the
 * server (finance.read) is the authority. All monetary values
 * arrive as canonical decimal strings and are rendered with
 * String(v) only — never Number().
 */
(function () {
    'use strict';

    var paymentsPage = 1, adjustmentsPage = 1, ledgerPage = 1;

    // Account scope (?bot_id=N entered from the Accounts detail
    // cross-link): Payments / Adjustments / Ledger requests all
    // carry bot_id. The Summary stays a GLOBAL aggregate and is
    // clearly labelled as such — it is never silently scoped.
    var scopeBot = null;
    var m = window.location.search.match(/[?&]bot_id=(\d+)/);
    if (m) scopeBot = m[1];

    function el(id) { return document.getElementById(id); }
    function esc(v) {
        return window.escapeHtml ? window.escapeHtml(String(v == null ? '' : v)) : String(v);
    }
    function money(v) { return esc(v); } // canonical decimal string, rendered as-is

    function api(path) { return window.adminAPI ? window.adminAPI(path) : fetch(path); }

    function table(headers, rows) {
        var h = headers.map(function (t) { return '<th>' + esc(t) + '</th>'; }).join('');
        var b = rows.map(function (r) {
            return '<tr>' + r.map(function (c) { return '<td>' + c + '</td>'; }).join('') + '</tr>';
        }).join('');
        return '<table class="admin-table"><thead><tr>' + h + '</tr></thead><tbody>' + b + '</tbody></table>';
    }

    function pager(which, page, total, pageSize) {
        var pages = Math.max(1, Math.ceil(total / pageSize));
        return '<div class="admin-pager">' +
            '<button class="btn" data-finance-page="' + which + '" data-page="' + (page - 1) + '"' + (page <= 1 ? ' disabled' : '') + '>&laquo; prev</button> ' +
            '<span>page ' + page + ' / ' + pages + ' (' + total + ' rows)</span> ' +
            '<button class="btn" data-finance-page="' + which + '" data-page="' + (page + 1) + '"' + (page >= pages ? ' disabled' : '') + '>next &raquo;</button></div>';
    }

    function loadSummary() {
        return api('/api/samelabs/finance/summary').then(function (r) { return r.json(); }).then(function (body) {
            var d = body.data || {};
            var statusRows = Object.keys(d.payments_by_status || {}).map(function (k) {
                return [esc(k), esc(d.payments_by_status[k])];
            });
            var volumeRows = (d.paid_volume || []).map(function (v) {
                return [esc(v.currency), money(v.amount_minor), esc(v.count)];
            });
            var scopeNote = scopeBot
                ? '<p class="admin-note"><strong>Account scope active: bot #' + esc(scopeBot) + '</strong> — Payments / Adjustments / Ledger below are scoped to this account. <a href="/samelabs/finance">Clear scope</a></p>'
                : '';
            el('adminFinanceSummary').innerHTML =
                '<h3>Summary <small>(global — not account-scoped)</small></h3>' + scopeNote +
                table(['payment status', 'count'], statusRows) +
                '<p><strong>Paid volume</strong> (per currency — never summed across currencies; this is not revenue):</p>' +
                table(['currency', 'amount_minor', 'payments'], volumeRows) +
                '<p>Granted credits: <strong>' + money(d.granted_credits) + '</strong> · Reversed credits: <strong>' + money(d.reversed_credits) + '</strong> · ' +
                'Refund facts: ' + esc(d.refund_fact_count) + ' · Dispute facts: ' + esc(d.dispute_fact_count) + '</p>';
        });
    }

    function loadPayments(page) {
        var qs = '?page=' + page + '&page_size=20';
        if (scopeBot) qs += '&bot_id=' + scopeBot;
        var st = el('financePaymentStatusFilter').value;
        var q = el('financePaymentQFilter').value.trim();
        if (st) qs += '&status=' + encodeURIComponent(st);
        if (q) qs += '&q=' + encodeURIComponent(q);
        return api('/api/samelabs/finance/payments' + qs).then(function (r) { return r.json(); }).then(function (body) {
            var d = body.data || {};
            paymentsPage = page;
            var rows = (d.payments || []).map(function (p) {
                return [
                    '<a href="#" data-finance-code="' + esc(p.code) + '">' + esc(p.code) + '</a>',
                    esc(p.bot_id) + ' / ' + esc(p.bot_name),
                    esc(p.provider), esc(p.status), money(p.amount_minor) + ' ' + esc(p.currency),
                    money(p.credits), esc(p.created_at), p.paid_at ? esc(p.paid_at) : '—'
                ];
            });
            el('adminFinancePayments').innerHTML = table(
                ['code', 'bot', 'provider', 'status', 'amount', 'credits', 'created', 'paid'], rows) +
                pager('payments', page, d.total || 0, 20);
        });
    }

    function loadAdjustments(page) {
        var qs = '?page=' + page + '&page_size=20';
        if (scopeBot) qs += '&bot_id=' + scopeBot;
        var kind = el('financeAdjustmentKindFilter').value;
        var code = el('financeAdjustmentCodeFilter').value.trim();
        if (kind) qs += '&kind=' + encodeURIComponent(kind);
        if (code) qs += '&payment_code=' + encodeURIComponent(code);
        return api('/api/samelabs/finance/adjustments' + qs).then(function (r) { return r.json(); }).then(function (body) {
            var d = body.data || {};
            adjustmentsPage = page;
            var rows = (d.adjustments || []).map(function (a) {
                return [
                    esc(a.payment_code), esc(a.bot_name), esc(a.kind), esc(a.provider),
                    esc(a.provider_transaction_id), money(a.amount_minor) + ' ' + esc(a.currency),
                    money(a.refunded_amount_minor), esc(a.object_status), esc(a.created_at)
                ];
            });
            el('adminFinanceAdjustments').innerHTML = table(
                ['payment', 'bot', 'kind', 'provider', 'provider txn', 'amount', 'refunded so far', 'object status', 'created'], rows) +
                pager('adjustments', page, d.total || 0, 20);
        });
    }

    function loadLedger(page) {
        var qs = '?page=' + page + '&page_size=20';
        var bot = scopeBot || el('financeLedgerBotFilter').value.trim();
        var type = el('financeLedgerTypeFilter').value.trim();
        if (bot) qs += '&bot_id=' + encodeURIComponent(bot);
        if (type) qs += '&type=' + encodeURIComponent(type);
        return api('/api/samelabs/finance/ledger' + qs).then(function (r) { return r.json(); }).then(function (body) {
            var d = body.data || {};
            ledgerPage = page;
            var rows = (d.entries || []).map(function (e) {
                return [esc(e.id), esc(e.bot_id) + ' / ' + esc(e.bot_name), esc(e.type),
                    money(e.amount), money(e.balance_after), esc(e.ref_type) + ':' + esc(e.ref_id), esc(e.created_at)];
            });
            el('adminFinanceLedger').innerHTML = table(
                ['id', 'bot', 'type', 'amount', 'balance_after', 'ref', 'created'], rows) +
                pager('ledger', page, d.total || 0, 20);
        });
    }

    function flag(v) { return v ? '<span class="ok">PASS</span>' : '<span class="fail">FAIL</span>'; }

    // No-ledger accounts must render N/A — never a fake PASS, never
    // a FAIL (there is no ledger fact to compare against).
    function ledgerFlag(v, latest) {
        if (latest === null || latest === undefined) return '<span class="na">N/A</span>';
        return flag(v);
    }

    function loadPaymentDetail(code) {
        return api('/api/samelabs/finance/payments/' + encodeURIComponent(code)).then(function (r) { return r.json(); }).then(function (body) {
            var d = body.data || {};
            var p = d.payment || {};
            var rec = d.reconciliation || {};
            var integ = rec.integrity || {};
            var adjRows = (d.adjustments || []).map(function (a) {
                return [esc(a.kind), esc(a.provider_transaction_id), money(a.amount_minor) + ' ' + esc(a.currency),
                    money(a.refunded_amount_minor), esc(a.object_status), esc(a.reason || '')];
            });
            var grantRows = (d.grant_payment_ledger || []).map(function (e) {
                return [esc(e.id), money(e.amount), money(e.balance_after), esc(e.created_at)];
            });
            var revRows = (d.reverse_payment_ledger || []).map(function (e) {
                return [esc(e.id), money(e.amount), money(e.balance_after), esc(e.created_at)];
            });
            var latest = rec.latest_account_ledger_balance_after;
            el('adminFinancePaymentDetail').innerHTML =
                '<h3>Payment ' + esc(code) + '</h3>' +
                table(['field', 'value'], [
                    ['bot', esc(p.bot_id) + ' / ' + esc(p.bot_name)],
                    ['provider', esc(p.provider) + ' · order ' + esc(p.provider_order_id || '—')],
                    ['amount', money(p.amount_minor) + ' ' + esc(p.currency)],
                    ['credits entitlement', money(p.credits)],
                    ['status', esc(p.status) + ' (adjustments are separate facts — status is never rewritten)']
                ]) +
                '<h4>Adjustments</h4>' + table(['kind', 'provider txn', 'amount', 'refunded so far', 'object status', 'reason'], adjRows) +
                '<h4>grant_payment ledger</h4>' + table(['id', 'amount', 'balance_after', 'created'], grantRows) +
                '<h4>reverse_payment ledger</h4>' + table(['id', 'amount', 'balance_after', 'created'], revRows) +
                '<h4>Local reconciliation</h4>' +
                table(['fact', 'value'], [
                    ['grant_payment count / sum', esc(rec.grant_payment_count) + ' / ' + money(rec.grant_payment_sum)],
                    ['reverse_payment count / sum', esc(rec.reverse_payment_count) + ' / ' + money(rec.reverse_payment_sum)],
                    ['adjustment count (refund/dispute)', esc(rec.adjustment_count) + ' (' + esc(rec.refund_fact_count) + '/' + esc(rec.dispute_fact_count) + ')'],
                    ['distinct provider-txn bases / amount-paid bases', esc(rec.distinct_adjustment_provider_transaction_count) + ' / ' + esc(rec.distinct_adjustment_amount_paid_count)],
                    ['max persisted refunded amount', money(rec.max_persisted_refunded_amount)],
                    ['account balance', money(rec.current_account_balance)],
                    ['latest ledger balance_after', latest == null ? 'n/a (no ledger rows)' : money(latest)]
                ]) +
                table(['integrity check', 'result'], [
                    ['paid_grant_exact', flag(integ.paid_grant_exact)],
                    ['adjustment_basis_consistent', flag(integ.adjustment_basis_consistent)],
                    ['reverse_payment_nonpositive', flag(integ.reverse_payment_nonpositive)],
                    ['reverse_payment_within_original_entitlement', flag(integ.reverse_payment_within_original_entitlement)],
                    ['account_balance_matches_latest_ledger', ledgerFlag(integ.account_balance_matches_latest_ledger, rec.latest_account_ledger_balance_after)]
                ]);
            window.scrollTo(0, el('adminFinancePaymentDetail').offsetTop);
        });
    }

    window.loadFinance = function () {
        return Promise.all([loadSummary(), loadPayments(1), loadAdjustments(1), loadLedger(1)]);
    };

    window.renderFinance = function () { /* render happens inline in loaders */ };

    window.bindFinanceEvents = function () {
        var root = document.getElementById('adminFinanceSection');
        if (!root || root.dataset.bound) return;
        root.dataset.bound = '1';

        el('adminFinancePaymentFilters').addEventListener('submit', function (ev) {
            ev.preventDefault(); loadPayments(1);
        });
        el('adminFinanceAdjustmentFilters').addEventListener('submit', function (ev) {
            ev.preventDefault(); loadAdjustments(1);
        });
        el('adminFinanceLedgerFilters').addEventListener('submit', function (ev) {
            ev.preventDefault(); loadLedger(1);
        });
        root.addEventListener('click', function (ev) {
            var t = ev.target.closest ? ev.target.closest('[data-finance-page],[data-finance-code]') : null;
            if (!t) return;
            ev.preventDefault();
            if (t.dataset.financePage) {
                var page = parseInt(t.dataset.page, 10) || 1;
                if (t.dataset.financePage === 'payments') loadPayments(page);
                else if (t.dataset.financePage === 'adjustments') loadAdjustments(page);
                else loadLedger(page);
            } else if (t.dataset.financeCode) {
                loadPaymentDetail(t.dataset.financeCode);
            }
        });
    };
})();
