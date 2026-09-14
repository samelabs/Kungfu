// Package payment owns the payment domain: payment orders, provider
// identity/reference, fiat amount/currency, credits amount, payment status,
// and the paid fact.
//
// Boundary rules:
//   - the ONLY subject is bot_id (owner is a UI expression, not an entity);
//   - payment NEVER writes tb_bots.balance or tb_transactions — the sole
//     credit-granting path is credits.Record inside CompletePayment;
//   - the amount+credits pairing is a SERVER-side product decision
//     (PaymentSpec); there is deliberately no client-facing "pay X get Y"
//     API in this round;
//   - a future provider adapter may only (1) create/map a provider order,
//     (2) verify a provider callback, and (3) hand the verified "this
//     payment was really paid" fact to CompletePayment. Adapters have no
//     ability to grant credits.
package payment

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
	"kungfu.md/internal/repository"
)

// Credit grant type recorded in tb_transactions for a paid payment.
const TxnTypeGrantPayment = "grant_payment"

// Provider names are stored identifiers (e.g. "manual", future "stripe"),
// not secrets; webhook secrets never enter this table.
var providerPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// PaymentSpec is a server-confirmed payment specification: how much fiat
// buys how many credits. It is produced by server-side product/pricing
// decisions (and tests), never accepted raw from clients.
type PaymentSpec struct {
	Provider          string
	ProviderProductID *string // creation-time provider product snapshot (fixed packages)
	ProviderOrderID   *string // optional at creation; set by the provider adapter
	AmountMinor       int64   // fiat, integer minor units (e.g. cents)
	Currency          string  // ISO 4217, uppercase
	Credits           float64 // credits granted on paid
}

// CreatePendingPayment records a pending payment fact for a bot.
// The spec must come from a server-side decision; this function validates
// its shape and that the bot exists, then persists a pending row.
func CreatePendingPayment(ctx context.Context, pool *pg.Pool, botID int64, spec PaymentSpec) (*model.Payment, error) {
	if spec.ProviderOrderID != nil {
		oid := strings.TrimSpace(*spec.ProviderOrderID)
		spec.ProviderOrderID = &oid
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	if err := requireBot(ctx, pool, botID); err != nil {
		return nil, err
	}

	code, err := publiccode.GenerateUnique(func(c string) (bool, error) {
		return repository.PaymentCodeExists(ctx, pool, c)
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not allocate payment code")
	}

	p, err := repository.CreatePendingPayment(ctx, pool, code, repository.PaymentSpec{
		BotID:             botID,
		Provider:          spec.Provider,
		ProviderProductID: spec.ProviderProductID,
		ProviderOrderID:   spec.ProviderOrderID,
		AmountMinor:       spec.AmountMinor,
		Currency:          spec.Currency,
		Credits:           spec.Credits,
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not create payment")
	}
	return p, nil
}

// CompletePayment marks a pending payment as paid and grants its credits.
// This is the ONLY place payment causes a balance change, and it is atomic:
//
//	BEGIN
//	SELECT payment ... FOR UPDATE        -- serialization point
//	  status = paid    -> idempotent no-op (no second grant)
//	  status != pending -> rejected (no illegal transition)
//	credits.Record(+credits, grant_payment, ref payment, ref_id code)  [same tx]
//	UPDATE payment -> paid, paid_at = NOW()
//	COMMIT
//
// Any failure at any step rolls back BOTH the payment transition and the
// credit grant — neither can succeed halfway. Duplicate and concurrent
// confirmations are safe: the row lock serializes them and the second one
// sees status=paid and returns without granting.
//
// Returns the (possibly already) paid payment and whether this call
// performed the transition.
func CompletePayment(ctx context.Context, pool *pg.Pool, code string) (*model.Payment, bool, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Could not begin payment completion")
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	p, err := repository.LockPaymentByCode(ctx, tx, code)
	if err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Could not load payment")
	}
	if p == nil {
		return nil, false, errors.New(404, "NOT_FOUND", "Payment not found")
	}

	// Idempotency: an already-paid payment returns success without granting.
	if p.Status == model.PaymentStatusPaid {
		return p, false, nil
	}
	// Only pending can become paid.
	if p.Status != model.PaymentStatusPending {
		return nil, false, errors.New(409, "INVALID_PAYMENT_STATE",
			"Payment is "+p.Status+" and cannot be paid")
	}

	// Grant credits inside the same transaction (credits.Record joins tx;
	// it locks tb_bots, updates balance, inserts the ledger row).
	refType := "payment"
	if _, err := credits.Record(ctx, pool, tx, p.BotID, TxnTypeGrantPayment,
		p.Credits, &refType, &p.Code); err != nil {
		return nil, false, err // 402/404/500 — nothing committed
	}

	if err := repository.MarkPaymentPaid(ctx, tx, p.ID); err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Could not mark payment paid")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Could not commit payment completion")
	}

	// Reflect the committed transition.
	p.Status = model.PaymentStatusPaid
	now := time.Now()
	p.PaidAt = &now
	return p, true, nil
}

// FailPayment records pending -> failed. Terminal, no credits involved.
func FailPayment(ctx context.Context, pool *pg.Pool, code string) (bool, error) {
	return terminatePayment(ctx, pool, code, model.PaymentStatusFailed)
}

// CancelPayment records pending -> cancelled. Terminal, no credits involved.
func CancelPayment(ctx context.Context, pool *pg.Pool, code string) (bool, error) {
	return terminatePayment(ctx, pool, code, model.PaymentStatusCancelled)
}

func terminatePayment(ctx context.Context, pool *pg.Pool, code, status string) (bool, error) {
	ok, err := repository.SetPaymentStatus(ctx, pool, code, status)
	if err != nil {
		return false, errors.New(500, "INTERNAL_ERROR", "Could not update payment status")
	}
	return ok, nil
}

// GetPayment returns a payment by code (read-only).
func GetPayment(ctx context.Context, pool *pg.Pool, code string) (*model.Payment, error) {
	p, err := repository.FindPaymentByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load payment")
	}
	if p == nil {
		return nil, errors.New(404, "NOT_FOUND", "Payment not found")
	}
	return p, nil
}

// -- validation --

func validateSpec(spec PaymentSpec) error {
	if !providerPattern.MatchString(spec.Provider) {
		return errors.New(400, "INVALID_PROVIDER",
			"Provider must be 1-32 chars of [a-z0-9_]")
	}
	if !currencyPattern.MatchString(spec.Currency) {
		return errors.New(400, "INVALID_CURRENCY", "Currency must be an uppercase ISO 4217 code")
	}
	if spec.AmountMinor <= 0 {
		return errors.New(400, "INVALID_AMOUNT", "Amount must be greater than zero")
	}
	if math.IsNaN(spec.Credits) || math.IsInf(spec.Credits, 0) {
		return errors.New(400, "INVALID_CREDITS", "Credits must be a finite number")
	}
	if spec.Credits <= 0 {
		return errors.New(400, "INVALID_CREDITS", "Credits must be greater than zero")
	}
	if spec.ProviderOrderID != nil {
		oid := *spec.ProviderOrderID
		if oid == "" || len(oid) > 64 {
			return errors.New(400, "INVALID_PROVIDER_ORDER", "Provider order id must be 1-64 chars")
		}
	}
	return nil
}

// requireBot enforces the single subject rule: payments attach to an
// existing bot row (FK is the backstop).
func requireBot(ctx context.Context, q pg.Querier, botID int64) error {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM tb_bots WHERE id = $1`, botID).Scan(&id)
	if err == pgx.ErrNoRows {
		return errors.New(404, "BOT_NOT_FOUND", "Bot not found")
	}
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Could not verify bot")
	}
	return nil
}

// GetPaymentForBot is the ownership-scoped read for owner-facing
// surfaces: the query itself filters by bot_id.
func GetPaymentForBot(ctx context.Context, pool *pg.Pool, botID int64, code string) (*model.Payment, error) {
	p, err := repository.FindPaymentByCodeForBot(ctx, pool, botID, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load payment")
	}
	if p == nil {
		return nil, errors.New(404, "PAYMENT_NOT_FOUND", "Payment not found")
	}
	return p, nil
}

// BindProviderOrder atomically records which provider order backs a
// payment, before any grant. Idempotent when already bound to the same
// order; conflict (no grant) when bound to a different one or when the
// provider order already belongs to another payment.
func BindProviderOrder(ctx context.Context, pool *pg.Pool, provider, code, providerOrderID string) error {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return fmt.Errorf("begin bind tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	other, err := repository.ProviderOrderBelongsToAnotherPayment(ctx, tx, provider, providerOrderID, code)
	if err != nil {
		return fmt.Errorf("provider order lookup: %w", err)
	}
	if other {
		return fmt.Errorf("provider order %s already belongs to another payment", providerOrderID)
	}

	res, err := repository.BindProviderOrderByCode(ctx, tx, code, provider, providerOrderID)
	if err != nil {
		return err
	}
	if res == repository.BindProviderOrderConflict {
		return fmt.Errorf("payment %s provider order binding conflict", code)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit bind tx: %w", err)
	}
	return nil
}
