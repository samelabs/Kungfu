package server

// WO-28 C11: the economic chain must be readable in the owner console.
// Every ledger type string the code writes into tb_transactions must
// have an owner.logs.tx_<type> label in ALL five languages — the logs
// screen renders transaction types through that key
// (web/assets/owner/render-logs.js: `logs.tx_${type}`), and a missing
// label would surface a raw key to the owner.
//
// The type list is taken from the owning packages' constants (the same
// constants the Record call sites use), not restated here.

import (
	"testing"

	"kungfu.md/internal/consumption"
	"kungfu.md/internal/i18n"
	"kungfu.md/internal/payment"
	"kungfu.md/internal/rewards"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func TestEveryLedgerTypeHasOwnerLogLabelInAllLanguages(t *testing.T) {
	ledgerTypes := []string{
		service.TxnTypeGrantSignup,      // registration genesis grant
		payment.TxnTypeGrantPayment,     // confirmed Creem top-up
		payment.TxnTypeReversePayment,   // provider refund / dispute reversal
		task.LedgerTypeLock,             // task_create budget lock
		task.LedgerTypeFund,             // task_fund budget add
		task.LedgerTypeEarn,             // submission settlement payout
		task.LedgerTypeRefund,           // task_refund budget return
		rewards.TxnTypeSpendRedemption,  // reward redeemed
		rewards.TxnTypeRefundRedemption, // redemption rejected → refund
		consumption.TxnTypeSpendPush,    // storage publish (priced 0 today)
		consumption.TxnTypeSpendGet,     // storage read (priced 0 today)
	}

	// The exact set the ledger can contain: nothing else may appear in
	// the code as a credits.Record / RecordAuthoritativeReversal type.
	// (Kept honest by the greppable constants above — each call site
	// names one of these.)
	if len(ledgerTypes) != 11 {
		t.Fatalf("expected 11 ledger types, got %d", len(ledgerTypes))
	}

	for _, locale := range i18n.SupportedLocales() {
		for _, txType := range ledgerTypes {
			key := "owner.logs.tx_" + txType
			if !i18n.Has(locale, key) {
				t.Errorf("locale %q: missing i18n key %s for ledger type %q", locale, key, txType)
			}
		}
	}
}
