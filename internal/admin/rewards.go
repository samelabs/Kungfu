package admin

// Rewards Administration control-plane orchestration.
//
// Pipeline: server handler → internal/admin (this file) →
// internal/rewards Tx primitives → repository → PostgreSQL. Credits
// mutations stay inside the Rewards primitives (rewards → credits); the
// Admin orchestration NEVER imports internal/credits and NEVER
// writes tb_store* SQL. Every privileged mutation runs inside ONE
// admin.WithAuditTx transaction together with its audit row.
//
// WithAuditTx exposes a pg.Querier; the Rewards Tx primitives need the
// underlying pgx.Tx. txOf is the private, fail-closed adapter (a
// plain type assertion that refuses to run on anything else).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/rewards"
)

// txOf extracts the pgx.Tx behind WithAuditTx's Querier. Fail-closed:
// a non-transaction Querier is a programming error and aborts.
func txOf(q pg.Querier) (pgx.Tx, error) {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil, errors.New(500, "INTERNAL_ERROR", "rewards administration requires the audit transaction")
	}
	return tx, nil
}

// -- product reads --

// ListRewardsProducts requires rewards.products.read.
func ListRewardsProducts(ctx context.Context, pool *pg.Pool, principal *Principal, f rewards.ProductListFilter) ([]model.RewardsProduct, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.products.read"); err != nil {
		return nil, 0, err
	}
	return rewards.ListProductsForAdmin(ctx, pool, f)
}

// GetRewardsProduct requires rewards.products.read; visible regardless
// of status (Admin sees inactive products too).
func GetRewardsProduct(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*model.RewardsProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.products.read"); err != nil {
		return nil, err
	}
	return rewards.GetProduct(ctx, pool, code)
}

// productFacts is the audit fact snapshot for a product row.
func productFacts(p *model.RewardsProduct) map[string]interface{} {
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

// CreateRewardsProduct requires rewards.products.manage; one transaction:
// Rewards insert + audit (action rewards.product.create, after = full
// snapshot of the new row).
func CreateRewardsProduct(ctx context.Context, pool *pg.Pool, principal *Principal, in rewards.ProductInput) (*model.RewardsProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.products.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "rewards.product.create",
		TargetType: "store_product",
		Success:    true,
	}
	var created *model.RewardsProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := rewards.CreateProductTx(ctx, pool, tx, in)
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

// UpdateRewardsProduct requires rewards.products.manage; partial patch
// (title/description/credits_price), row locked against Redeem.
func UpdateRewardsProduct(ctx context.Context, pool *pg.Pool, principal *Principal, code string, patch rewards.ProductPatch) (*model.RewardsProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.products.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "rewards.product.update",
		TargetType: "store_product",
		TargetID:   code,
		Success:    true,
	}
	var updated *model.RewardsProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := rewards.UpdateProductTx(ctx, tx, code, patch)
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

// ActivateRewardsProduct / DeactivateRewardsProduct require
// rewards.products.manage; idempotent; snapshots never touched.
func SetRewardsProductStatus(ctx context.Context, pool *pg.Pool, principal *Principal, code, status string) (*model.RewardsProduct, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.products.manage"); err != nil {
		return nil, err
	}
	action := "rewards.product.activate"
	if status == model.ProductStatusInactive {
		action = "rewards.product.deactivate"
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     action,
		TargetType: "store_product",
		TargetID:   code,
		Success:    true,
	}
	var updated *model.RewardsProduct
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, q pg.Querier) error {
		tx, err := txOf(q)
		if err != nil {
			return err
		}
		oc, err := rewards.SetProductStatusTx(ctx, tx, code, status)
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

// ListRewardsRedemptions requires rewards.redemptions.read; platform
// view (no ownership filtering).
func ListRewardsRedemptions(ctx context.Context, pool *pg.Pool, principal *Principal, f rewards.RedemptionListFilter) ([]model.Redemption, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.redemptions.read"); err != nil {
		return nil, 0, err
	}
	return rewards.ListRedemptionsForAdmin(ctx, pool, f)
}

// GetRewardsRedemption requires rewards.redemptions.read.
func GetRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*model.Redemption, error) {
	if err := RequirePermission(ctx, pool, principal, "rewards.redemptions.read"); err != nil {
		return nil, err
	}
	return rewards.GetRedemption(ctx, pool, code)
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

// transitionRewardsRedemption runs one redemption transition inside a
// single WithAuditTx transaction: Rewards state machine (+ Credits
// refund for reject/cancel) + audit with the transaction-local
// before/after and a transitioned metadata flag.
func transitionRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal,
	action, code string, run func(ctx context.Context, tx pgx.Tx) (*rewards.TransitionOutcome, error)) (*rewards.TransitionOutcome, error) {

	if err := RequirePermission(ctx, pool, principal, "rewards.redemptions.manage"); err != nil {
		return nil, err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     action,
		TargetType: "store_redemption",
		TargetID:   code,
		Success:    true,
	}
	var outcome *rewards.TransitionOutcome
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

// ApproveRewardsRedemption: pending_review → approved.
func ApproveRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, reviewNote string) (*rewards.TransitionOutcome, error) {
	return transitionRewardsRedemption(ctx, pool, principal, "rewards.redemption.approve", code,
		func(ctx context.Context, tx pgx.Tx) (*rewards.TransitionOutcome, error) {
			return rewards.ApproveRedemptionTx(ctx, tx, code, reviewNote)
		})
}

// RejectRewardsRedemption: pending_review → rejected + refund, same tx.
func RejectRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, reviewNote string) (*rewards.TransitionOutcome, error) {
	return transitionRewardsRedemption(ctx, pool, principal, "rewards.redemption.reject", code,
		func(ctx context.Context, tx pgx.Tx) (*rewards.TransitionOutcome, error) {
			return rewards.RejectRedemptionTx(ctx, pool, tx, code, reviewNote)
		})
}

// FulfillRewardsRedemption: approved → fulfilled.
func FulfillRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code, fulfillmentNote string) (*rewards.TransitionOutcome, error) {
	return transitionRewardsRedemption(ctx, pool, principal, "rewards.redemption.fulfill", code,
		func(ctx context.Context, tx pgx.Tx) (*rewards.TransitionOutcome, error) {
			return rewards.FulfillRedemptionTx(ctx, tx, code, fulfillmentNote)
		})
}

// CancelRewardsRedemption: pending_review|approved → cancelled + refund.
func CancelRewardsRedemption(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*rewards.TransitionOutcome, error) {
	return transitionRewardsRedemption(ctx, pool, principal, "rewards.redemption.cancel", code,
		func(ctx context.Context, tx pgx.Tx) (*rewards.TransitionOutcome, error) {
			return rewards.CancelRedemptionTx(ctx, pool, tx, code)
		})
}

// -- request type re-exports: handlers must not import internal/rewards
// (server → admin → domain pipeline). These aliases are the only
// shapes the HTTP layer needs.

// RewardsProductInput mirrors rewards.ProductInput for handlers.
type RewardsProductInput = rewards.ProductInput

// RewardsProductPatch mirrors rewards.ProductPatch (partial update).
type RewardsProductPatch = rewards.ProductPatch

// RewardsProductListFilter mirrors rewards.ProductListFilter.
type RewardsProductListFilter = rewards.ProductListFilter

// RewardsRedemptionListFilter mirrors rewards.RedemptionListFilter.
type RewardsRedemptionListFilter = rewards.RedemptionListFilter

// RewardsProduct is the product row type for handler serialization.
type RewardsProduct = model.RewardsProduct

// Redemption is the redemption row type for handler serialization.
type Redemption = model.Redemption

// RewardsTransitionOutcome mirrors rewards.TransitionOutcome for handlers.
type RewardsTransitionOutcome = rewards.TransitionOutcome
