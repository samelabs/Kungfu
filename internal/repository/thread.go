package repository

// Thread core persistence (D2) — kungfu.md §6.1, §6.2, §6.5, §4, L3.
// Every method accepts a pg.Querier so it works with both *pg.Pool
// and pgx.Tx; room mutations run inside one caller transaction that
// first locks the thread row (LockThreadByCode/LockThreadByID), which
// is what serializes competing joins, key operations and closes on
// one room (L5).

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
)

// Thread vocabulary (kungfu.md §6.1–§6.5).
const (
	ThreadStatusOpen   = "open"
	ThreadStatusClosed = "closed"

	ThreadRoleGovernor = "governor"
	ThreadRoleSpeaker  = "speaker"
	ThreadRoleObserver = "observer"
)

// -- threads --

// GenerateUniqueThreadCode generates a 12-hex code not yet used by
// any thread.
func GenerateUniqueThreadCode(ctx context.Context, q pg.Querier) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM threads WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

// InsertThread creates an open room; subject may be NULL. Returns
// the new thread id. The creator's governor membership row is a
// separate InsertThreadMember in the same caller transaction.
func InsertThread(ctx context.Context, q pg.Querier, code string, subject *string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO threads (code, subject) VALUES ($1, $2) RETURNING id`,
		code, subject).Scan(&id)
	return id, err
}

// FindThreadByCode returns the thread row, or nil when no such code.
func FindThreadByCode(ctx context.Context, q pg.Querier, code string) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, `
		SELECT id, code, subject, status, key_hash, key_role, key_issued_at, next_seq, created_at, closed_at
		FROM threads WHERE code = $1`, code))
}

// LockThreadByCode takes the row lock on the room inside the
// caller's transaction. Every mutation path (key issue/revoke, join,
// member governance, close) locks the room first so competing
// actions serialize (L5).
func LockThreadByCode(ctx context.Context, q pg.Querier, code string) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, `
		SELECT id, code, subject, status, key_hash, key_role, key_issued_at, next_seq, created_at, closed_at
		FROM threads WHERE code = $1 FOR UPDATE`, code))
}

// LockThreadByID is LockThreadByCode by id.
func LockThreadByID(ctx context.Context, q pg.Querier, id int64) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, `
		SELECT id, code, subject, status, key_hash, key_role, key_issued_at, next_seq, created_at, closed_at
		FROM threads WHERE id = $1 FOR UPDATE`, id))
}

// FindOpenThreadByKeyHash resolves an active key to its open room, or
// nil. sha256(raw key) → at most one row (single-active-key group);
// closed rooms have no key (§6.5), so a dead key finds nothing.
func FindOpenThreadByKeyHash(ctx context.Context, q pg.Querier, keyHash string) (*model.Thread, error) {
	return scanThread(q.QueryRow(ctx, `
		SELECT id, code, subject, status, key_hash, key_role, key_issued_at, next_seq, created_at, closed_at
		FROM threads WHERE key_hash = $1 AND status = 'open'`, keyHash))
}

// IssueThreadKey replaces the whole key group in one statement: the
// previous key (if any) dies in this same transaction (§6.1 —
// signing a new key revokes the old one).
func IssueThreadKey(ctx context.Context, q pg.Querier, threadID int64, keyHash, keyRole string) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET key_hash = $1, key_role = $2, key_issued_at = NOW()
		WHERE id = $3`, keyHash, keyRole, threadID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// RevokeThreadKey NULLs the whole key group (§6.1 — revocation on
// its own, without signing a new key).
func RevokeThreadKey(ctx context.Context, q pg.Querier, threadID int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET key_hash = NULL, key_role = NULL, key_issued_at = NULL
		WHERE id = $1`, threadID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// CloseThreadByID closes an OPEN room (§6.5): terminal status,
// closed_at recorded, the key group cleared. Returns false when the
// room is already closed. Memberships are kept (read-only room).
func CloseThreadByID(ctx context.Context, q pg.Querier, threadID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE threads
		SET status = 'closed', closed_at = NOW(),
		    key_hash = NULL, key_role = NULL, key_issued_at = NULL
		WHERE id = $1 AND status = 'open'`, threadID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func scanThread(row pgx.Row) (*model.Thread, error) {
	var (
		t           model.Thread
		createdAt   time.Time
		closedAt    *time.Time
		keyIssuedAt *time.Time
	)
	if err := row.Scan(&t.ID, &t.Code, &t.Subject, &t.Status,
		&t.KeyHash, &t.KeyRole, &keyIssuedAt, &t.NextSeq, &createdAt, &closedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t.CreatedAt = timeToStr(createdAt)
	if closedAt != nil {
		s := timeToStr(*closedAt)
		t.ClosedAt = &s
	}
	if keyIssuedAt != nil {
		s := timeToStr(*keyIssuedAt)
		t.KeyIssuedAt = &s
	}
	return &t, nil
}

// -- thread_members --

// FindThreadMember returns the membership row, or nil.
func FindThreadMember(ctx context.Context, q pg.Querier, threadID, accountID int64) (*model.ThreadMember, error) {
	row := q.QueryRow(ctx, `
		SELECT thread_id, account_id, role, joined_via_key_hash, joined_at
		FROM thread_members WHERE thread_id = $1 AND account_id = $2`, threadID, accountID)
	return scanThreadMember(row)
}

func scanThreadMember(row pgx.Row) (*model.ThreadMember, error) {
	var (
		m        model.ThreadMember
		joinedAt time.Time
	)
	if err := row.Scan(&m.ThreadID, &m.AccountID, &m.Role, &m.JoinedViaKeyHash, &joinedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	m.JoinedAt = timeToStr(joinedAt)
	return &m, nil
}

// InsertThreadMember adds one membership. The (thread_id, account_id)
// primary key rejects a second row — the caller checks for an
// existing membership first (L2 duplicate join) under the room lock.
func InsertThreadMember(ctx context.Context, q pg.Querier, threadID, accountID int64, role string, joinedViaKeyHash *string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO thread_members (thread_id, account_id, role, joined_via_key_hash)
		VALUES ($1, $2, $3, $4)`, threadID, accountID, role, joinedViaKeyHash)
	return err
}

// DeleteThreadMember terminates one membership (leave, removal, §4
// deactivation). Returns false when no such row existed.
func DeleteThreadMember(ctx context.Context, q pg.Querier, threadID, accountID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM thread_members WHERE thread_id = $1 AND account_id = $2`,
		threadID, accountID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetThreadMemberRole changes one member's role (§6.2).
func SetThreadMemberRole(ctx context.Context, q pg.Querier, threadID, accountID int64, role string) error {
	tag, err := q.Exec(ctx, `
		UPDATE thread_members SET role = $3 WHERE thread_id = $1 AND account_id = $2`,
		threadID, accountID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountThreadMembers counts the room's members (L4 cap ≤ 50).
func CountThreadMembers(ctx context.Context, q pg.Querier, threadID int64) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_members WHERE thread_id = $1`, threadID).Scan(&n)
	return n, err
}

// CountThreadGovernors counts the room's governors — the §6.2
// invariant (an open room always keeps ≥ 1) and the LAST_MANAGER
// guard are checked against this under the room lock.
func CountThreadGovernors(ctx context.Context, q pg.Querier, threadID int64) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_members WHERE thread_id = $1 AND role = 'governor'`,
		threadID).Scan(&n)
	return n, err
}

// CountOpenThreadMembershipsByAccount counts the OPEN rooms the
// account is a member of (L4 cap ≤ 100; applies to creating and
// joining alike).
func CountOpenThreadMembershipsByAccount(ctx context.Context, q pg.Querier, accountID int64) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM thread_members m
		JOIN threads t ON t.id = m.thread_id
		WHERE m.account_id = $1 AND t.status = 'open'`, accountID).Scan(&n)
	return n, err
}

// ThreadMemberRow is the member-list projection for thread_get (§9:
// members read the member table). BotName joins in for display.
type ThreadMemberRow struct {
	AccountID        int64
	BotName          string
	Role             string
	JoinedViaKeyHash *string
	JoinedAt         string
}

// ListThreadMembers returns the room's members ordered by join time
// (oldest first — the creator, then admission order).
func ListThreadMembers(ctx context.Context, q pg.Querier, threadID int64) ([]ThreadMemberRow, error) {
	rows, err := q.Query(ctx, `
		SELECT m.account_id, b.bot_name, m.role, m.joined_via_key_hash, m.joined_at
		FROM thread_members m
		JOIN tb_bots b ON b.id = m.account_id
		WHERE m.thread_id = $1
		ORDER BY m.joined_at, m.account_id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadMemberRow
	for rows.Next() {
		var r ThreadMemberRow
		var joinedAt time.Time
		if err := rows.Scan(&r.AccountID, &r.BotName, &r.Role, &r.JoinedViaKeyHash, &joinedAt); err != nil {
			return nil, err
		}
		r.JoinedAt = timeToStr(joinedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ThreadListItem is one thread_list row (§8): the room, my role, the
// two membership facts. Open-item counts arrive with the D5 turn
// projection.
type ThreadListItem struct {
	Code             string
	Subject          *string
	Status           string
	Role             string
	JoinedViaKeyHash *string
	JoinedAt         string
	ID               int64 // pagination cursor, opaque to callers
}

// ListThreadsForMember returns the account's rooms, newest room
// first, keyset-paginated by the opaque cursor (the previous page's
// last item id); status "" means both. limit ≤ 50 (§2).
func ListThreadsForMember(ctx context.Context, q pg.Querier, accountID int64, status string, beforeID int64, limit int) ([]ThreadListItem, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.code, t.subject, t.status, m.role, m.joined_via_key_hash, m.joined_at
		FROM thread_members m
		JOIN threads t ON t.id = m.thread_id
		WHERE m.account_id = $1
		  AND ($2 = '' OR t.status = $2)
		  AND ($3 = 0 OR t.id < $3)
		ORDER BY t.id DESC
		LIMIT $4`, accountID, status, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadListItem
	for rows.Next() {
		var it ThreadListItem
		var joinedAt time.Time
		if err := rows.Scan(&it.ID, &it.Code, &it.Subject, &it.Status, &it.Role,
			&it.JoinedViaKeyHash, &joinedAt); err != nil {
			return nil, err
		}
		it.JoinedAt = timeToStr(joinedAt)
		out = append(out, it)
	}
	return out, rows.Err()
}

// -- thread_idempotency (L3) --

// FindThreadIdempotency returns the stored receipt for (account,
// tool, key), or nil.
func FindThreadIdempotency(ctx context.Context, q pg.Querier, accountID int64, tool, key string) (*model.ThreadIdempotency, error) {
	row := q.QueryRow(ctx, `
		SELECT account_id, tool, key, request_hash, result_snapshot::text, created_at
		FROM thread_idempotency
		WHERE account_id = $1 AND tool = $2 AND key = $3`, accountID, tool, key)
	var (
		r         model.ThreadIdempotency
		createdAt time.Time
	)
	if err := row.Scan(&r.AccountID, &r.Tool, &r.Key, &r.RequestHash, &r.ResultSnapshot, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.CreatedAt = timeToStr(createdAt)
	return &r, nil
}

// InsertThreadIdempotency stores the first success's receipt. The
// (account_id, tool, key) primary key makes the logical request
// unique: a concurrent duplicate insert fails with a unique
// violation, which the caller resolves by re-reading (replay or
// conflict — never a second execution).
func InsertThreadIdempotency(ctx context.Context, q pg.Querier, accountID int64, tool, key, requestHash, resultSnapshot string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO thread_idempotency (account_id, tool, key, request_hash, result_snapshot)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, accountID, tool, key, requestHash, resultSnapshot)
	return err
}

// -- §4 deactivation cascade --

// TerminateAccountThreadMemberships runs the account-deactivation
// cascade (kungfu.md §4) inside the caller's transaction: every
// membership of the account ends, and every OPEN room that would be
// left with no governor closes in the same transaction with the
// §6.5 effects (key group cleared, members kept read-only). Returns
// the codes of the rooms closed by this cascade.
// LockAccountRow serializes room-count checks (thread_start / thread_join
// open-room limit) on the acting account: two concurrent starts otherwise
// both count <100 and both commit, overshooting the cap (audit P1-B).
func LockAccountRow(ctx context.Context, q pg.Querier, botID int64) error {
	_, err := q.Exec(ctx, `SELECT id FROM tb_bots WHERE id = $1 FOR UPDATE`, botID)
	return err
}

func TerminateAccountThreadMemberships(ctx context.Context, q pg.Querier, botID int64) ([]string, error) {
	// Serialize concurrent cascades on the same rooms (audit P1-E): two
	// admins disabling the last two governors of one thread otherwise both
	// see "another governor remains" and neither closes it. The lock runs
	// as its own statement so the decision UPDATE below takes a fresh
	// snapshot and sees the memberships the transaction we waited on
	// already deleted.
	lockRows, err := q.Query(ctx, `
		SELECT t.id FROM threads t
		WHERE t.status = 'open'
		  AND EXISTS (SELECT 1 FROM thread_members g
		              WHERE g.thread_id = t.id AND g.account_id = $1 AND g.role = 'governor')
		FOR UPDATE`, botID)
	if err != nil {
		return nil, err
	}
	for lockRows.Next() {
	}
	if err := lockRows.Err(); err != nil {
		lockRows.Close()
		return nil, err
	}
	lockRows.Close()

	rows, err := q.Query(ctx, `
		UPDATE threads t
		SET status = 'closed', closed_at = NOW(),
		    key_hash = NULL, key_role = NULL, key_issued_at = NULL
		WHERE t.status = 'open'
		  AND EXISTS (SELECT 1 FROM thread_members g
		              WHERE g.thread_id = t.id AND g.account_id = $1 AND g.role = 'governor')
		  AND NOT EXISTS (SELECT 1 FROM thread_members o
		                  WHERE o.thread_id = t.id AND o.account_id <> $1 AND o.role = 'governor')
		RETURNING t.code`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var closed []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		closed = append(closed, code)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM thread_members WHERE account_id = $1`, botID); err != nil {
		return nil, err
	}
	return closed, nil
}
