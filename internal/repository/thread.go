package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
)

type ThreadRow struct {
	ID            int64
	Code          string
	OwnerID       int64
	OwnerName     string
	Title         string
	Objective     string
	Status        string
	NextActorID   *int64
	NextActorName *string
	NextAction    *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ClosedAt      *time.Time
	MemberRole    string
}

type ThreadMemberRow struct {
	BotID     int64
	BotName   string
	Role      string
	Status    string
	JoinedAt  time.Time
	RemovedAt *time.Time
}

type ThreadInviteRow struct {
	ID             int64
	ThreadID       int64
	CreatedByID    int64
	InviteeName    *string
	ExpiresAt      time.Time
	AcceptedByID   *int64
	AcceptedAt     *time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
	ThreadStatus   string
	ThreadOwnerID  int64
}

type ThreadMessageRow struct {
	ID         int64
	ThreadID   int64
	AuthorID   int64
	AuthorName string
	Body       string
	CreatedAt  time.Time
}

type ThreadDeliveryRow struct {
	ID           int64
	ThreadID     int64
	AuthorID     int64
	AuthorName   string
	Title        string
	Body         string
	Status       string
	RevisesID    *int64
	ReviewerID   *int64
	ReviewerName *string
	ReviewNote   *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ReviewedAt   *time.Time
}

type ThreadEventRow struct {
	ID        int64
	ThreadID  int64
	ActorID   *int64
	ActorName *string
	Type      string
	Payload   string
	CreatedAt time.Time
}

func GenerateUniqueThreadCode(ctx context.Context, q pg.Querier) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM tb_thread WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

func InsertThread(ctx context.Context, q pg.Querier, code string, ownerID int64, title, objective string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_thread
		    (code, owner_id, title, objective, status, next_actor_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'active', NULL, NOW(), NOW())
		RETURNING id`, code, ownerID, title, objective).Scan(&id)
	return id, err
}

func InsertOwnerMembership(ctx context.Context, q pg.Querier, threadID, ownerID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_thread_member (thread_id, bot_id, role, status, joined_at)
		VALUES ($1, $2, 'owner', 'active', NOW())`, threadID, ownerID)
	return err
}

func FindThreadByCode(ctx context.Context, q pg.Querier, code string) (*ThreadRow, error) {
	row := q.QueryRow(ctx, `
		SELECT t.id, t.code, t.owner_id, owner.bot_name, t.title, t.objective,
		       t.status, t.next_actor_id, next_bot.bot_name, t.next_action,
		       t.created_at, t.updated_at, t.closed_at
		FROM tb_thread t
		JOIN tb_bots owner ON owner.id = t.owner_id
		LEFT JOIN tb_bots next_bot ON next_bot.id = t.next_actor_id
		WHERE t.code = $1`, code)
	var r ThreadRow
	if err := row.Scan(
		&r.ID, &r.Code, &r.OwnerID, &r.OwnerName, &r.Title, &r.Objective,
		&r.Status, &r.NextActorID, &r.NextActorName, &r.NextAction,
		&r.CreatedAt, &r.UpdatedAt, &r.ClosedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func FindActiveThreadMembership(ctx context.Context, q pg.Querier, threadID, botID int64) (*ThreadMemberRow, error) {
	row := q.QueryRow(ctx, `
		SELECT m.bot_id, b.bot_name, m.role, m.status, m.joined_at, m.removed_at
		FROM tb_thread_member m
		JOIN tb_bots b ON b.id = m.bot_id
		WHERE m.thread_id = $1 AND m.bot_id = $2 AND m.status = 'active'`,
		threadID, botID)
	var r ThreadMemberRow
	if err := row.Scan(&r.BotID, &r.BotName, &r.Role, &r.Status, &r.JoinedAt, &r.RemovedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func FindActiveThreadMemberByName(ctx context.Context, q pg.Querier, threadID int64, name string) (*ThreadMemberRow, error) {
	row := q.QueryRow(ctx, `
		SELECT m.bot_id, b.bot_name, m.role, m.status, m.joined_at, m.removed_at
		FROM tb_thread_member m
		JOIN tb_bots b ON b.id = m.bot_id
		WHERE m.thread_id = $1 AND b.bot_name = $2 AND m.status = 'active'`,
		threadID, name)
	var r ThreadMemberRow
	if err := row.Scan(&r.BotID, &r.BotName, &r.Role, &r.Status, &r.JoinedAt, &r.RemovedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func UpsertThreadMember(ctx context.Context, q pg.Querier, threadID, botID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_thread_member
		    (thread_id, bot_id, role, status, joined_at, removed_at)
		VALUES ($1, $2, 'member', 'active', NOW(), NULL)
		ON CONFLICT (thread_id, bot_id) DO UPDATE
		    SET status = 'active', role = 'member', joined_at = NOW(), removed_at = NULL
		WHERE tb_thread_member.role <> 'owner'`, threadID, botID)
	return err
}

func RemoveThreadMember(ctx context.Context, q pg.Querier, threadID, botID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_thread_member
		SET status = 'removed', removed_at = NOW()
		WHERE thread_id = $1 AND bot_id = $2 AND role <> 'owner' AND status = 'active'`,
		threadID, botID)
	return err == nil && tag.RowsAffected() == 1, err
}

func CountThreadsForBot(ctx context.Context, q pg.Querier, botID int64, status string) (int64, error) {
	var total int64
	if status == "" {
		err := q.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM tb_thread t
			JOIN tb_thread_member m ON m.thread_id = t.id
			WHERE m.bot_id = $1 AND m.status = 'active'`, botID).Scan(&total)
		return total, err
	}
	err := q.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM tb_thread t
		JOIN tb_thread_member m ON m.thread_id = t.id
		WHERE m.bot_id = $1 AND m.status = 'active' AND t.status = $2`,
		botID, status).Scan(&total)
	return total, err
}

func ListThreadsForBot(ctx context.Context, q pg.Querier, botID int64, status string, limit, offset int) ([]ThreadRow, error) {
	query := `
		SELECT t.id, t.code, t.owner_id, owner.bot_name, t.title, t.objective,
		       t.status, t.next_actor_id, next_bot.bot_name, t.next_action,
		       t.created_at, t.updated_at, t.closed_at, m.role
		FROM tb_thread t
		JOIN tb_thread_member m ON m.thread_id = t.id
		JOIN tb_bots owner ON owner.id = t.owner_id
		LEFT JOIN tb_bots next_bot ON next_bot.id = t.next_actor_id
		WHERE m.bot_id = $1 AND m.status = 'active'`
	args := []any{botID}
	if status != "" {
		query += ` AND t.status = $2`
		args = append(args, status)
		query += ` ORDER BY t.updated_at DESC, t.id DESC LIMIT $3 OFFSET $4`
		args = append(args, limit, offset)
	} else {
		query += ` ORDER BY t.updated_at DESC, t.id DESC LIMIT $2 OFFSET $3`
		args = append(args, limit, offset)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ThreadRow{}
	for rows.Next() {
		var r ThreadRow
		if err := rows.Scan(
			&r.ID, &r.Code, &r.OwnerID, &r.OwnerName, &r.Title, &r.Objective,
			&r.Status, &r.NextActorID, &r.NextActorName, &r.NextAction,
			&r.CreatedAt, &r.UpdatedAt, &r.ClosedAt, &r.MemberRole,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ListActiveThreadMembers(ctx context.Context, q pg.Querier, threadID int64) ([]ThreadMemberRow, error) {
	rows, err := q.Query(ctx, `
		SELECT m.bot_id, b.bot_name, m.role, m.status, m.joined_at, m.removed_at
		FROM tb_thread_member m
		JOIN tb_bots b ON b.id = m.bot_id
		WHERE m.thread_id = $1 AND m.status = 'active'
		ORDER BY CASE WHEN m.role = 'owner' THEN 0 ELSE 1 END, m.joined_at, m.bot_id`,
		threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThreadMemberRow{}
	for rows.Next() {
		var r ThreadMemberRow
		if err := rows.Scan(&r.BotID, &r.BotName, &r.Role, &r.Status, &r.JoinedAt, &r.RemovedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func InsertThreadInvite(ctx context.Context, q pg.Querier, threadID, creatorID int64, tokenHash []byte, inviteeName *string, expiresAt time.Time) (int64, time.Time, error) {
	var id int64
	var createdAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO tb_thread_invite
		    (thread_id, created_by_id, token_hash, invitee_name, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		RETURNING id, created_at`,
		threadID, creatorID, tokenHash, inviteeName, expiresAt).Scan(&id, &createdAt)
	return id, createdAt, err
}

func FindThreadInviteByHashForUpdate(ctx context.Context, q pg.Querier, tokenHash []byte) (*ThreadInviteRow, error) {
	row := q.QueryRow(ctx, `
		SELECT i.id, i.thread_id, i.created_by_id, i.invitee_name, i.expires_at,
		       i.accepted_by_id, i.accepted_at, i.revoked_at, i.created_at,
		       t.status, t.owner_id
		FROM tb_thread_invite i
		JOIN tb_thread t ON t.id = i.thread_id
		WHERE i.token_hash = $1
		FOR UPDATE OF i`, tokenHash)
	var r ThreadInviteRow
	if err := row.Scan(
		&r.ID, &r.ThreadID, &r.CreatedByID, &r.InviteeName, &r.ExpiresAt,
		&r.AcceptedByID, &r.AcceptedAt, &r.RevokedAt, &r.CreatedAt,
		&r.ThreadStatus, &r.ThreadOwnerID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func RevokeThreadInvite(ctx context.Context, q pg.Querier, threadID, inviteID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_thread_invite
		SET revoked_at = COALESCE(revoked_at, NOW())
		WHERE id = $1 AND thread_id = $2 AND accepted_at IS NULL`,
		inviteID, threadID)
	return err == nil && tag.RowsAffected() == 1, err
}

func MarkThreadInviteAccepted(ctx context.Context, q pg.Querier, inviteID, botID int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_thread_invite
		SET accepted_by_id = $1, accepted_at = NOW()
		WHERE id = $2`, botID, inviteID)
	return err
}

func InsertThreadMessage(ctx context.Context, q pg.Querier, threadID, authorID int64, body string) (int64, time.Time, error) {
	var id int64
	var createdAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO tb_thread_message (thread_id, author_id, body, created_at)
		VALUES ($1, $2, $3, NOW())
		RETURNING id, created_at`,
		threadID, authorID, body).Scan(&id, &createdAt)
	return id, createdAt, err
}

func ListRecentThreadMessages(ctx context.Context, q pg.Querier, threadID int64, limit int) ([]ThreadMessageRow, error) {
	rows, err := q.Query(ctx, `
		SELECT x.id, x.thread_id, x.author_id, x.bot_name, x.body, x.created_at
		FROM (
			SELECT m.id, m.thread_id, m.author_id, b.bot_name, m.body, m.created_at
			FROM tb_thread_message m
			JOIN tb_bots b ON b.id = m.author_id
			WHERE m.thread_id = $1
			ORDER BY m.id DESC
			LIMIT $2
		) x
		ORDER BY x.id ASC`, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThreadMessageRow{}
	for rows.Next() {
		var r ThreadMessageRow
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.AuthorID, &r.AuthorName, &r.Body, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func InsertThreadDelivery(ctx context.Context, q pg.Querier, threadID, authorID int64, title, body string, revisesID *int64) (int64, time.Time, error) {
	var id int64
	var createdAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO tb_thread_delivery
		    (thread_id, author_id, title, body, status, revises_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'submitted', $5, NOW(), NOW())
		RETURNING id, created_at`,
		threadID, authorID, title, body, revisesID).Scan(&id, &createdAt)
	return id, createdAt, err
}

func FindThreadDeliveryForUpdate(ctx context.Context, q pg.Querier, threadID, deliveryID int64) (*ThreadDeliveryRow, error) {
	row := q.QueryRow(ctx, `
		SELECT d.id, d.thread_id, d.author_id, author.bot_name, d.title, d.body,
		       d.status, d.revises_id, d.reviewer_id, reviewer.bot_name,
		       d.review_note, d.created_at, d.updated_at, d.reviewed_at
		FROM tb_thread_delivery d
		JOIN tb_bots author ON author.id = d.author_id
		LEFT JOIN tb_bots reviewer ON reviewer.id = d.reviewer_id
		WHERE d.thread_id = $1 AND d.id = $2
		FOR UPDATE OF d`, threadID, deliveryID)
	var r ThreadDeliveryRow
	if err := row.Scan(
		&r.ID, &r.ThreadID, &r.AuthorID, &r.AuthorName, &r.Title, &r.Body,
		&r.Status, &r.RevisesID, &r.ReviewerID, &r.ReviewerName,
		&r.ReviewNote, &r.CreatedAt, &r.UpdatedAt, &r.ReviewedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func ListRecentThreadDeliveries(ctx context.Context, q pg.Querier, threadID int64, limit int) ([]ThreadDeliveryRow, error) {
	rows, err := q.Query(ctx, `
		SELECT d.id, d.thread_id, d.author_id, author.bot_name, d.title, d.body,
		       d.status, d.revises_id, d.reviewer_id, reviewer.bot_name,
		       d.review_note, d.created_at, d.updated_at, d.reviewed_at
		FROM tb_thread_delivery d
		JOIN tb_bots author ON author.id = d.author_id
		LEFT JOIN tb_bots reviewer ON reviewer.id = d.reviewer_id
		WHERE d.thread_id = $1
		ORDER BY d.id DESC
		LIMIT $2`, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThreadDeliveryRow{}
	for rows.Next() {
		var r ThreadDeliveryRow
		if err := rows.Scan(
			&r.ID, &r.ThreadID, &r.AuthorID, &r.AuthorName, &r.Title, &r.Body,
			&r.Status, &r.RevisesID, &r.ReviewerID, &r.ReviewerName,
			&r.ReviewNote, &r.CreatedAt, &r.UpdatedAt, &r.ReviewedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ReviewThreadDelivery(ctx context.Context, q pg.Querier, deliveryID, reviewerID int64, status string, note *string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_thread_delivery
		SET status = $1, reviewer_id = $2, review_note = $3,
		    reviewed_at = NOW(), updated_at = NOW()
		WHERE id = $4`, status, reviewerID, note, deliveryID)
	return err
}

func UpdateThreadHandoff(ctx context.Context, q pg.Querier, threadID int64, nextActorID *int64, nextAction *string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_thread
		SET next_actor_id = $1, next_action = $2, updated_at = NOW()
		WHERE id = $3`, nextActorID, nextAction, threadID)
	return err
}

func ClearThreadHandoffIfActor(ctx context.Context, q pg.Querier, threadID, botID int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_thread
		SET next_actor_id = NULL, next_action = NULL, updated_at = NOW()
		WHERE id = $1 AND next_actor_id = $2`, threadID, botID)
	return err
}

func CloseThread(ctx context.Context, q pg.Querier, threadID int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_thread
		SET status = 'closed', next_actor_id = NULL, next_action = NULL,
		    closed_at = NOW(), updated_at = NOW()
		WHERE id = $1`, threadID)
	return err
}

func TouchThread(ctx context.Context, q pg.Querier, threadID int64) error {
	_, err := q.Exec(ctx, `UPDATE tb_thread SET updated_at = NOW() WHERE id = $1`, threadID)
	return err
}

func InsertThreadEvent(ctx context.Context, q pg.Querier, threadID int64, actorID *int64, eventType, payloadJSON string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_thread_event (thread_id, actor_id, type, payload, created_at)
		VALUES ($1, $2, $3, $4::jsonb, NOW())
		RETURNING id`, threadID, actorID, eventType, payloadJSON).Scan(&id)
	return id, err
}

func MaxThreadEventID(ctx context.Context, q pg.Querier, threadID int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(MAX(id), 0) FROM tb_thread_event WHERE thread_id = $1`,
		threadID).Scan(&id)
	return id, err
}

func ListThreadEventsAfter(ctx context.Context, q pg.Querier, threadID, cursor int64, limit int) ([]ThreadEventRow, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id, e.thread_id, e.actor_id, b.bot_name, e.type, e.payload::text, e.created_at
		FROM tb_thread_event e
		LEFT JOIN tb_bots b ON b.id = e.actor_id
		WHERE e.thread_id = $1 AND e.id > $2
		ORDER BY e.id ASC
		LIMIT $3`, threadID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThreadEventRow{}
	for rows.Next() {
		var r ThreadEventRow
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.ActorID, &r.ActorName, &r.Type, &r.Payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
