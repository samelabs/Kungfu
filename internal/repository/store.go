package repository

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// Store persistence for tb_store_products and tb_redemptions.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and
// pgx.Tx; review/fulfillment/cancel transitions always run inside the
// store service's transaction.

// ErrProductCodeExists / ErrRequestKeyExists are uniqueness outcomes the
// service translates into business errors.
var (
	ErrProductCodeExists  = stderrors.New("product code exists")
	ErrRequestKeyConflict = stderrors.New("request key conflict")
)

// -- catalog --

// StoreProductInput is the validated payload for creating a product.
type StoreProductInput struct {
	Title        string
	Description  *string
	CreditsPrice float64
}

// CreateStoreProduct inserts an active product row.
func CreateStoreProduct(ctx context.Context, q pg.Querier, code string, in StoreProductInput) (*model.StoreProduct, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO tb_store_products (code, title, description, credits_price, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'active', NOW(), NOW())
		RETURNING id, code, title, description, credits_price, status, created_at, updated_at`,
		code, in.Title, in.Description, in.CreditsPrice)
	return scanStoreProduct(row)
}

// SetStoreProductStatus flips active <-> inactive. Products are never
// deleted, so historical redemptions keep their FK.
func SetStoreProductStatus(ctx context.Context, q pg.Querier, code, status string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_store_products SET status = $2, updated_at = NOW()
		WHERE code = $1`, code, status)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FindStoreProductByCode returns a product by code, or nil when absent.
func FindStoreProductByCode(ctx context.Context, q pg.Querier, code string) (*model.StoreProduct, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, title, description, credits_price, status, created_at, updated_at
		FROM tb_store_products WHERE code = $1`, code)
	p, err := scanStoreProduct(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// LockActiveStoreProductByCode locks the product row (FOR UPDATE) inside
// the redemption transaction: the snapshot (title + price) is taken from
// the locked row, so a concurrent price change cannot interleave.
// Returns nil (no error) when the code does not exist.
func LockActiveStoreProductByCode(ctx context.Context, tx pgx.Tx, code string) (*model.StoreProduct, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, code, title, description, credits_price, status, created_at, updated_at
		FROM tb_store_products
		WHERE code = $1 AND status = 'active'
		FOR UPDATE`, code)
	p, err := scanStoreProduct(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// ListActiveStoreProducts returns the active catalog, newest first.
func ListActiveStoreProducts(ctx context.Context, q pg.Querier) ([]model.StoreProduct, error) {
	rows, err := q.Query(ctx, `
		SELECT id, code, title, description, credits_price, status, created_at, updated_at
		FROM tb_store_products
		WHERE status = 'active'
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.StoreProduct
	for rows.Next() {
		var it model.StoreProduct
		if err := rows.Scan(&it.ID, &it.Code, &it.Title, &it.Description,
			&it.CreditsPrice, &it.Status, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// StoreProductCodeExists reports whether a code is already used.
func StoreProductCodeExists(ctx context.Context, q pg.Querier, code string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tb_store_products WHERE code = $1)`, code).Scan(&exists)
	return exists, err
}

// -- redemptions --

// InsertRedemption inserts a pending_review redemption with its snapshots.
// On (bot_id, request_key) uniqueness violation it returns
// ErrRequestKeyConflict; on any other constraint violation a plain error.
// Callers run this inside the redemption transaction; PG keeps the
// transaction usable (the INSERT is the only statement on this path that
// can conflict, and the caller aborts on error).
func InsertRedemption(ctx context.Context, tx pgx.Tx, r *model.Redemption) (*model.Redemption, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO tb_redemptions (code, bot_id, product_id, product_title,
		                            credits_cost, request_key, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending_review', NOW(), NOW())
		RETURNING id, status, created_at, updated_at, reviewed_at, fulfilled_at, cancelled_at`,
		r.Code, r.BotID, r.ProductID, r.ProductTitle, r.CreditsCost, r.RequestKey)

	out := *r
	err := row.Scan(&out.ID, &out.Status, &out.CreatedAt, &out.UpdatedAt,
		&out.ReviewedAt, &out.FulfilledAt, &out.CancelledAt)
	if err != nil {
		return nil, classifyInsertError(err)
	}
	return &out, nil
}

// FindRedemptionByRequestKey returns the existing redemption for
// (bot_id, request_key), or nil when absent. Used BEFORE the insert, on
// the pool connection (not the aborted tx), to resolve idempotent replays
// and product conflicts.
func FindRedemptionByRequestKey(ctx context.Context, q pg.Querier, botID int64, requestKey string) (*model.Redemption, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, product_id, product_title, credits_cost,
		       request_key, status, review_note, fulfillment_note,
		       created_at, updated_at, reviewed_at, fulfilled_at, cancelled_at
		FROM tb_redemptions
		WHERE bot_id = $1 AND request_key = $2`, botID, requestKey)
	r, err := scanRedemption(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// FindRedemptionByCode returns a redemption by its public code, or nil
// when absent (no lock — read-only path).
func FindRedemptionByCode(ctx context.Context, q pg.Querier, code string) (*model.Redemption, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, product_id, product_title, credits_cost,
		       request_key, status, review_note, fulfillment_note,
		       created_at, updated_at, reviewed_at, fulfilled_at, cancelled_at
		FROM tb_redemptions
		WHERE code = $1`, code)
	r, err := scanRedemption(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// LockRedemptionByCode locks the redemption row (FOR UPDATE) inside the
// caller's transaction — the serialization point of every state
// transition (approve / reject / cancel / fulfill). Returns nil when the
// code does not exist.
func LockRedemptionByCode(ctx context.Context, tx pgx.Tx, code string) (*model.Redemption, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, code, bot_id, product_id, product_title, credits_cost,
		       request_key, status, review_note, fulfillment_note,
		       created_at, updated_at, reviewed_at, fulfilled_at, cancelled_at
		FROM tb_redemptions
		WHERE code = $1
		FOR UPDATE`, code)
	r, err := scanRedemption(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// UpdateRedemptionStatus performs a guarded state transition inside the
// caller's transaction. The WHERE clause enforces the legal edge
// (fromStatus -> toStatus); the row is already locked by the caller.
// Returns false when the current status did not match fromStatus.
func UpdateRedemptionStatus(ctx context.Context, tx pgx.Tx, id int64,
	fromStatus, toStatus string, note *string) (bool, error) {

	var tag pgconn.CommandTag
	var err error
	switch toStatus {
	case model.RedemptionStatusApproved, model.RedemptionStatusRejected:
		tag, err = tx.Exec(ctx, `
			UPDATE tb_redemptions
			SET status = $2, review_note = $3, reviewed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = $4`,
			id, toStatus, note, fromStatus)
	case model.RedemptionStatusFulfilled:
		tag, err = tx.Exec(ctx, `
			UPDATE tb_redemptions
			SET status = $2, fulfillment_note = $3, fulfilled_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = $4`,
			id, toStatus, note, fromStatus)
	case model.RedemptionStatusCancelled:
		// Cancellation must NOT touch review_note / reviewed_at: the
		// review fact is history and survives the cancel (an approved
		// redemption keeps its approval note). Only status/cancelled_at/
		// updated_at change; the note argument is deliberately unused.
		tag, err = tx.Exec(ctx, `
			UPDATE tb_redemptions
			SET status = $2, cancelled_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = $3`,
			id, toStatus, fromStatus)
	default:
		return false, stderrors.New("unsupported redemption target status: " + toStatus)
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RedemptionCodeExists reports whether a code is already used.
func RedemptionCodeExists(ctx context.Context, q pg.Querier, code string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tb_redemptions WHERE code = $1)`, code).Scan(&exists)
	return exists, err
}

// -- scan helpers --

func scanStoreProduct(row pgx.Row) (*model.StoreProduct, error) {
	var p model.StoreProduct
	err := row.Scan(&p.ID, &p.Code, &p.Title, &p.Description,
		&p.CreditsPrice, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func scanRedemption(row pgx.Row) (*model.Redemption, error) {
	var r model.Redemption
	err := row.Scan(&r.ID, &r.Code, &r.BotID, &r.ProductID, &r.ProductTitle,
		&r.CreditsCost, &r.RequestKey, &r.Status, &r.ReviewNote, &r.FulfillmentNote,
		&r.CreatedAt, &r.UpdatedAt, &r.ReviewedAt, &r.FulfilledAt, &r.CancelledAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// classifyInsertError maps a uniqueness violation on tb_redemptions to
// ErrRequestKeyConflict (the only UNIQUE constraint a caller can retry on).
func classifyInsertError(err error) error {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) && pgErr.Code == "23505" {
		if strings.Contains(pgErr.ConstraintName, "uk_redemption_bot_request") {
			return ErrRequestKeyConflict
		}
		if pgErr.ConstraintName == "uk_store_product_code" {
			return ErrProductCodeExists
		}
	}
	return err
}
