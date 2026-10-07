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
// Standalone memories only, matching ListActiveKungfusByBotID.
func CountActiveKungfusByBotID(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_kungfus
		WHERE bot_id = $1 AND status = 'active' AND origin <> 'thread'`, botID).Scan(&count)
	return count, err
}

// KungfuListItem holds the public projection returned by ListActiveKungfusByBotID.
type KungfuListItem struct {
	Code        string
	Title       string
	TagsJSON    string
	Description *string
	Visibility  string
	Revision    int64
	Origin      string
	CreatedAt   string
	UpdatedAt   string
}

// -- 2. listActiveByBotId --
// Standalone memories only: origin='thread' rows (Thread-stage entry
// payloads, no writer yet) stay out of the default listing.
func ListActiveKungfusByBotID(ctx context.Context, q pg.Querier, botID int64, limit, offset int) ([]KungfuListItem, error) {
	rows, err := q.Query(ctx, `
		SELECT code, title, tags_json::text, description, visibility,
		       revision, origin, created_at, updated_at
		FROM tb_kungfus
		WHERE bot_id = $1 AND status = 'active' AND origin <> 'thread'
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
			&it.Visibility, &it.Revision, &it.Origin, &createdAt, &updatedAt); err != nil {
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
		       visibility, status, created_at, updated_at, revision, origin
		FROM tb_kungfus
		WHERE code = $1 AND status = 'active'`, code)
	return scanKungfu(row)
}

// -- 4. findOwnedActiveByCode --
func FindOwnedActiveKungfuByCode(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, created_at, updated_at, revision, origin
		FROM tb_kungfus
		WHERE code = $1 AND bot_id = $2 AND status = 'active'`, code, botID)
	return scanKungfu(row)
}

// -- 4a. findKungfuByCodeAnyStatus --
// FindKungfuByCodeAnyStatus returns the memory row regardless of
// status — the author may read any of their versions even after a
// withdrawal (kungfu.md §5/§9), so revision reads must locate the
// row first and judge authorship before validity.
func FindKungfuByCodeAnyStatus(ctx context.Context, q pg.Querier, code string) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, created_at, updated_at, revision, origin
		FROM tb_kungfus
		WHERE code = $1`, code)
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
		&k.Content, &k.Checksum, &k.Visibility, &k.Status, &createdAt, &updatedAt,
		&k.Revision, &k.Origin); err != nil {
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

// -- 7. updateContentWithRevision --
// The versioned update, one step of the single update transaction
// (kungfu.md §5): the caller first locks the row
// (LockOwnedActiveKungfuByID), then ArchiveKungfuRevision snapshots
// the locked old version into memory_revisions, then this statement
// overwrites the content and bumps revision = revision + 1 (relative
// — never an absolute value — so concurrent updates serialized by the
// row lock produce one new version each, with no gaps). Returns the
// new current revision.
func UpdateKungfuContentWithRevision(ctx context.Context, q pg.Querier, id int64, title, tagsJSON, description, content, checksum string) (int64, error) {
	var revision int64
	err := q.QueryRow(ctx, `
		UPDATE tb_kungfus
		SET title = $1, tags_json = $2, description = $3,
		    content = $4, checksum = $5, updated_at = NOW(),
		    revision = revision + 1
		WHERE id = $6
		RETURNING revision`,
		title, tagsJSON, description, content, checksum, id).Scan(&revision)
	return revision, err
}

// -- 7a. lockOwnedActiveKungfuByID --
// LockOwnedActiveKungfuByID takes the row lock (SELECT ... FOR
// UPDATE) on the caller's active memory. Run inside the update
// transaction: it serializes concurrent updates of one memory and
// re-reads the row the archive step snapshots. A withdrawn (deleted)
// memory yields no row — updates after withdrawal are impossible.
func LockOwnedActiveKungfuByID(ctx context.Context, q pg.Querier, id, botID int64) (*model.Kungfu, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, tags_json::text, description, content, checksum,
		       visibility, status, created_at, updated_at, revision, origin
		FROM tb_kungfus
		WHERE id = $1 AND bot_id = $2 AND status = 'active'
		FOR UPDATE`, id, botID)
	return scanKungfu(row)
}

// -- 7b. archiveKungfuRevision --
// ArchiveKungfuRevision snapshots the memory's CURRENT row (the old
// version, already row-locked in this transaction) into
// memory_revisions, copying the content fields verbatim from the row
// itself. The (memory_id, revision) primary key makes every version
// archive exactly once and keeps archived rows immutable by
// construction — a re-insert of the same revision is rejected.
func ArchiveKungfuRevision(ctx context.Context, q pg.Querier, id int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO memory_revisions
		    (memory_id, revision, title, tags_json, description, content, checksum, updated_at)
		SELECT id, revision, title, tags_json, description, content, checksum, updated_at
		FROM tb_kungfus
		WHERE id = $1`, id)
	return err
}

// -- 7c. findKungfuRevision --
// FindKungfuRevision returns one archived prior version of a memory,
// or nil when that revision does not exist.
func FindKungfuRevision(ctx context.Context, q pg.Querier, memoryID, revision int64) (*model.KungfuRevision, error) {
	row := q.QueryRow(ctx, `
		SELECT memory_id, revision, title, tags_json::text, description, content, checksum, updated_at
		FROM memory_revisions
		WHERE memory_id = $1 AND revision = $2`, memoryID, revision)
	var (
		r         model.KungfuRevision
		updatedAt time.Time
	)
	if err := row.Scan(&r.MemoryID, &r.Revision, &r.Title, &r.TagsJSON, &r.Description,
		&r.Content, &r.Checksum, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
	return &r, nil
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
