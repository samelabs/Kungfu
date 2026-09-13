// Package consumption owns platform-capability pricing: which capability
// actions are charged, how much, and which ledger type they book. It is
// the single layer between capability consumers (storage/kungfu today) and
// the credits domain.
//
// Boundary rules:
//   - consumers call Apply(ctx, pool, tx, botID, action, refType, refID)
//     and never pass an amount or a ledger type — pricing lives here;
//   - consumption is the only credits caller on this path; it must not
//     import consumer domains (storage/kungfu/task/store/payment) nor
//     write tb_bots.balance / tb_transactions itself;
//   - economic facts (payment grants, redemption refunds, task budget
//     settlement, signup grants) stay with their owning domains and call
//     credits directly — they are NOT capability consumption.
//
// Making an action free later means editing policy here only.
package consumption

import (
	"context"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// Action identifies a chargeable platform-capability usage.
type Action string

const (
	// ActionStorageCreate: publishing a kungfu to storage.
	ActionStorageCreate Action = "storage.create"
	// ActionStorageGetPublic: reading someone else's public kungfu.
	ActionStorageGetPublic Action = "storage.get_public"
)

// policy is the pricing decision for one action: the credit amount and
// the ledger type booked. Ledger types keep their historical names so the
// existing statement stays continuous — no data migration.
type policy struct {
	amount  float64
	txnType string
	errCode string
	errMsg  string
}

var policies = map[Action]policy{
	// Storage actions are currently FREE (amount 0): no credits call, no
	// balance check, no ledger row. Re-enabling charging means setting a
	// negative amount (+ txnType) here only — the historical ledger types
	// are kept in the policy for that future.
	ActionStorageCreate: {
		amount:  0,
		txnType: "spend_push",
		errCode: "INSUFFICIENT_CREDITS",
		errMsg:  "Need 1 credit to publish kungfu. Complete platform tasks to earn credits.",
	},
	ActionStorageGetPublic: {
		amount:  0,
		txnType: "spend_get",
		errCode: "INSUFFICIENT_CREDITS",
		errMsg:  "Need 1 credit to retrieve. Complete platform tasks to earn credits.",
	},
}

// Apply charges botID for one use of action, booking refType/refID on the
// ledger row, and joins the caller's transaction when tx != nil (caller
// owns commit/rollback); otherwise it runs in its own transaction.
// Insufficient funds surface as a 402 AppError; DB failures propagate.
//
// A FREE action (amount 0) short-circuits: credits.Record is never called,
// no balance is read, nothing enters the ledger.
func Apply(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	action Action, refType, refID string) error {

	p, ok := policies[action]
	if !ok {
		return errors.New(500, "INTERNAL_ERROR", "Unknown consumption action: "+string(action))
	}

	// Free action: no charge, no balance check, no ledger.
	if p.amount == 0 {
		return nil
	}

	_, err := credits.Record(ctx, pool, tx, botID, p.txnType, p.amount, &refType, &refID)
	if err != nil {
		if ae, isApp := errors.IsAppError(err); isApp && ae.HTTPCode == 402 {
			return errors.New(402, p.errCode, p.errMsg)
		}
		return errors.New(500, "INTERNAL_ERROR", "Could not apply consumption")
	}
	return nil
}
