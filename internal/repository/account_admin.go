package repository

// Platform Account Administration repository primitives (011).
//
// Dedicated read/write primitives for the Admin Accounts surface
// (tb_bots). internal/admin never writes raw SQL for this domain —
// it goes through these functions only.
//
// Read projections are explicit column allowlists: they NEVER select
// password_hash, api_key_hash, or any raw credential material. The
// masked Agent key facts (api_key_last4, key_issued_at) are the only
// key metadata exposed.
//
// The balance column IS the authoritative Credits balance column —
// the same single source of truth internal/credits.Balance reads.
// Selecting it here is a projection, NOT a second balance
// calculation; nothing in this file computes or mutates balances.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// AdminAccountStatus constants — the ONLY two statuses the Admin
// Accounts surface may set. Existing rows carry the same vocabulary.
const (
	AdminAccountStatusActive   = "active"
	AdminAccountStatusDisabled = "disabled"
)

// AdminAccount is the Admin operational projection of a tb_bots row.
// It carries no credential material by construction.
type AdminAccount struct {
	ID          int64
	BotName     string
	Status      string
	Balance     int64 // authoritative Credits balance column, read-only projection
	APIKeyLast4 string
	KeyIssuedAt *time.Time
	LastActive  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AdminAccountDetail adds the existing light aggregates for the
// detail view. The two task facts are DIFFERENT business facts and
// are never merged:
//   - PublishedTaskCount: tb_tasks rows created by this bot
//   - SubmissionCount:    tb_task_submissions rows submitted by this bot
//   - KungfuCount:        tb_kungfus memory/kungfu rows owned by this bot
type AdminAccountDetail struct {
	AdminAccount
	PublishedTaskCount int64
	SubmissionCount    int64
	KungfuCount        int64
}

// AdminAccountFilter carries the account list parameters.
type AdminAccountFilter struct {
	Status   string // "all" | "active" | "disabled"
	Q        string // bot_name substring search
	Page     int
	PageSize int
}

// ErrInvalidAccountStatus is returned when the status filter is not
// one of the three public vocabulary values. Bad user input is an
// error — never silently dressed up as a legitimate empty result.
var ErrInvalidAccountStatus = apperr.New(400, "INVALID_STATUS", "status must be one of: all, active, disabled")

// adminAccountColumns is the explicit read allowlist shared by list
// and detail. Credential columns are absent by design.
const adminAccountColumns = `id, bot_name, status, balance, api_key_last4, key_issued_at, last_active_at, created_at, updated_at`

func scanAdminAccount(row pgx.Row) (*AdminAccount, error) {
	var a AdminAccount
	if err := row.Scan(&a.ID, &a.BotName, &a.Status, &a.Balance, &a.APIKeyLast4,
		&a.KeyIssuedAt, &a.LastActive, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// AdminListAccounts returns the platform accounts matching the
// filters, paginated with a stable ORDER BY (created_at DESC, id
// DESC — same convention as the other admin lists), plus the total
// match count. NO aggregate joins: list stays a flat single-query
// projection (aggregates live in the detail view only).
func AdminListAccounts(ctx context.Context, q pg.Querier, f AdminAccountFilter) ([]AdminAccount, int64, error) {
	where, args := "WHERE 1=1", []interface{}{}
	n := 0
	addArg := func(v interface{}) string {
		n++
		args = append(args, v)
		return "$" + strconv.Itoa(n)
	}
	switch f.Status {
	case "", "all":
	case AdminAccountStatusActive, AdminAccountStatusDisabled:
		where += " AND status = " + addArg(f.Status)
	default:
		// Unknown status filter: an explicit 400 — bad input must not
		// masquerade as a legitimate empty result.
		return nil, 0, ErrInvalidAccountStatus
	}
	if f.Q != "" {
		where += " AND bot_name LIKE " + addArg("%"+f.Q+"%")
	}

	var total int64
	if err := q.QueryRow(ctx,
		"SELECT COUNT(*) FROM tb_bots "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (f.Page - 1) * f.PageSize
	limit := addArg(f.PageSize)
	off := addArg(offset)
	rows, err := q.Query(ctx, `
		SELECT `+adminAccountColumns+`
		FROM tb_bots `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT `+limit+` OFFSET `+off, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var items []AdminAccount
	for rows.Next() {
		var it AdminAccount
		if err := rows.Scan(&it.ID, &it.BotName, &it.Status, &it.Balance, &it.APIKeyLast4,
			&it.KeyIssuedAt, &it.LastActive, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, it)
	}
	return items, total, rows.Err()
}

// AdminGetAccountDetail returns one account with its existing light
// aggregates. Returns (nil, nil) when the account does not exist.
// The three counts are separate business facts; they are computed
// with three independent scalar subqueries (single query, no N+1).
func AdminGetAccountDetail(ctx context.Context, q pg.Querier, botID int64) (*AdminAccountDetail, error) {
	row := q.QueryRow(ctx, `
		SELECT `+adminAccountColumns+`,
			(SELECT COUNT(*) FROM tb_tasks WHERE tb_tasks.bot_id = tb_bots.id)               AS published_task_count,
			(SELECT COUNT(*) FROM tb_task_submissions WHERE tb_task_submissions.bot_id = tb_bots.id) AS submission_count,
			(SELECT COUNT(*) FROM tb_kungfus WHERE tb_kungfus.bot_id = tb_bots.id)           AS kungfu_count
		FROM tb_bots
		WHERE id = $1`, botID)
	var d AdminAccountDetail
	if err := row.Scan(&d.ID, &d.BotName, &d.Status, &d.Balance, &d.APIKeyLast4,
		&d.KeyIssuedAt, &d.LastActive, &d.CreatedAt, &d.UpdatedAt,
		&d.PublishedTaskCount, &d.SubmissionCount, &d.KungfuCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

// AdminLockBotForUpdate transactionally locks the target tb_bots row
// (SELECT ... FOR UPDATE) and re-reads the status under the lock.
// Returns (nil, nil) when the account does not exist.
func AdminLockBotForUpdate(ctx context.Context, q pg.Querier, botID int64) (*AdminAccount, error) {
	return scanAdminAccount(q.QueryRow(ctx, `
		SELECT `+adminAccountColumns+`
		FROM tb_bots
		WHERE id = $1
		FOR UPDATE`, botID))
}

// AdminSetBotStatus writes the account status inside the caller's
// transaction. The caller (admin domain) has already validated the
// transition; this primitive only persists it.
func AdminSetBotStatus(ctx context.Context, q pg.Querier, botID int64, status string) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_bots SET status = $1, updated_at = NOW() WHERE id = $2`, status, botID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// AdminAccountKeyFacts is the masked key metadata projection used by
// tests to assert last4/issued_at exposure without credential data.
type AdminAccountKeyFacts struct {
	Last4     string
	IssuedAt  *time.Time
	Status    string
	BotExists bool
}

// AdminGetAccountKeyFacts returns the masked key metadata for one
// account (no credential columns selected).
func AdminGetAccountKeyFacts(ctx context.Context, q pg.Querier, botID int64) (*AdminAccountKeyFacts, error) {
	var k AdminAccountKeyFacts
	err := q.QueryRow(ctx,
		`SELECT api_key_last4, key_issued_at, status FROM tb_bots WHERE id = $1`, botID).
		Scan(&k.Last4, &k.IssuedAt, &k.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	k.BotExists = true
	return &k, nil
}

// AdminCountAccountsWithStatus returns how many platform accounts
// hold the given status (read-only fact for tests).
func AdminCountAccountsWithStatus(ctx context.Context, q pg.Querier, status string) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_bots WHERE status = $1`, status).Scan(&n)
	return n, err
}
