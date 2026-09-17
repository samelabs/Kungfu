package admin

// B2 Store Administration control-plane orchestration.
//
// Pipeline: server handler → internal/admin (this file) →
// internal/store Tx primitives → repository → PostgreSQL. Credits
// mutations stay inside the Store primitives (store → credits); the
// Admin orchestration NEVER imports internal/credits and NEVER
// writes tb_store* SQL. Every privileged mutation runs inside ONE
// admin.WithAuditTx transaction together with its audit row.
//
// WithAuditTx exposes a pg.Querier; the Store Tx primitives need the
// underlying pgx.Tx. txOf is the private, fail-closed adapter (a
// plain type assertion that refuses to run on anything else).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/store"
)

// txOf extracts the pgx.Tx behind WithAuditTx's Querier. Fail-closed:
// a non-transaction Querier is a programming error and aborts.
func txOf(q pg.Querier) (pgx.Tx, error) {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil, errors.New(500, "INTERNAL_ERROR", "store administration requires the audit transaction")
	}
	return tx, nil
}

// -- product reads --

// ListStoreProducts requires store.products.read.
func ListStoreProducts(ctx context.Context, pool *pg.Pool, principal *Principal, f store.ProductListFilter) ([]model.StoreProduct, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "store.products.read"); err != nil {
		return nil, 0, err
	}
	return store.ListProductsForAdmin(ctx, pool, f)
}

// GetStoreProduct requires store.products.read; visible regardless
// of status (Admin sees inactive products too).
func GetStoreProduct(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*model.StoreProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "store.products.read"); err != nil {
		return nil, err
	}
	return store.GetProduct(ctx, pool, code)
}

// productFacts is the audit fact snapshot for a product row.
func productFacts(p *model.StoreProduct) map[string]interface{} {
	var desc interface{}
	if p.Description != nil && *p.Description != "" {
		desc = *p.Description
	}
	return map[string]interface{}{
		"code":          p.Code,
		"title":         p.Title,
		"description":   desc,
		"credits_price": p.CreditsPrice,
		"status":        p.Status,
	}
}

// -- product mutations --

// CreateStoreProduct requires store.products.manage; one transaction:
// Store insert + audit (action store.product.create, after = full
// snapshot of the new row).
func CreateStoreProduct(ctx context.Context, pool *pg.Pool, principal *Principal, in store.ProductInput) (*model.StoreProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "store.products.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "store.product.create",
		TargetType: "store_product",
		Success:    true,
	}
	var created *model.StoreProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := store.CreateProductTx(ctx, pool, tx, in)
		if err != nil {
			return err
		}
		created = oc.After
		entry.TargetID = oc.After.Code
		entry.After = productFacts(oc.After)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateStoreProduct requires store.products.manage; partial patch
// (title/description/credits_price), row locked against Redeem.
func UpdateStoreProduct(ctx context.Context, pool *pg.Pool, principal *Principal, code string, patch store.ProductPatch) (*model.StoreProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "store.products.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "store.product.update",
		TargetType: "store_product",
		TargetID:   code,
		Success:    true,
	}
	var updated *model.StoreProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := store.UpdateProductTx(ctx, tx, code, patch)
		if err != nil {
			return err
		}
		updated = oc.After
		entry.Before = productFacts(oc.Before)
		entry.After = productFacts(oc.After)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ActivateStoreProduct / DeactivateStoreProduct require
// store.products.manage; idempotent; snapshots never touched.
func SetStoreProductStatus(ctx context.Context, pool *pg.Pool, principal *Principal, code, status string) (*model.StoreProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "store.products.manage"); err != nil {
		return nil, err
	}
	action := "store.product.activate"
	if status == model.ProductStatusInactive {
		action = "store.product.deactivate"
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     action,
		TargetType: "store_product",
		TargetID:   code,
		Success:    true,
	}
	var updated *model.StoreProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := store.SetProductStatusTx(ctx, tx, code, status)
		if err != nil {
			return err
		}
		updated = oc.After
		entry.Before = productFacts(oc.Before)
		entry.After = productFacts(oc.After)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// -- redemption reads --

// ListStoreRedemptions requires store.redemptions.read; platform
// view (no ownership filtering).
func ListStoreRedemptions(ctx context.Context, pool *pg.Pool, principal *Principal, f store.RedemptionListFilter) ([]model.Redemption, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "store.redemptions.read"); err != nil {
		return nil, 0, err
	}
	return store.ListRedemptionsForAdmin(ctx, pool, f)
}

// GetStoreRedemption requires store.redemptions.read.
func GetStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*model.Redemption, error) {
	if err := RequirePermission(ctx, pool, principal, "store.redemptions.read"); err != nil {
		return nil, err
	}
	return store.GetRedemption(ctx, pool, code)
}

// redemptionFacts is the audit fact snapshot for a redemption row.
func redemptionFacts(r *model.Redemption) map[string]interface{} {
	facts := map[string]interface{}{
		"code":          r.Code,
		"bot_id":        r.BotID,
		"product_id":    r.ProductID,
		"product_title": r.ProductTitle,
		"credits_cost":  r.CreditsCost,
		"status":        r.Status,
		"reviewed_at":   nullableTime(r.ReviewedAt),
		"fulfilled_at":  nullableTime(r.FulfilledAt),
		"cancelled_at":  nullableTime(r.CancelledAt),
	}
	facts["review_note"] = auditStr(r.ReviewNote)
	facts["fulfillment_note"] = auditStr(r.FulfillmentNote)
	return facts
}

func auditStr(s *string) interface{} {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullableTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}

// -- redemption mutations --

// transitionStoreRedemption runs one redemption transition inside a
// single WithAuditTx transaction: Store state machine (+ Credits
// refund for reject/cancel) + audit with the transaction-local
// before/after and a transitioned metadata flag.
func transitionStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal,
	action, code string, run func(ctx context.Context, tx pgx.Tx) (*store.TransitionOutcome, error)) (*store.TransitionOutcome, error) {

	if err := RequirePermission(ctx, pool, principal, "store.redemptions.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     action,
		TargetType: "store_redemption",
		TargetID:   code,
		Success:    true,
	}
	var outcome *store.TransitionOutcome
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := run(ctx, tx)
		if err != nil {
			return err
		}
		outcome = oc
		entry.Before = redemptionFacts(oc.Before)
		entry.After = redemptionFacts(oc.After)
		entry.Metadata = map[string]interface{}{"transitioned": oc.Transitioned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// ApproveStoreRedemption: pending_review → approved.
func ApproveStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, reviewNote string) (*store.TransitionOutcome, error) {
	return transitionStoreRedemption(ctx, pool, principal, "store.redemption.approve", code,
		func(ctx context.Context, tx pgx.Tx) (*store.TransitionOutcome, error) {
			return store.ApproveRedemptionTx(ctx, tx, code, reviewNote)
		})
}

// RejectStoreRedemption: pending_review → rejected + refund, same tx.
func RejectStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, reviewNote string) (*store.TransitionOutcome, error) {
	return transitionStoreRedemption(ctx, pool, principal, "store.redemption.reject", code,
		func(ctx context.Context, tx pgx.Tx) (*store.TransitionOutcome, error) {
			return store.RejectRedemptionTx(ctx, pool, tx, code, reviewNote)
		})
}

// FulfillStoreRedemption: approved → fulfilled.
func FulfillStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, fulfillmentNote string) (*store.TransitionOutcome, error) {
	return transitionStoreRedemption(ctx, pool, principal, "store.redemption.fulfill", code,
		func(ctx context.Context, tx pgx.Tx) (*store.TransitionOutcome, error) {
			return store.FulfillRedemptionTx(ctx, tx, code, fulfillmentNote)
		})
}

// CancelStoreRedemption: pending_review|approved → cancelled + refund.
func CancelStoreRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*store.TransitionOutcome, error) {
	return transitionStoreRedemption(ctx, pool, principal, "store.redemption.cancel", code,
		func(ctx context.Context, tx pgx.Tx) (*store.TransitionOutcome, error) {
			return store.CancelRedemptionTx(ctx, pool, tx, code)
		})
}

// -- request type re-exports: handlers must not import internal/store
// (server → admin → domain pipeline). These aliases are the only
// shapes the HTTP layer needs.

// StoreProductInput mirrors store.ProductInput for handlers.
type StoreProductInput = store.ProductInput

// StoreProductPatch mirrors store.ProductPatch (partial update).
type StoreProductPatch = store.ProductPatch

// StoreProductListFilter mirrors store.ProductListFilter.
type StoreProductListFilter = store.ProductListFilter

// StoreRedemptionListFilter mirrors store.RedemptionListFilter.
type StoreRedemptionListFilter = store.RedemptionListFilter

// StoreProduct is the product row type for handler serialization.
type StoreProduct = model.StoreProduct

// Redemption is the redemption row type for handler serialization.
type Redemption = model.Redemption
