package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
)

// ---- RoleLink -------------------------------------------------------------

func FindRoleLink(ctx context.Context, q pg.Querier, lowID, highID int64) (*model.RoleLink, error) {
	return scanRoleLink(q.QueryRow(ctx, `
		SELECT role_low_id, role_high_id, requested_by_role_id, status, created_at, accepted_at
		FROM role_links
		WHERE role_low_id = $1 AND role_high_id = $2`, lowID, highID))
}

func FindRoleLinkForUpdate(ctx context.Context, q pg.Querier, lowID, highID int64) (*model.RoleLink, error) {
	return scanRoleLink(q.QueryRow(ctx, `
		SELECT role_low_id, role_high_id, requested_by_role_id, status, created_at, accepted_at
		FROM role_links
		WHERE role_low_id = $1 AND role_high_id = $2
		FOR UPDATE`, lowID, highID))
}

func scanRoleLink(row pgx.Row) (*model.RoleLink, error) {
	var link model.RoleLink
	if err := row.Scan(&link.RoleLowID, &link.RoleHighID, &link.RequestedByRoleID,
		&link.Status, &link.CreatedAt, &link.AcceptedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &link, nil
}

func InsertPendingRoleLink(ctx context.Context, q pg.Querier, lowID, highID, requesterID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO role_links (role_low_id, role_high_id, requested_by_role_id, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT (role_low_id, role_high_id) DO NOTHING`,
		lowID, highID, requesterID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func ActivateRoleLink(ctx context.Context, q pg.Querier, lowID, highID int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE role_links
		SET status = 'active', accepted_at = NOW()
		WHERE role_low_id = $1 AND role_high_id = $2 AND status = 'pending'`,
		lowID, highID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func DeleteRoleLink(ctx context.Context, q pg.Querier, lowID, highID int64) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM role_links
		WHERE role_low_id = $1 AND role_high_id = $2`, lowID, highID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

type RoleLinkPeerRow struct {
	Link     model.RoleLink
	PeerID   int64
	PeerName string
}

func ListRoleLinksForRole(ctx context.Context, q pg.Querier, roleID int64) ([]RoleLinkPeerRow, error) {
	rows, err := q.Query(ctx, `
		SELECT rl.role_low_id, rl.role_high_id, rl.requested_by_role_id,
		       rl.status, rl.created_at, rl.accepted_at,
		       CASE WHEN rl.role_low_id = $1 THEN rl.role_high_id ELSE rl.role_low_id END AS peer_id,
		       CASE WHEN rl.role_low_id = $1 THEN hi.bot_name ELSE lo.bot_name END AS peer_name
		FROM role_links rl
		JOIN tb_bots lo ON lo.id = rl.role_low_id
		JOIN tb_bots hi ON hi.id = rl.role_high_id
		WHERE rl.role_low_id = $1 OR rl.role_high_id = $1
		ORDER BY rl.created_at DESC, peer_id`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RoleLinkPeerRow
	for rows.Next() {
		var r RoleLinkPeerRow
		if err := rows.Scan(&r.Link.RoleLowID, &r.Link.RoleHighID, &r.Link.RequestedByRoleID,
			&r.Link.Status, &r.Link.CreatedAt, &r.Link.AcceptedAt, &r.PeerID, &r.PeerName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Thread ---------------------------------------------------------------

func GenerateUniqueThreadCode(ctx context.Context, q pg.Querier) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM threads WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

func InsertThread(ctx context.Context, q pg.Querier, code, subject string, creatorID int64, parentThreadID, anchorEntryID *int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO threads
		    (code, subject, created_by_role_id, parent_thread_id, anchor_entry_id, status, next_seq, revision)
		VALUES ($1, $2, $3, $4, $5, 'open', 1, 1)
		RETURNING id`,
		code, subject, creatorID, parentThreadID, anchorEntryID).Scan(&id)
	return id, err
}

const threadSelect = `
	SELECT id, code, join_key_hash, join_entry_id, subject, created_by_role_id,
	       parent_thread_id, anchor_entry_id, status, next_seq, revision,
	       created_at, updated_at
	FROM threads`

func FindThreadByID(ctx context.Context, q pg.Querier, id int64) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, threadSelect+` WHERE id = $1`, id))
}

func FindThreadByCode(ctx context.Context, q pg.Querier, code string) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, threadSelect+` WHERE code = $1`, code))
}

func FindThreadByIDForUpdate(ctx context.Context, q pg.Querier, id int64) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, threadSelect+` WHERE id = $1 FOR UPDATE`, id))
}

func scanThread(row pgx.Row) (*model.Thread, error) {
	var t model.Thread
	if err := row.Scan(&t.ID, &t.Code, &t.JoinKeyHash, &t.JoinEntryID, &t.Subject,
		&t.CreatedByRoleID, &t.ParentThreadID, &t.AnchorEntryID, &t.Status,
		&t.NextSeq, &t.Revision, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

func FindOpenThreadByJoinKeyHashForUpdate(ctx context.Context, q pg.Querier, keyHash []byte) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, threadSelect+`
		WHERE join_key_hash = $1 AND status = 'open'
		FOR UPDATE`, keyHash))
}

func SetThreadJoinKey(ctx context.Context, q pg.Querier, threadID int64, keyHash []byte, entryID int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET join_key_hash = $2, join_entry_id = $3,
		    revision = revision + 1, updated_at = NOW()
		WHERE id = $1`, threadID, keyHash, entryID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func ClearThreadJoinKey(ctx context.Context, q pg.Querier, threadID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET join_key_hash = NULL, join_entry_id = NULL,
		    revision = revision + 1, updated_at = NOW()
		WHERE id = $1 AND join_key_hash IS NOT NULL`, threadID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AllocateThreadSeq atomically reserves the next timeline seq and advances the
// shared Thread revision. Call it inside the transaction that inserts the
// corresponding ThreadMemory.
func AllocateThreadSeq(ctx context.Context, q pg.Querier, threadID int64) (seq, revision int64, err error) {
	err = q.QueryRow(ctx, `
		UPDATE threads
		SET next_seq = next_seq + 1,
		    revision = revision + 1,
		    updated_at = NOW()
		WHERE id = $1
		RETURNING next_seq - 1, revision`, threadID).Scan(&seq, &revision)
	return
}

func BumpThreadRevision(ctx context.Context, q pg.Querier, threadID int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET revision = revision + 1, updated_at = NOW()
		WHERE id = $1`, threadID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func InsertThreadMemory(ctx context.Context, q pg.Querier, threadID, memoryID, memoryRevision, seq, authorRoleID int64, replyToEntryID *int64) (*model.ThreadMemory, error) {
	var tm model.ThreadMemory
	err := q.QueryRow(ctx, `
		INSERT INTO thread_memories
		    (thread_id, memory_id, memory_revision, seq, author_role_id, reply_to_entry_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, thread_id, memory_id, memory_revision, seq, author_role_id,
		          reply_to_entry_id, created_at`,
		threadID, memoryID, memoryRevision, seq, authorRoleID, replyToEntryID).Scan(
		&tm.ID, &tm.ThreadID, &tm.MemoryID, &tm.MemoryRevision, &tm.Seq,
		&tm.AuthorRoleID, &tm.ReplyToEntryID, &tm.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &tm, nil
}

func FindThreadMemoryByID(ctx context.Context, q pg.Querier, entryID int64) (*model.ThreadMemory, error) {
	var tm model.ThreadMemory
	err := q.QueryRow(ctx, `
		SELECT id, thread_id, memory_id, memory_revision, seq, author_role_id,
		       reply_to_entry_id, created_at
		FROM thread_memories
		WHERE id = $1`, entryID).Scan(
		&tm.ID, &tm.ThreadID, &tm.MemoryID, &tm.MemoryRevision, &tm.Seq,
		&tm.AuthorRoleID, &tm.ReplyToEntryID, &tm.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tm, nil
}

func EntryBelongsToThread(ctx context.Context, q pg.Querier, threadID, entryID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM thread_memories
			WHERE thread_id = $1 AND id = $2
		)`, threadID, entryID).Scan(&ok)
	return ok, err
}

// EntryAllowedInThreadScope accepts an entry in the Thread timeline or that
// Thread's direct anchor. This is the allowed entry scope for participant
// entry and reply targets; Branch uses EntryBelongsToThread because a Child
// anchor must live in its direct parent timeline.
func EntryAllowedInThreadScope(ctx context.Context, q pg.Querier, threadID, entryID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM threads t
			WHERE t.id = $1
			  AND (
			    t.anchor_entry_id = $2
			    OR EXISTS(
			      SELECT 1 FROM thread_memories tm
			      WHERE tm.thread_id = t.id AND tm.id = $2
			    )
			  )
		)`, threadID, entryID).Scan(&ok)
	return ok, err
}

func InsertThreadRole(ctx context.Context, q pg.Querier, threadID, roleID int64, permission string, joinedByRoleID *int64, entryID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO thread_roles
		    (thread_id, role_id, permission, joined_by_role_id, entry_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (thread_id, role_id) DO NOTHING`,
		threadID, roleID, permission, joinedByRoleID, entryID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func FindThreadRole(ctx context.Context, q pg.Querier, threadID, roleID int64) (*model.ThreadRole, error) {
	var tr model.ThreadRole
	err := q.QueryRow(ctx, `
		SELECT thread_id, role_id, permission, joined_by_role_id, entry_id, joined_at
		FROM thread_roles
		WHERE thread_id = $1 AND role_id = $2`, threadID, roleID).Scan(
		&tr.ThreadID, &tr.RoleID, &tr.Permission, &tr.JoinedByRoleID, &tr.EntryID, &tr.JoinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tr, nil
}

func ListDirectChildren(ctx context.Context, q pg.Querier, parentThreadID int64) ([]model.Thread, error) {
	rows, err := q.Query(ctx, threadSelect+`
		WHERE parent_thread_id = $1
		ORDER BY created_at, id`, parentThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ThreadLineage returns root → current, bounded only by the persisted parent
// chain. Product/API pagination is added at the work-context layer.
func ThreadLineage(ctx context.Context, q pg.Querier, threadID int64) ([]model.Thread, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, code, join_key_hash, join_entry_id, subject, created_by_role_id,
			       parent_thread_id, anchor_entry_id, status, next_seq, revision,
			       created_at, updated_at, 0 AS depth
			FROM threads WHERE id = $1
			UNION ALL
			SELECT p.id, p.code, p.join_key_hash, p.join_entry_id, p.subject, p.created_by_role_id,
			       p.parent_thread_id, p.anchor_entry_id, p.status, p.next_seq, p.revision,
			       p.created_at, p.updated_at, c.depth + 1
			FROM threads p
			JOIN chain c ON p.id = c.parent_thread_id
		)
		SELECT id, code, join_key_hash, join_entry_id, subject, created_by_role_id,
		       parent_thread_id, anchor_entry_id, status, next_seq, revision,
		       created_at, updated_at
		FROM chain
		ORDER BY depth DESC`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func RootCreatorCanGovernThread(ctx context.Context, q pg.Querier, threadID, roleID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, parent_thread_id, created_by_role_id
			FROM threads WHERE id = $1
			UNION ALL
			SELECT p.id, p.parent_thread_id, p.created_by_role_id
			FROM threads p
			JOIN chain c ON p.id = c.parent_thread_id
		)
		SELECT EXISTS(
			SELECT 1 FROM chain
			WHERE parent_thread_id IS NULL AND created_by_role_id = $2
		)`, threadID, roleID).Scan(&ok)
	return ok, err
}

// ---- T3 receipt / membership state ---------------------------------------

func InsertPendingThreadReceipt(ctx context.Context, q pg.Querier, threadID, inputEntryID, roleID int64, reason string) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO thread_receipts
		    (thread_id, input_entry_id, role_id, reason, state)
		VALUES ($1, $2, $3, $4, 'pending')
		ON CONFLICT (thread_id, input_entry_id, role_id) DO NOTHING`,
		threadID, inputEntryID, roleID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func ListPendingThreadReceiptsForRole(ctx context.Context, q pg.Querier, roleID int64) ([]model.ThreadReceipt, error) {
	rows, err := q.Query(ctx, `
		SELECT r.thread_id, r.input_entry_id, r.role_id, r.reason, r.state,
		       r.created_at, r.handled_at, r.withdrawn_at
		FROM thread_receipts r
		JOIN threads t ON t.id = r.thread_id
		JOIN thread_roles tr
		  ON tr.thread_id = r.thread_id AND tr.role_id = r.role_id
		WHERE r.role_id = $1
		  AND r.state = 'pending'
		  AND t.status = 'open'
		  AND tr.permission IN ('write', 'manage')
		ORDER BY r.created_at, r.thread_id, r.input_entry_id`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.ThreadReceipt, 0)
	for rows.Next() {
		var r model.ThreadReceipt
		if err := rows.Scan(
			&r.ThreadID, &r.InputEntryID, &r.RoleID, &r.Reason, &r.State,
			&r.CreatedAt, &r.HandledAt, &r.WithdrawnAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func FindThreadReceipt(ctx context.Context, q pg.Querier, threadID, inputEntryID, roleID int64) (*model.ThreadReceipt, error) {
	var r model.ThreadReceipt
	err := q.QueryRow(ctx, `
		SELECT thread_id, input_entry_id, role_id, reason, state,
		       created_at, handled_at, withdrawn_at
		FROM thread_receipts
		WHERE thread_id=$1 AND input_entry_id=$2 AND role_id=$3`,
		threadID, inputEntryID, roleID).Scan(
		&r.ThreadID, &r.InputEntryID, &r.RoleID, &r.Reason, &r.State,
		&r.CreatedAt, &r.HandledAt, &r.WithdrawnAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// HandlePendingThreadReceipt is the Todo-consuming CAS. RowsAffected=false
// means the input is stale (already handled/withdrawn or never pending).
func HandlePendingThreadReceipt(ctx context.Context, q pg.Querier, threadID, inputEntryID, roleID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE thread_receipts
		SET state='handled', handled_at=NOW(), withdrawn_at=NULL
		WHERE thread_id=$1 AND input_entry_id=$2 AND role_id=$3
		  AND state='pending'`, threadID, inputEntryID, roleID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func WithdrawPendingThreadReceiptsByRole(ctx context.Context, q pg.Querier, threadID, roleID int64) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE thread_receipts
		SET state='withdrawn', withdrawn_at=NOW(), handled_at=NULL
		WHERE thread_id=$1 AND role_id=$2 AND state='pending'`, threadID, roleID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func WithdrawAllPendingThreadReceipts(ctx context.Context, q pg.Querier, threadID int64) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE thread_receipts
		SET state='withdrawn', withdrawn_at=NOW(), handled_at=NULL
		WHERE thread_id=$1 AND state='pending'`, threadID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func UpdateThreadRolePermission(ctx context.Context, q pg.Querier, threadID, roleID int64, permission string, entryID *int64) error {
	var tag pgconn.CommandTag
	var err error
	if entryID == nil {
		tag, err = q.Exec(ctx, `
			UPDATE thread_roles
			SET permission=$3
			WHERE thread_id=$1 AND role_id=$2`,
			threadID, roleID, permission)
	} else {
		tag, err = q.Exec(ctx, `
			UPDATE thread_roles
			SET permission=$3, entry_id=$4
			WHERE thread_id=$1 AND role_id=$2`,
			threadID, roleID, permission, *entryID)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func DeleteThreadRole(ctx context.Context, q pg.Querier, threadID, roleID int64) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM thread_roles WHERE thread_id=$1 AND role_id=$2`,
		threadID, roleID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func SetThreadStatus(ctx context.Context, q pg.Querier, threadID int64, from, to string) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET status=$3, revision=revision+1, updated_at=NOW()
		WHERE id=$1 AND status=$2`, threadID, from, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func SetThreadSubject(ctx context.Context, q pg.Querier, threadID int64, subject string) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET subject=$2, revision=revision+1, updated_at=NOW()
		WHERE id=$1`, threadID, subject)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// ---- T3 idempotency -------------------------------------------------------

func TryInsertThreadIdempotency(ctx context.Context, q pg.Querier, roleID int64, operation, key string, requestHash []byte) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO thread_idempotency
		    (role_id, operation, idempotency_key, request_hash, result_ref)
		VALUES ($1, $2, $3, $4, '')
		ON CONFLICT (role_id, operation, idempotency_key) DO NOTHING`,
		roleID, operation, key, requestHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func FindThreadIdempotency(ctx context.Context, q pg.Querier, roleID int64, operation, key string) (*model.ThreadIdempotency, error) {
	var id model.ThreadIdempotency
	err := q.QueryRow(ctx, `
		SELECT role_id, operation, idempotency_key, request_hash, result_ref, created_at
		FROM thread_idempotency
		WHERE role_id=$1 AND operation=$2 AND idempotency_key=$3`,
		roleID, operation, key).Scan(
		&id.RoleID, &id.Operation, &id.IdempotencyKey, &id.RequestHash,
		&id.ResultRef, &id.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func CompleteThreadIdempotency(ctx context.Context, q pg.Querier, roleID int64, operation, key, resultRef string) error {
	tag, err := q.Exec(ctx, `
		UPDATE thread_idempotency
		SET result_ref=$4
		WHERE role_id=$1 AND operation=$2 AND idempotency_key=$3
		  AND result_ref=''`,
		roleID, operation, key, resultRef)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
