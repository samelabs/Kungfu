package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// numericToFloat converts pgtype.Numeric to float64 safely.
func numericToFloat(n pgtype.Numeric) float64 {
	if f, err := n.Float64Value(); err == nil {
		return f.Float64
	}
	return 0
}

// timeToStr converts time.Time to the canonical timestamp string format ("2006-01-02 15:04:05").
func timeToStr(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

// timePtrToStr converts *time.Time to *string (nil-safe).
func timePtrToStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := timeToStr(*t)
	return &s
}

// -- 1. findActiveBotAccountById --
// FindActiveBotAccountByID returns the active bot matching the id, or nil if not found.
func FindActiveBotAccountByID(ctx context.Context, q pg.Querier, botID int64) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT bot_name, status
		FROM tb_bots
		WHERE id = $1 AND status = 'active'`, botID)
	var b model.Bot
	if err := row.Scan(&b.BotName, &b.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = botID
	return &b, nil
}

// -- 2. findActiveBotKeyById --
// FindActiveBotKeyByID returns the active bot with key metadata
// (stored SHA-256 digest + display-only last4), or nil if not found.
// The plaintext key no longer exists at rest; reset verification
// compares against APIKeyHash.
func FindActiveBotKeyByID(ctx context.Context, q pg.Querier, botID int64) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT id, bot_name, api_key_hash, api_key_last4, status, key_issued_at
		FROM tb_bots
		WHERE id = $1 AND status = 'active'`, botID)
	var (
		b           model.Bot
		dbID        int32
		keyIssuedAt *time.Time
	)
	if err := row.Scan(&dbID, &b.BotName, &b.APIKeyHash, &b.APIKeyLast4, &b.Status, &keyIssuedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = int64(dbID)
	b.KeyIssuedAt = timePtrToStr(keyIssuedAt)
	return &b, nil
}

// -- 3. findActiveBotByApiKeyHash --
// FindActiveBotByAPIKeyHash looks up an active bot by the SHA-256
// digest of its API key, or nil if not found. The repository accepts
// ONLY the digest — there is no plaintext-key query path.
func FindActiveBotByAPIKeyHash(ctx context.Context, q pg.Querier, keyHash []byte) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT id, bot_name, status
		FROM tb_bots
		WHERE api_key_hash = $1 AND status = 'active'`, keyHash)
	var (
		b    model.Bot
		dbID int32
	)
	if err := row.Scan(&dbID, &b.BotName, &b.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = int64(dbID)
	return &b, nil
}

// -- 4. findActiveBotSummaryById --
// FindActiveBotSummaryByID returns a trimmed active bot summary, or nil if not found.
func FindActiveBotSummaryByID(ctx context.Context, q pg.Querier, botID int64) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT id, bot_name, status
		FROM tb_bots
		WHERE id = $1 AND status = 'active'`, botID)
	var (
		b    model.Bot
		dbID int32
	)
	if err := row.Scan(&dbID, &b.BotName, &b.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = int64(dbID)
	return &b, nil
}

// -- 5. botNameExists --
func BotNameExists(ctx context.Context, q pg.Querier, name string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM tb_bots WHERE bot_name = $1)`, name).Scan(&exists)
	return exists, err
}

// -- 6. findActiveBotCredentialsByName --
// FindActiveBotCredentialsByName returns the active bot's auth credentials by name.
func FindActiveBotCredentialsByName(ctx context.Context, q pg.Querier, name string) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT id, bot_name, password_hash, status
		FROM tb_bots
		WHERE bot_name = $1 AND status = 'active'`, name)
	var (
		b    model.Bot
		dbID int32
	)
	if err := row.Scan(&dbID, &b.BotName, &b.PasswordHash, &b.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = int64(dbID)
	return &b, nil
}

// -- 7. findOwnerSessionBotById --
// FindOwnerSessionBotByID returns the active bot with fields needed for owner-session views.
func FindOwnerSessionBotByID(ctx context.Context, q pg.Querier, botID int64) (*model.Bot, error) {
	row := q.QueryRow(ctx, `
		SELECT id, bot_name, status, key_issued_at
		FROM tb_bots
		WHERE id = $1 AND status = 'active'`, botID)
	var (
		b           model.Bot
		dbID        int32
		keyIssuedAt *time.Time
	)
	if err := row.Scan(&dbID, &b.BotName, &b.Status, &keyIssuedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	b.ID = int64(dbID)
	b.KeyIssuedAt = timePtrToStr(keyIssuedAt)
	return &b, nil
}

// KungfuStats holds the aggregate counts returned by KungfuStatsByBotID.
type KungfuStats struct {
	Total       int64
	PublicTotal int64
}

// -- 8. kungfuStatsByBotId --
func KungfuStatsByBotID(ctx context.Context, q pg.Querier, botID int64) (KungfuStats, error) {
	var stats KungfuStats
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total,
		       COALESCE(SUM(CASE WHEN visibility = 'public' THEN 1 ELSE 0 END), 0) AS public_total
		FROM tb_kungfus
		WHERE bot_id = $1 AND status = 'active'`, botID).Scan(&stats.Total, &stats.PublicTotal)
	if err != nil {
		return KungfuStats{}, err
	}
	return stats, nil
}

// -- 9. platformTaskCountByBotId --
func PlatformTaskCountByBotID(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_tasks
		WHERE bot_id = $1`, botID).Scan(&count)
	return count, err
}

// -- 10. updatePasswordHashById --
func UpdatePasswordHashByID(ctx context.Context, q pg.Querier, botID int64, passwordHash string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_bots SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		passwordHash, botID)
	return err
}

// -- 11. updateAgentKeyHashById --
// UpdateAgentKeyHashByID atomically replaces the stored key digest,
// display-only last4, and issuance timestamp. It never accepts or
// writes a raw key.
func UpdateAgentKeyHashByID(ctx context.Context, q pg.Querier, botID int64, keyHash []byte, last4 string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_bots SET api_key_hash = $1, api_key_last4 = $2, key_issued_at = NOW(), updated_at = NOW() WHERE id = $3`,
		keyHash, last4, botID)
	return err
}

// -- 12. updateLastActiveAt --
func UpdateLastActiveAt(ctx context.Context, q pg.Querier, botID int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_bots SET last_active_at = NOW(), updated_at = NOW() WHERE id = $1`, botID)
	return err
}

// -- 13. insertRegisteredBot --
// InsertRegisteredBot inserts a freshly registered bot inside an open
// transaction with balance=0; the signup grant (+66, grant_signup) is applied
// afterwards via the credits domain so the ledger always has a genesis entry.
func InsertRegisteredBot(ctx context.Context, q pg.Querier, name string, keyHash []byte, keyLast4, passwordHash, ip string) (int64, error) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	var id int32
	err := q.QueryRow(ctx, `
		INSERT INTO tb_bots
		    (bot_name, api_key_hash, api_key_last4, password_hash, key_issued_at, balance, register_ip, status, last_active_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 0, $6, 'active', $7, NOW(), NOW())
		RETURNING id`,
		name, keyHash, keyLast4, passwordHash, now, ip, now).Scan(&id)
	if err != nil {
		return 0, err
	}
	return int64(id), nil
}
