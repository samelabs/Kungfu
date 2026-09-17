// Package store owns the credit-value-exit domain: the product catalog,
// redemption orders, review state, fulfillment state, and price/title
// snapshots. It is deliberately a single package — catalog and redemption
// are one business domain, not two.
//
// Boundary rules:
//   - the ONLY subject is bot_id (no owner/admin entity this round);
//   - store NEVER writes tb_bots.balance or tb_transactions — every credit
//     movement goes through credits.Record inside the redemption
//     transaction (spend at creation, refund on reject/cancel);
//   - store must not import payment / task / kungfu / storage;
//   - catalog mutation is an internal service capability only (no HTTP
//     surface: there is no platform-admin identity model yet).
package store

import (
	"context"
	stderrors "errors"
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

// Credit transaction types recorded in tb_transactions.
const (
	TxnTypeSpendRedemption  = "spend_redemption"
	TxnTypeRefundRedemption = "refund_redemption"
)

var requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// -- catalog --

// ProductInput is the payload for CreateProduct.
type ProductInput struct {
	Title        string
	Description  string
	CreditsPrice float64
}

// CreateProduct adds an active product to the catalog. Thin tx-owner
// wrapper over the SAME CreateProductTx primitive the Admin control
// plane uses — one implementation, no second state machine.
func CreateProduct(ctx context.Context, pool *pg.Pool, in ProductInput) (*model.StoreProduct, error) {
	var out *model.StoreProduct
	err := runInTx(ctx, pool, func(tx pgx.Tx) error {
		oc, err := CreateProductTx(ctx, pool, tx, in)
		if err != nil {
			return err
		}
		out = oc.After
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runInTx is the shared wrapper-owner transaction helper: BEGIN,
// fn, COMMIT (rollback on any error).
func runInTx(ctx context.Context, pool *pg.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Could not begin transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Could not commit transaction")
	}
	return nil
}

// SetProductActive / SetProductInactive toggle catalog visibility.
// Deactivation is the only "removal": rows persist for redemption history.
func SetProductActive(ctx context.Context, pool *pg.Pool, code string) (bool, error) {
	return setProductStatus(ctx, pool, code, model.ProductStatusActive)
}

func SetProductInactive(ctx context.Context, pool *pg.Pool, code string) (bool, error) {
	return setProductStatus(ctx, pool, code, model.ProductStatusInactive)
}

func setProductStatus(ctx context.Context, pool *pg.Pool, code, status string) (bool, error) {
	ok, err := repository.SetStoreProductStatus(ctx, pool, code, status)
	if err != nil {
		return false, errors.New(500, "INTERNAL_ERROR", "Could not update product status")
	}
	return ok, nil
}

// GetProduct returns a product by code regardless of status.
func GetProduct(ctx context.Context, pool *pg.Pool, code string) (*model.StoreProduct, error) {
	p, err := repository.FindStoreProductByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load product")
	}
	if p == nil {
		return nil, errors.New(404, "PRODUCT_NOT_FOUND", "Product not found")
	}
	return p, nil
}

// ListActiveProducts returns the active catalog.
func ListActiveProducts(ctx context.Context, pool *pg.Pool) ([]model.StoreProduct, error) {
	items, err := repository.ListActiveStoreProducts(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load catalog")
	}
	return items, nil
}

// -- redemption creation (debit at creation, atomically) --

// RedeemResult is the outcome of a Redeem call: either a fresh
// pending_review redemption (Created=true) or an idempotent replay of an
// earlier one (Created=false).
type RedeemResult struct {
	Redemption *model.Redemption
	Created    bool
}

// Redeem creates a redemption order and debits the credits — atomically,
// debited immediately at creation (NOT deferred to review approval), so
// the credits cannot be spent elsewhere while the order is pending.
//
//	Idempotency: (bot_id, request_key) resolves to ONE redemption ever.
//	Replay with the same product  -> same redemption returned, Created=false.
//	Replay with a different product -> 409 IDEMPOTENCY_CONFLICT.
//
// PG aborted-transaction pitfall: when two concurrent Redeem calls race on
// (bot_id, request_key), the loser's INSERT hits the UNIQUE constraint
// INSIDE its transaction. Instead of continuing to query on an aborted
// transaction, the loser rolls back entirely and resolves the replay on a
// fresh pool connection (read-only). The DB constraint remains the last
// line of defense; the handler layer is not involved.
func Redeem(ctx context.Context, pool *pg.Pool, botID int64, productCode, requestKey string) (*RedeemResult, error) {
	requestKey = strings.TrimSpace(requestKey)
	if !requestKeyPattern.MatchString(requestKey) {
		return nil, errors.New(400, "INVALID_REQUEST_KEY",
			"Request key must be 1-64 chars of [A-Za-z0-9_-]")
	}

	// Fast path: an existing redemption for (bot, request_key) is resolved
	// before any transaction work.
	if existing, err := repository.FindRedemptionByRequestKey(ctx, pool, botID, requestKey); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not check request key")
	} else if existing != nil {
		return resolveReplay(ctx, pool, existing, productCode)
	}

	// Insufficient credits fail fast before opening a transaction
	// (balance is re-checked authoritatively inside credits.Record).
	balance, err := credits.Balance(ctx, pool, botID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, errors.New(404, "BOT_NOT_FOUND", "Bot not found")
		}
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not read balance")
	}

	// Read the product for a pre-check (locked read happens in the tx).
	product, err := repository.FindStoreProductByCode(ctx, pool, productCode)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load product")
	}
	if product == nil {
		return nil, errors.New(404, "PRODUCT_NOT_FOUND", "Product not found")
	}
	if product.Status != model.ProductStatusActive {
		return nil, errors.New(409, "PRODUCT_INACTIVE", "Product is not available for redemption")
	}
	if balance < product.CreditsPrice {
		return nil, errors.New(402, "INSUFFICIENT_CREDITS",
			"Not enough credits for this product")
	}

	res, err := redeemTx(ctx, pool, botID, productCode, requestKey)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// redeemTx is the atomic core of Redeem:
//
//	BEGIN
//	Lock active product (FOR UPDATE)      -- snapshot source: title + price
//	  missing/inactive -> 404/409
//	Insert pending_review redemption      -- snapshot title + credits_price
//	  UNIQUE(bot_id, request_key) violation -> rollback, resolve on pool
//	credits.Record(-cost, spend_redemption, ref redemption/<code>)  [same tx]
//	COMMIT
func redeemTx(ctx context.Context, pool *pg.Pool, botID int64, productCode, requestKey string) (*RedeemResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not begin redemption")
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// Lock the product row and take the snapshot from it.
	product, err := repository.LockActiveStoreProductByCode(ctx, tx, productCode)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load product")
	}
	if product == nil {
		// Distinguish missing vs inactive for a precise error.
		any, lerr := repository.FindStoreProductByCode(ctx, pool, productCode)
		if lerr == nil && any != nil {
			return nil, errors.New(409, "PRODUCT_INACTIVE", "Product is not available for redemption")
		}
		return nil, errors.New(404, "PRODUCT_NOT_FOUND", "Product not found")
	}

	code, err := publiccode.GenerateUnique(func(c string) (bool, error) {
		return repository.RedemptionCodeExists(ctx, pool, c)
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not allocate redemption code")
	}

	r, err := repository.InsertRedemption(ctx, tx, &model.Redemption{
		Code:         code,
		BotID:        botID,
		ProductID:    product.ID,
		ProductTitle: product.Title,
		CreditsCost:  product.CreditsPrice,
		RequestKey:   requestKey,
	})
	if err != nil {
		if stderrors.Is(err, repository.ErrRequestKeyConflict) {
			// Lost the race: DO NOT query on the aborted transaction.
			// Roll back and resolve the replay on the pool connection.
			_ = tx.Rollback(ctx)
			existing, ferr := repository.FindRedemptionByRequestKey(ctx, pool, botID, requestKey)
			if ferr != nil || existing == nil {
				return nil, errors.New(500, "INTERNAL_ERROR", "Could not resolve concurrent request key")
			}
			return resolveReplay(ctx, pool, existing, productCode)
		}
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not create redemption")
	}

	// Debit the credits inside the same transaction. This is the
	// authoritative balance check — insufficient funds abort everything.
	refType := "redemption"
	if _, err := credits.Record(ctx, pool, tx, botID, TxnTypeSpendRedemption,
		-product.CreditsPrice, &refType, &r.Code); err != nil {
		return nil, err // 402/404/500 — redemption + debit roll back together
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not commit redemption")
	}
	return &RedeemResult{Redemption: r, Created: true}, nil
}

// resolveReplay maps an existing (bot, request_key) redemption to the
// idempotent outcome: same product -> replay (Created=false), different
// product -> 409 IDEMPOTENCY_CONFLICT.
func resolveReplay(ctx context.Context, pool *pg.Pool, existing *model.Redemption, productCode string) (*RedeemResult, error) {
	requested, err := repository.FindStoreProductByCode(ctx, pool, productCode)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not resolve product")
	}
	if requested == nil || requested.ID != existing.ProductID {
		return nil, errors.New(409, "IDEMPOTENCY_CONFLICT",
			"Request key already used for a different product")
	}
	return &RedeemResult{Redemption: existing, Created: false}, nil
}

// -- review / cancel / fulfillment (all row-locked transitions) --

// TransitionResult reports the outcome of a state transition.
type TransitionResult struct {
	Redemption   *model.Redemption
	Transitioned bool // false = idempotent replay of the same end state
}

// ApproveRedemption: pending_review -> approved. No credit movement —
// the debit already happened at creation. Re-approving an approved
// redemption is an idempotent success; any other status is rejected.
func ApproveRedemption(ctx context.Context, pool *pg.Pool, code string, reviewNote string) (*TransitionResult, error) {
	var res *TransitionResult
	err := runInTx(ctx, pool, func(tx pgx.Tx) error {
		oc, err := ApproveRedemptionTx(ctx, tx, code, reviewNote)
		if err != nil {
			return err
		}
		res = &TransitionResult{Redemption: oc.After, Transitioned: oc.Transitioned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// RejectRedemption: pending_review -> rejected with an atomic refund:
//
//	BEGIN
//	lock redemption (FOR UPDATE)
//	  approved/fulfilled/cancelled -> 409 (rejected itself -> idempotent)
//	credits.Record(+cost, refund_redemption, ref redemption/<code>)  [same tx]
//	mark rejected (guarded UPDATE ... WHERE status = 'pending_review')
//	COMMIT
//
// Re-rejecting a rejected redemption returns idempotent success without a
// second refund (the WHERE guard makes the economic action unreachable).
func RejectRedemption(ctx context.Context, pool *pg.Pool, code string, reviewNote string) (*TransitionResult, error) {
	var res *TransitionResult
	err := runInTx(ctx, pool, func(tx pgx.Tx) error {
		oc, err := RejectRedemptionTx(ctx, pool, tx, code, reviewNote)
		if err != nil {
			return err
		}
		res = &TransitionResult{Redemption: oc.After, Transitioned: oc.Transitioned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CancelRedemption: pending_review -> cancelled, or approved -> cancelled
// (the failure exit so an approved order that cannot be fulfilled does not
// become a dead order). Atomic with refund_redemption. fulfilled/rejected
// cannot be cancelled; double cancel never refunds twice.
func CancelRedemption(ctx context.Context, pool *pg.Pool, code string) (*TransitionResult, error) {
	var res *TransitionResult
	err := runInTx(ctx, pool, func(tx pgx.Tx) error {
		oc, err := CancelRedemptionTx(ctx, pool, tx, code)
		if err != nil {
			return err
		}
		res = &TransitionResult{Redemption: oc.After, Transitioned: oc.Transitioned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// FulfillRedemption: approved -> fulfilled, recording the minimal virtual
// fulfillment fact (fulfillment_note + fulfilled_at). No economic action;
// double fulfill is an idempotent success. pending_review cannot be
// fulfilled directly.
func FulfillRedemption(ctx context.Context, pool *pg.Pool, code string, fulfillmentNote string) (*TransitionResult, error) {
	var res *TransitionResult
	err := runInTx(ctx, pool, func(tx pgx.Tx) error {
		oc, err := FulfillRedemptionTx(ctx, tx, code, fulfillmentNote)
		if err != nil {
			return err
		}
		res = &TransitionResult{Redemption: oc.After, Transitioned: oc.Transitioned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// applyTransition mirrors a committed transition onto the in-memory copy.
func applyTransition(r *model.Redemption, target string, note *string) {
	r.Status = target
	now := time.Now()
	switch target {
	case model.RedemptionStatusApproved, model.RedemptionStatusRejected:
		r.ReviewNote = note
		r.ReviewedAt = &now
	case model.RedemptionStatusFulfilled:
		r.FulfillmentNote = note
		r.FulfilledAt = &now
	case model.RedemptionStatusCancelled:
		r.CancelledAt = &now
	}
}

// GetRedemption returns a redemption by code (read-only, no lock).
func GetRedemption(ctx context.Context, pool *pg.Pool, code string) (*model.Redemption, error) {
	r, err := repository.FindRedemptionByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load redemption")
	}
	if r == nil {
		return nil, errors.New(404, "REDEMPTION_NOT_FOUND", "Redemption not found")
	}
	return r, nil
}

// GetRedemptionForBot is the ownership-scoped read for owner-facing
// surfaces: the query itself is filtered by bot_id, so a code owned by
// another bot is indistinguishable from a missing one (404).
func GetRedemptionForBot(ctx context.Context, pool *pg.Pool, botID int64, code string) (*model.Redemption, error) {
	r, err := repository.FindRedemptionByCodeForBot(ctx, pool, botID, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load redemption")
	}
	if r == nil {
		return nil, errors.New(404, "REDEMPTION_NOT_FOUND", "Redemption not found")
	}
	return r, nil
}
