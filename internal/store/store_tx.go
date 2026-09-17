package store

// B2 Store Administration: caller-transaction (Tx) variants of every
// Admin-triggered Store mutation, plus the Admin-facing read queries.
//
// Invariants:
//   - Tx primitives accept a caller-owned pgx.Tx; they NEVER begin,
//     commit, or roll back the transaction and NEVER write Admin
//     audit — the Admin control plane (internal/admin/store.go) owns
//     the transaction via admin.WithAuditTx and the audit row.
//   - There is exactly ONE state-machine implementation per operation:
//     the legacy public wrappers (owner/internal surfaces) now begin
//     their own transaction and delegate to the SAME Tx primitive.
//   - Credits mutations still go exclusively through credits.Record
//     on the same transaction (spend_redemption / refund_redemption).
//   - Every Tx mutation returns the authoritative BEFORE and AFTER
//     rows read inside the transaction, so the Admin audit records
//     real transaction-local state (never client-side guesses).

import (
	"context"
	stderrors "errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
	"kungfu.md/internal/repository"
)

// TransitionOutcome carries the transaction-local authoritative
// before/after for audit: Before is the locked pre-state, After is
// the real post-mutation row (re-read in the same transaction).
type TransitionOutcome struct {
	Before       *model.Redemption
	After        *model.Redemption
	Transitioned bool // false = idempotent replay (before == after)
}

// ProductOutcome is the product analogue of TransitionOutcome.
type ProductOutcome struct {
	Before *model.StoreProduct // nil on create
	After  *model.StoreProduct
}

// ProductPatch carries OPTIONAL editable fields (partial update).
// nil = preserve; non-nil = set (description "" clears).
type ProductPatch struct {
	Title        *string
	Description  *string
	CreditsPrice *float64
}

// -- product Tx primitives --

// CreateProductTx inserts a new active product on the caller's
// transaction. The public code is allocated by publiccode (code
// uniqueness probed on the pool — safe: INSERT constraint is the
// authority). Returns before=nil, after=fresh row.
func CreateProductTx(ctx context.Context, pool *pg.Pool, tx pgx.Tx, in ProductInput) (*ProductOutcome, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" || utf8.RuneCountInString(title) > 128 {
		return nil, errors.New(400, "INVALID_TITLE", "Title must be 1-128 chars")
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return nil, errors.New(400, "INVALID_DESCRIPTION", "Description must be at most 500 chars")
	}
	if math.IsNaN(in.CreditsPrice) || math.IsInf(in.CreditsPrice, 0) {
		return nil, errors.New(400, "INVALID_PRICE", "Credits price must be a finite number")
	}
	if in.CreditsPrice <= 0 {
		return nil, errors.New(400, "INVALID_PRICE", "Credits price must be greater than zero")
	}

	var desc *string
	if in.Description != "" {
		desc = &in.Description
	}

	code, err := publiccode.GenerateUnique(func(c string) (bool, error) {
		return repository.StoreProductCodeExists(ctx, pool, c)
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not allocate product code")
	}

	p, err := repository.CreateStoreProduct(ctx, tx, code, repository.StoreProductInput{
		Title: title, Description: desc, CreditsPrice: in.CreditsPrice,
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not create product")
	}
	return &ProductOutcome{Before: nil, After: p}, nil
}

// UpdateProductTx applies a PARTIAL edit to a product on the caller's
// transaction. The product row is locked FOR UPDATE first — this is
// the same lock Redeem takes, so a concurrent edit and a Redeem
// serialize and the redemption snapshot (title + price) comes wholly
// from the old or the new row, never mixed. code/id/created_at are
// immutable; status is NOT settable here (dedicated activate/
// deactivate paths). At least one editable field must be provided.
func UpdateProductTx(ctx context.Context, tx pgx.Tx, code string, patch ProductPatch) (*ProductOutcome, error) {
	if patch.Title == nil && patch.Description == nil && patch.CreditsPrice == nil {
		return nil, errors.New(400, "EMPTY_PATCH", "PATCH must include at least one of title, description, credits_price")
	}
	var nextTitle string
	if patch.Title != nil {
		nextTitle = strings.TrimSpace(*patch.Title)
		if nextTitle == "" || utf8.RuneCountInString(nextTitle) > 128 {
			return nil, errors.New(400, "INVALID_TITLE", "Title must be 1-128 chars")
		}
	}
	if patch.Description != nil && utf8.RuneCountInString(*patch.Description) > 500 {
		return nil, errors.New(400, "INVALID_DESCRIPTION", "Description must be at most 500 chars")
	}
	if patch.CreditsPrice != nil {
		if math.IsNaN(*patch.CreditsPrice) || math.IsInf(*patch.CreditsPrice, 0) {
			return nil, errors.New(400, "INVALID_PRICE", "Credits price must be a finite number")
		}
		if *patch.CreditsPrice <= 0 {
			return nil, errors.New(400, "INVALID_PRICE", "Credits price must be greater than zero")
		}
	}

	before, err := repository.LockStoreProductByCode(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load product")
	}
	if before == nil {
		return nil, errors.New(404, "PRODUCT_NOT_FOUND", "Product not found")
	}

	next := *before
	if patch.Title != nil {
		next.Title = nextTitle
	}
	if patch.Description != nil {
		if *patch.Description == "" {
			next.Description = nil
		} else {
			d := *patch.Description
			next.Description = &d
		}
	}
	if patch.CreditsPrice != nil {
		next.CreditsPrice = *patch.CreditsPrice
	}

	after, err := repository.UpdateStoreProduct(ctx, tx, before.ID, next.Title, next.Description, next.CreditsPrice)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not update product")
	}
	return &ProductOutcome{Before: before, After: after}, nil
}

// SetProductStatusTx flips active <-> inactive on the caller's
// transaction (row locked). Idempotent: setting the current status is
// a successful no-op with Before == After. Products are never
// deleted; redemption snapshots are never touched.
func SetProductStatusTx(ctx context.Context, tx pgx.Tx, code, status string) (*ProductOutcome, error) {
	if status != model.ProductStatusActive && status != model.ProductStatusInactive {
		return nil, errors.New(400, "INVALID_PRODUCT_STATUS", "Product status must be active or inactive")
	}
	before, err := repository.LockStoreProductByCode(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load product")
	}
	if before == nil {
		return nil, errors.New(404, "PRODUCT_NOT_FOUND", "Product not found")
	}
	if before.Status == status {
		return &ProductOutcome{Before: before, After: before}, nil // idempotent
	}
	after, err := repository.SetStoreProductStatusReturning(ctx, tx, before.ID, status)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not update product status")
	}
	return &ProductOutcome{Before: before, After: after}, nil
}

// -- redemption Tx primitives (single state machine, shared with the
// existing public wrappers) --

// ApproveRedemptionTx: pending_review -> approved. No credit
// movement. Idempotent when already approved; any other status 409.
func ApproveRedemptionTx(ctx context.Context, tx pgx.Tx, code, reviewNote string) (*TransitionOutcome, error) {
	if utf8.RuneCountInString(reviewNote) > 500 {
		return nil, errors.New(400, "INVALID_REVIEW_NOTE", "Review note must be at most 500 chars")
	}
	return lockTransitionTx(ctx, tx, code, reviewNote, model.RedemptionStatusApproved, false)
}

// RejectRedemptionTx: pending_review -> rejected + refund_redemption
// in the same transaction. Idempotent when already rejected (no
// second refund); approved/fulfilled/cancelled -> 409.
func RejectRedemptionTx(ctx context.Context, pool *pg.Pool, tx pgx.Tx, code, reviewNote string) (*TransitionOutcome, error) {
	if utf8.RuneCountInString(reviewNote) > 500 {
		return nil, errors.New(400, "INVALID_REVIEW_NOTE", "Review note must be at most 500 chars")
	}
	return refundTransitionTx(ctx, pool, tx, code, reviewNote, model.RedemptionStatusRejected)
}

// FulfillRedemptionTx: approved -> fulfilled. No economic action.
// Idempotent; pending_review -> 409.
func FulfillRedemptionTx(ctx context.Context, tx pgx.Tx, code, fulfillmentNote string) (*TransitionOutcome, error) {
	if utf8.RuneCountInString(fulfillmentNote) > 500 {
		return nil, errors.New(400, "INVALID_FULFILLMENT_NOTE", "Fulfillment note must be at most 500 chars")
	}
	return lockTransitionTx(ctx, tx, code, fulfillmentNote, model.RedemptionStatusFulfilled, true)
}

// CancelRedemptionTx: pending_review|approved -> cancelled + refund
// in the same transaction. fulfilled/rejected -> 409. Never rewrites
// review_note/reviewed_at.
func CancelRedemptionTx(ctx context.Context, pool *pg.Pool, tx pgx.Tx, code string) (*TransitionOutcome, error) {
	return refundTransitionTx(ctx, pool, tx, code, "", model.RedemptionStatusCancelled)
}

// lockTransitionTx is the no-refund transition core (approve/fulfill)
// on a caller-owned transaction: lock, verify legal edge, guarded
// UPDATE, re-read after.
func lockTransitionTx(ctx context.Context, tx pgx.Tx, code, note, target string, requireApprovedOnly bool) (*TransitionOutcome, error) {
	from := model.RedemptionStatusPendingReview
	if requireApprovedOnly {
		from = model.RedemptionStatusApproved
	}

	r, err := repository.LockRedemptionByCode(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load redemption")
	}
	if r == nil {
		return nil, errors.New(404, "REDEMPTION_NOT_FOUND", "Redemption not found")
	}

	if r.Status == target {
		return &TransitionOutcome{Before: r, After: r, Transitioned: false}, nil
	}
	if r.Status != from {
		return nil, errors.New(409, "INVALID_REDEMPTION_STATE",
			"Redemption is "+r.Status+" and cannot become "+target)
	}

	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	ok, err := repository.UpdateRedemptionStatus(ctx, tx, r.ID, from, target, notePtr)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not update redemption")
	}
	if !ok {
		return nil, errors.New(409, "INVALID_REDEMPTION_STATE", "Concurrent state change")
	}

	after, err := repository.FindRedemptionByID(ctx, tx, r.ID)
	if err != nil || after == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not re-read redemption")
	}
	return &TransitionOutcome{Before: r, After: after, Transitioned: true}, nil
}

// refundTransitionTx is the refunding transition core (reject/cancel)
// on a caller-owned transaction: lock, verify legal edge(s), refund
// via credits.Record on the SAME transaction, guarded UPDATE, re-read.
func refundTransitionTx(ctx context.Context, pool *pg.Pool, tx pgx.Tx, code, note, target string) (*TransitionOutcome, error) {
	legalFrom := map[string]bool{
		model.RedemptionStatusPendingReview: true,
	}
	if target == model.RedemptionStatusCancelled {
		legalFrom[model.RedemptionStatusApproved] = true
	}

	r, err := repository.LockRedemptionByCode(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load redemption")
	}
	if r == nil {
		return nil, errors.New(404, "REDEMPTION_NOT_FOUND", "Redemption not found")
	}

	if r.Status == target {
		return &TransitionOutcome{Before: r, After: r, Transitioned: false}, nil
	}
	if !legalFrom[r.Status] {
		return nil, errors.New(409, "INVALID_REDEMPTION_STATE",
			"Redemption is "+r.Status+" and cannot become "+target)
	}

	// Refund inside the caller's transaction — the Admin audit INSERT
	// later in the same transaction makes "refund committed but audit
	// missing" impossible.
	refType := "redemption"
	if _, err := credits.Record(ctx, pool, tx, r.BotID, TxnTypeRefundRedemption,
		r.CreditsCost, &refType, &r.Code); err != nil {
		return nil, err
	}

	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	ok, err := repository.UpdateRedemptionStatus(ctx, tx, r.ID, r.Status, target, notePtr)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not update redemption")
	}
	if !ok {
		return nil, errors.New(409, "INVALID_REDEMPTION_STATE", "Concurrent state change")
	}

	after, err := repository.FindRedemptionByID(ctx, tx, r.ID)
	if err != nil || after == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not re-read redemption")
	}
	return &TransitionOutcome{Before: r, After: after, Transitioned: true}, nil
}

var _ = stderrors.Is // parity with legacy file's imports when refactored
