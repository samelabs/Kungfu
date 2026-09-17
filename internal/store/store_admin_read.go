package store

// B2 Admin-facing Store reads. The Admin is a PLATFORM subject (not a
// bot), so these global reads do no owner bot ownership filtering and
// never attach a bot identity to the Admin — they merely return rows
// whose bot_id column is an operational fact of the redemption.

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// ProductListFilter carries the Admin product-list query.
type ProductListFilter struct {
	Status   string // all | active | inactive ("" = all)
	Q        string // searches code + title
	Page     int
	PageSize int
}

// ListProductsForAdmin returns the FULL catalog (active + inactive)
// with pagination and search, stably ordered created_at DESC, id DESC.
func ListProductsForAdmin(ctx context.Context, pool *pg.Pool, f ProductListFilter) ([]model.StoreProduct, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 50
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	switch f.Status {
	case "", "all", model.ProductStatusActive, model.ProductStatusInactive:
	default:
		return nil, 0, errors.New(400, "INVALID_STATUS_FILTER", "status must be all, active or inactive")
	}
	items, total, err := repository.AdminListStoreProducts(ctx, pool, repository.AdminProductFilter{
		Status: f.Status, Q: f.Q, Page: f.Page, PageSize: f.PageSize,
	})
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Could not load products")
	}
	return items, total, nil
}

// RedemptionListFilter carries the Admin redemption-list query.
type RedemptionListFilter struct {
	Status   string // all | pending_review | approved | rejected | fulfilled | cancelled
	BotID    int64  // 0 = no filter
	Q        string // searches code + product_title + request_key
	Page     int
	PageSize int
}

// ListRedemptionsForAdmin returns ALL redemptions with filters and
// pagination, stably ordered created_at DESC, id DESC.
func ListRedemptionsForAdmin(ctx context.Context, pool *pg.Pool, f RedemptionListFilter) ([]model.Redemption, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 50
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	switch f.Status {
	case "", "all",
		model.RedemptionStatusPendingReview, model.RedemptionStatusApproved,
		model.RedemptionStatusRejected, model.RedemptionStatusFulfilled,
		model.RedemptionStatusCancelled:
	default:
		return nil, 0, errors.New(400, "INVALID_STATUS_FILTER", "invalid redemption status filter")
	}
	if f.BotID < 0 {
		return nil, 0, errors.New(400, "INVALID_BOT_ID", "bot_id must be a positive integer")
	}
	items, total, err := repository.AdminListRedemptions(ctx, pool, repository.AdminRedemptionFilter{
		Status: f.Status, BotID: f.BotID, Q: f.Q, Page: f.Page, PageSize: f.PageSize,
	})
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Could not load redemptions")
	}
	return items, total, nil
}
