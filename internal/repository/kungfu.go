package repository

import (
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
func CountActiveKungfusByBotID(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_kungfus
		WHERE bot_id = $1 AND status = 'active' AND origin = 'standalone'`, botID).Scan(&count)
	return count, err
}

// KungfuListItem holds the public projection returned by ListActiveKungfusByBotID.
type KungfuListItem struct {
	Code        string
	Title       string
	TagsJSON    string
	Description *string
	Visibility  string
	CreatedAt   string
	UpdatedAt   string
}

// -- 2. listActiveByBotId --
func ListActiveKungfusByBotID(ctx context.Context, q pg.Querier, botID int64, limit, offset int) ([]KungfuListItem, error) {
	rows, err := q.Query(ctx, `
		SELECT code, title, tags_json::text, description, visibility, created_at, updated_at
		FROM tb_kungfus
		WHERE bot_id = $1 AND status = 'active' AND origin = 'standalone'
		ORDER BY updated_at DESC, id DESC
		LIMIT $2 OFFSET $3`, botID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []KungfuListItem
	for rows.Next() {
		var it KungfuListItem
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&it.Code, &it.Title, &it.TagsJSON, &it.Description,
			&it.Visibility, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		it.CreatedAt = createdAt.Format("2006-01-02 15:04:05")
		it.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// -- 3. findActiveByCode --
func FindActiveKungfuByCode(ctx context.Context, q pg.Querier, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, revision, origin, created_at, updated_at
		FROM tb_kungfus
		WHERE code = $1 AND status = 'active'`, code)
	return scanKungfu(row)
}

// -- 4. findOwnedActiveByCode --
func FindOwnedActiveKungfuByCode(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, revision, origin, created_at, updated_at
		FROM tb_kungfus
		WHERE code = $1 AND bot_id = $2 AND status = 'active'`, code, botID)
	return scanKungfu(row)
}

// FindOwnedActiveKungfuByCodeForUpdate is the row-locking form used by
// Memory update transactions before archiving the current revision.
func FindOwnedActiveKungfuByCodeForUpdate(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, revision, origin, created_at, updated_at
		FROM tb_kungfus
		WHERE code = $1 AND bot_id = $2 AND status = 'active'
		FOR UPDATE`, code, botID)
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
		&k.Content, &k.Checksum, &k.Visibility, &k.Status, &k.Revision, &k.Origin,
		&createdAt, &updatedAt); err != nil {
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

// -- 7. revision/update --

// ArchiveKungfuRevision persists the exact current editable fields under the
// current revision. The caller must hold the current row lock and commit the
// archive together with the replacement update.
func ArchiveKungfuRevision(ctx context.Context, q pg.Querier, k *model.Kungfu) error {
	_, err := q.Exec(ctx, `
		INSERT INTO memory_revisions
		    (memory_id, revision, title, tags_json, description, content, checksum)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		k.ID, k.Revision, k.Title, k.TagsJSON, k.Description, k.Content, k.Checksum)
	return err
}

// UpdateKungfuContentByID replaces the current editable fields and advances
// the current revision. Archiving is deliberately separate so service owns
// the transaction containing lock → archive → update.
func UpdateKungfuContentByID(ctx context.Context, q pg.Querier, id int64, title, tagsJSON, description, content, checksum string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_kungfus
		SET title = $1, tags_json = $2, description = $3,
		    content = $4, checksum = $5, revision = revision + 1, updated_at = NOW()
		WHERE id = $6`,
		title, tagsJSON, description, content, checksum, id)
	return err
}

// FindMemoryRevision resolves a pinned Memory revision. The current revision
// is read from tb_kungfus even after soft-delete; older revisions come from
// memory_revisions.
func FindMemoryRevision(ctx context.Context, q pg.Querier, memoryID, revision int64) (*model.MemoryRevision, error) {
	var current model.MemoryRevision
	var currentRevision int64
	err := q.QueryRow(ctx, `
		SELECT id, revision, title, tags_json::text, description, content, checksum
		FROM tb_kungfus WHERE id = $1`, memoryID).Scan(
		&current.MemoryID, &currentRevision, &current.Title, &current.TagsJSON,
		&current.Description, &current.Content, &current.Checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revision == currentRevision {
		current.Revision = currentRevision
		return &current, nil
	}
	if revision < 1 || revision > currentRevision {
		return nil, nil
	}

	var historical model.MemoryRevision
	err = q.QueryRow(ctx, `
		SELECT memory_id, revision, title, tags_json::text, description, content, checksum
		FROM memory_revisions
		WHERE memory_id = $1 AND revision = $2`, memoryID, revision).Scan(
		&historical.MemoryID, &historical.Revision, &historical.Title, &historical.TagsJSON,
		&historical.Description, &historical.Content, &historical.Checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &historical, nil
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
// InsertNewKungfu inserts a standalone private, active current Memory row.
func InsertNewKungfu(ctx context.Context, q pg.Querier, code string, botID int64, title, tagsJSON, description, content, checksum string) error {
	_, err := insertKungfuWithOrigin(ctx, q, code, botID, title, tagsJSON, description, content, checksum, "standalone")
	return err
}

// InsertThreadKungfu is the Thread-internal persistence primitive. It writes a
// current Memory row directly with origin=thread and never invokes standalone
// Store consumption policy.
func InsertThreadKungfu(ctx context.Context, q pg.Querier, code string, botID int64, title, tagsJSON, description, content, checksum string) (int64, error) {
	return insertKungfuWithOrigin(ctx, q, code, botID, title, tagsJSON, description, content, checksum, "thread")
}

func insertKungfuWithOrigin(ctx context.Context, q pg.Querier, code string, botID int64, title, tagsJSON, description, content, checksum, origin string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_kungfus
		    (code, bot_id, title, tags_json, description, content, checksum, visibility, status, revision, origin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'private', 'active', 1, $8, NOW(), NOW())
		RETURNING id`,
		code, botID, title, tagsJSON, description, content, checksum, origin).Scan(&id)
	return id, err
}
