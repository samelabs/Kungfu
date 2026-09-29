package repository

import (
	"fmt"

	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
)

// KungfuRepository persists kungfu (skill document) rows.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and pgx.Tx.

// -- 1. countActiveByBotId --
// CountActiveKungfusByBotID returns the number of active kungfus owned by a bot.
// KungfuListFilter narrows ListActiveKungfusByBot (WO-21): Q matches
// title, description and tags case-insensitively (LIKE wildcards are
// escaped by the caller); Code is an exact match. Both optional.
type KungfuListFilter struct {
	Q    string
	Code string
}

// KungfuListItem holds the public projection returned by ListActiveKungfusByBotID.
type KungfuListItem struct {
	Code        string
	Title       string
	TagsJSON    string
	Description *string
	Visibility  string
	Bytes       int64
	CreatedAt   string
	UpdatedAt   string
}

// ListActiveKungfusByBot returns ONE page of the bot's active
// memories with the TOTAL number of matching rows (WO-21): the count
// runs as its own query over the same filters, so paging stays exact.
func ListActiveKungfusByBot(ctx context.Context, q pg.Querier, botID int64, f KungfuListFilter, limit, offset int) ([]KungfuListItem, int64, error) {
	where := ` WHERE bot_id = $1 AND status = 'active'`
	args := []any{botID}
	if f.Code != "" {
		args = append(args, f.Code)
		where += fmt.Sprintf(` AND code = $%d`, len(args))
	}
	if f.Q != "" {
		args = append(args, "%"+f.Q+"%")
		n := len(args)
		where += fmt.Sprintf(`
		  AND (title ILIKE $%d ESCAPE '\'
		        OR COALESCE(description, '') ILIKE $%d ESCAPE '\'
		        OR tags_json::text ILIKE $%d ESCAPE '\')`, n, n, n)
	}
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := q.Query(ctx, `
		SELECT code, title, tags_json::text, description, visibility,
		       OCTET_LENGTH(content) AS bytes, created_at, updated_at
		FROM tb_kungfus`+
		where+`
		ORDER BY updated_at DESC, id DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []KungfuListItem
	for rows.Next() {
		var it KungfuListItem
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&it.Code, &it.Title, &it.TagsJSON, &it.Description,
			&it.Visibility, &it.Bytes, &createdAt, &updatedAt); err != nil {
			return nil, 0, err
		}
		it.CreatedAt = createdAt.Format("2006-01-02 15:04:05")
		it.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// -- 3. findActiveByCode --
func FindActiveKungfuByCode(ctx context.Context, q pg.Querier, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, created_at, updated_at
		FROM tb_kungfus
		WHERE code = $1 AND status = 'active'`, code)
	return scanKungfu(row)
}

// -- 4. findOwnedActiveByCode --
func FindOwnedActiveKungfuByCode(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, created_at, updated_at
		FROM tb_kungfus
		WHERE code = $1 AND bot_id = $2 AND status = 'active'`, code, botID)
	return scanKungfu(row)
}

// scanKungfu scans a kungfu row with type conversion.
func scanKungfu(row pgx.Row) (*model.Kungfu, error) {
	var (
		k         model.Kungfu
		botID     int32
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(&k.ID, &k.Code, &botID, &k.Title, &k.TagsJSON, &k.Description,
		&k.Content, &k.Checksum, &k.Visibility, &k.Status, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	k.BotID = int64(botID)
	k.CreatedAt = createdAt.Format("2006-01-02 15:04:05")
	k.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
	return &k, nil
}

// -- 5. updateVisibilityById --
// UpdateKungfuVisibilityByID sets visibility (public/private) for a kungfu.
func UpdateKungfuVisibilityByID(ctx context.Context, q pg.Querier, id int64, visibility string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_kungfus SET visibility = $1, updated_at = NOW() WHERE id = $2`,
		visibility, id)
	return err
}

// -- 6. softDeleteById --
// SoftDeleteKungfuByID marks a kungfu as deleted (status='deleted') without removing the row.
func SoftDeleteKungfuByID(ctx context.Context, q pg.Querier, id int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_kungfus SET status = 'deleted' WHERE id = $1`, id)
	return err
}

// -- 7. updateContentById --
// UpdateKungfuContentByID overwrites the editable content fields of a kungfu.
func UpdateKungfuContentByID(ctx context.Context, q pg.Querier, id int64, title, tagsJSON, description, content, checksum string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_kungfus
		SET title = $1, tags_json = $2, description = $3,
		    content = $4, checksum = $5, updated_at = NOW()
		WHERE id = $6`,
		title, tagsJSON, description, content, checksum, id)
	return err
}

// -- 8. generateUniqueCode --
// GenerateUniqueKungfuCode generates a 12-hex code that does not yet exist in tb_kungfus.
func GenerateUniqueKungfuCode(ctx context.Context, q pg.Querier) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM tb_kungfus WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

// -- 9. insertNewKungfu --
// InsertNewKungfu inserts a new private, active kungfu row.
func InsertNewKungfu(ctx context.Context, q pg.Querier, code string, botID int64, title, tagsJSON, description, content, checksum string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_kungfus
		    (code, bot_id, title, tags_json, description, content, checksum, visibility, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'private', 'active', NOW(), NOW())`,
		code, botID, title, tagsJSON, description, content, checksum)
	return err
}
