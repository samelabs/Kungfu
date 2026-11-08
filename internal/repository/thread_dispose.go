package repository

// Room disposal (migration 035): the end-of-life path. One
// transaction settles what is still open (close semantics plus
// undecided for judgment clocks still running), removes the room
// aggregate, and drops payload memories (origin='thread') that lost
// their last LIVE reference — entries anywhere, live engagements,
// non-closed task contracts and unjudged deliveries all keep their
// payload readable (DeleteUnreferencedPayload). Standalone memories
// never go — their life is the author's (memory_delete), not the
// room's.

import (
	"context"
	"errors"

	"kungfu.md/internal/pg"

	"github.com/jackc/pgx/v5"
)

// DisposalCounts reports what one disposal removed.
type DisposalCounts struct {
	Entries         int
	Assigns         int
	PayloadMemories int
}

// ForceUndecidedThreadAssigns settles every delivered assignment of
// one room whose judgment clock is still running: the disposal is the
// deadline (§6.4 undecided — not held against the taker).
func ForceUndecidedThreadAssigns(ctx context.Context, q pg.Querier, threadID int64) (int, error) {
	tag, err := q.Exec(ctx, `
		UPDATE assigns SET state = 'undecided'
		WHERE thread_id = $1 AND state = 'delivered'`, threadID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DisposeThreadCascade removes one room's aggregate inside the
// caller's transaction and returns what went. Payload memories are
// reference-counted: an origin='thread' row is deleted only when no
// live object anywhere still needs it (DeleteUnreferencedPayload —
// the same payload may be pinned in another room, by an engagement,
// or by an undelivered-against task contract).
func DisposeThreadCascade(ctx context.Context, q pg.Querier, threadID int64) (DisposalCounts, error) {
	var c DisposalCounts

	// what this room's entries pin — collected before the rows go
	rows, err := q.Query(ctx, `
		SELECT DISTINCT memory_id FROM thread_entries WHERE thread_id = $1 AND memory_id IS NOT NULL`, threadID)
	if err != nil {
		return c, err
	}
	var payloads []int64
	for rows.Next() {
		var m int64
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return c, err
		}
		payloads = append(payloads, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	// deletion order follows the no-action FKs (children first):
	// receipts → entries, assigns → entries, entries → memory/reply,
	// members → threads; deliveries cascade with their assigns
	if _, err := q.Exec(ctx, `DELETE FROM thread_receipts WHERE thread_id = $1`, threadID); err != nil {
		return c, err
	}
	if err := bump(ctx, q, &c.Assigns, `DELETE FROM assigns WHERE thread_id = $1`, threadID); err != nil {
		return c, err
	}
	if err := bump(ctx, q, &c.Entries, `DELETE FROM thread_entries WHERE thread_id = $1`, threadID); err != nil {
		return c, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM thread_members WHERE thread_id = $1`, threadID); err != nil {
		return c, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM threads WHERE id = $1`, threadID); err != nil {
		return c, err
	}

	// reference-counted payload disposal: the shared eligibility core
	// decides per payload whether its last live reference went with
	// this room (a never-updated payload has no archived rows)
	for _, m := range payloads {
		gone, err := DeleteUnreferencedPayload(ctx, q, m)
		if err != nil {
			return c, err
		}
		if gone {
			c.PayloadMemories++
		}
	}
	return c, nil
}

func bump(ctx context.Context, q pg.Querier, slot *int, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	*slot = int(tag.RowsAffected())
	return nil
}

// RecordThreadDisposal writes the metadata-only audit row. An empty
// actorRef (the retention sweep has no actor identity) stores NULL —
// the column's default, not a placeholder string.
func RecordThreadDisposal(ctx context.Context, q pg.Querier, code, actorKind, actorRef string) error {
	ref := any(nil)
	if actorRef != "" {
		ref = actorRef
	}
	_, err := q.Exec(ctx, `
		INSERT INTO thread_disposals (code, actor_kind, actor_ref) VALUES ($1, $2, $3)`,
		code, actorKind, ref)
	return err
}

// ClosedThreadsOlderThan lists rooms closed before the cutoff (the
// retention sweep's candidate set).
func ClosedThreadsOlderThan(ctx context.Context, q pg.Querier, cutoffISO string) ([]int64, []string, error) {
	rows, err := q.Query(ctx, `
		SELECT id, code FROM threads
		WHERE status = 'closed' AND closed_at IS NOT NULL AND closed_at < $1
		ORDER BY id LIMIT 100`, cutoffISO)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var codes []string
	for rows.Next() {
		var id int64
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		codes = append(codes, code)
	}
	return ids, codes, rows.Err()
}

// EntryAuthor returns the author of one entry (0 when missing).
func EntryAuthor(ctx context.Context, q pg.Querier, threadID, entryID int64) (int64, error) {
	var author int64
	err := q.QueryRow(ctx, `
		SELECT author_id FROM thread_entries WHERE id = $1 AND thread_id = $2`, entryID, threadID).Scan(&author)
	return author, err
}

// ScrubThreadEntry redacts an author's own entry: summary cleared,
// payload reference removed — the shell (seq, reply_to, asked) stays
// so other members' references never dangle. Returns whether the row
// was the author's and existed.
func ScrubThreadEntry(ctx context.Context, q pg.Querier, threadID, entryID, authorID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE thread_entries
		SET summary = NULL, memory_id = NULL, memory_revision = NULL
		WHERE id = $1 AND thread_id = $2 AND author_id = $3`, entryID, threadID, authorID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// EntryPayload returns the entry's payload memory id (0 when the
// entry has none — already scrubbed; migration 035 made the column
// nullable, so the NULL must coalesce, not scan).
func EntryPayload(ctx context.Context, q pg.Querier, entryID int64) (int64, error) {
	var m int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(memory_id, 0) FROM thread_entries WHERE id = $1`, entryID).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return m, err
}

// DeleteUnreferencedPayload removes one thread-payload memory when no
// LIVE object still needs it, then its revision archive (a
// never-updated payload has no archived rows at all). A reference is
// live by its OWN object's lifecycle, not by existing:
//
//   - thread_entries   any entry anywhere (the payload may be pinned
//     in another room) — always live while the row is
//   - engagements      a claim pin (claim_harness_revisions) is live
//     only while the claim is active AND unexpired;
//     pins of ended engagements are dropped here in
//     the same transaction (dead pins are the one FK
//     that would otherwise block this delete forever)
//   - task contracts   harness_refs of a non-closed task's current
//     contract (a closed task can never reopen; its
//     live engagements protect their own pins above)
//   - room deliveries  memories_json of an assignment still awaiting
//     judgment (state 'delivered'); once judged the
//     delivery is history, not a live input
//
// Standalone memories never go by this path (origin predicate) —
// their life is the author's (memory_delete).
func DeleteUnreferencedPayload(ctx context.Context, q pg.Querier, memoryID int64) (bool, error) {
	if _, err := q.Exec(ctx, `
		DELETE FROM claim_harness_revisions p
		USING tb_task_claim c
		WHERE p.memory_id = $1 AND p.claim_id = c.claim_id
		  AND NOT (c.status = 'active' AND c.expires_at > NOW())`, memoryID); err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `
		DELETE FROM tb_kungfus k
		WHERE k.id = $1 AND k.origin = 'thread'
		  AND NOT EXISTS (SELECT 1 FROM thread_entries e WHERE e.memory_id = k.id)
		  AND NOT EXISTS (SELECT 1 FROM claim_harness_revisions p
		                  JOIN tb_task_claim c ON c.claim_id = p.claim_id
		                  WHERE p.memory_id = k.id
		                    AND c.status = 'active' AND c.expires_at > NOW())
		  AND NOT EXISTS (SELECT 1 FROM tb_task t
		                  WHERE t.status <> 'closed'
		                    AND t.contract->'harness_refs' ? k.code)
		  AND NOT EXISTS (SELECT 1 FROM assign_deliveries d
		                  JOIN assigns a ON a.id = d.assign_id
		                  WHERE a.state = 'delivered'
		                    AND d.memories_json @> jsonb_build_array(jsonb_build_object('code', k.code)))`,
		memoryID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM memory_revisions WHERE memory_id = $1`, memoryID); err != nil {
		return false, err
	}
	return true, nil
}

// OrphanThreadPayloadIDs lists thread-payload memories that no entry
// references at all — candidates whose cross-object protection may
// have expired since their room went away (the deferred half of
// DeleteUnreferencedPayload: protection lifted later still ends in
// deletion, on the next sweep).
func OrphanThreadPayloadIDs(ctx context.Context, q pg.Querier, limit int) ([]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT k.id FROM tb_kungfus k
		WHERE k.origin = 'thread'
		  AND NOT EXISTS (SELECT 1 FROM thread_entries e WHERE e.memory_id = k.id)
		ORDER BY k.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
