package service

// Room disposal (migration 035) — three doors, one core:
//
//   governor  ThreadDissolve  — dissolve a room (open or closed):
//                              settle everything, then hard-delete the
//                              aggregate; payload memories that lost
//                              their last reference go with it
//   author    ThreadScrubEntry — redact ONE own entry (content out,
//                              shell stays: others' replies and
//                              receipts never dangle)
//   platform  DisposeThreadByCode — admin action / retention sweep,
//                              same core, no agent actor
//
// Safety: disposal settles before it deletes (close semantics, plus
// undecided for judgment clocks still running — a legitimate terminal
// transition), so the turn projection never loses an open obligation;
// FK cascades carry deliveries away with their assigns; the audit row
// keeps metadata only (code, actor, time — never content).

import (
	"context"
	goerrors "errors"
	"fmt"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"

	"github.com/jackc/pgx/v5"
)

// disposeThreadTx is the shared core: settles, deletes, audits. The
// caller holds the transaction and has already resolved the thread.
func disposeThreadTx(ctx context.Context, tx pgx.Tx, th *model.Thread, actorKind, actorRef string) (repository.DisposalCounts, error) {
	if _, err := repository.ForceUndecidedThreadAssigns(ctx, tx, th.ID); err != nil {
		return repository.DisposalCounts{}, err
	}
	if _, err := repository.VoidUndeliveredAssigns(ctx, tx, th.ID); err != nil {
		return repository.DisposalCounts{}, err
	}
	if _, err := repository.WithdrawAllThreadReceipts(ctx, tx, th.ID, "close"); err != nil {
		return repository.DisposalCounts{}, err
	}
	counts, err := repository.DisposeThreadCascade(ctx, tx, th.ID)
	if err != nil {
		return repository.DisposalCounts{}, err
	}
	if err := repository.RecordThreadDisposal(ctx, tx, th.Code, actorKind, actorRef); err != nil {
		return repository.DisposalCounts{}, err
	}
	return counts, nil
}

// ThreadDissolve disposes a room (governor only). Idempotent like
// every write tool; a dissolved room answers THREAD_NOT_FOUND
// afterwards (the object is gone, not hidden).
func ThreadDissolve(ctx context.Context, pool *pg.Pool, botID int64, code, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"thread":%q}`, code))
	return runThreadAction(ctx, pool, botID, "thread_dissolve", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, code)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
			}
			if th == nil {
				return threadActionOutcome{}, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
			}
			if me == nil {
				return threadActionOutcome{}, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
			}
			if me.Role != repository.ThreadRoleGovernor {
				return threadActionOutcome{}, errors.New(403, "NOT_GOVERNOR", "Only a governor may dissolve this thread")
			}
			counts, err := disposeThreadTx(ctx, tx, th, "governor", fmt.Sprintf("bot:%d", botID))
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_dissolve")
			}
			return threadActionOutcome{
				Facts: map[string]any{
					"thread": th.Code, "disposed": true,
					"entries_removed": counts.Entries, "assigns_removed": counts.Assigns,
					"payload_memories_removed": counts.PayloadMemories,
				},
				View: threadView([]string{"thread_start"}),
			}, nil
		})
}

// ThreadScrubEntry: an author redacts the CONTENT of one own entry.
// The entry shell (seq, reply_to, asked) survives — other members'
// replies and receipts reference the entry, not the content.
func ThreadScrubEntry(ctx context.Context, pool *pg.Pool, botID int64, code string, entry int64, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"thread":%q,"entry":%d}`, code, entry))
	return runThreadAction(ctx, pool, botID, "thread_scrub", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, code)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
			}
			if th == nil {
				return threadActionOutcome{}, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
			}
			if me == nil {
				return threadActionOutcome{}, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
			}
			author, err := repository.EntryAuthor(ctx, tx, th.ID, entry)
			if goerrors.Is(err, pgx.ErrNoRows) {
				return threadActionOutcome{}, errors.New(404, "NOT_FOUND", "Entry not found in this thread")
			}
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading entry")
			}
			if author != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "Only the entry's author may scrub it")
			}
			payload, err := repository.EntryPayload(ctx, tx, entry)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading entry")
			}
			ok, err := repository.ScrubThreadEntry(ctx, tx, th.ID, entry, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_scrub")
			}
			if !ok {
				return threadActionOutcome{}, errors.New(404, "NOT_FOUND", "Entry not found in this thread")
			}
			payloadGone := false
			if payload > 0 {
				payloadGone, err = repository.DeleteUnreferencedPayload(ctx, tx, payload)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_scrub")
				}
			}
			return threadActionOutcome{
				Facts: map[string]any{
					"thread": th.Code, "entry": entry, "redacted": true,
					"payload_memory_removed": payloadGone,
				},
				View: threadView([]string{"thread_get"}),
			}, nil
		})
}

// DisposeThreadByCode: the platform door (admin action or retention
// sweep) — same core, no agent actor.
func DisposeThreadByCode(ctx context.Context, pool *pg.Pool, code, actorKind, actorRef string) (map[string]any, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error beginning disposal")
	}
	defer func() { _ = pg.Rollback(tx) }()
	th, err := repository.LockThreadByCode(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
	}
	if th == nil {
		return nil, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
	}
	counts, err := disposeThreadTx(ctx, tx, th, actorKind, actorRef)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during room disposal")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error committing disposal")
	}
	return map[string]any{
		"thread": th.Code, "disposed": true,
		"entries_removed": counts.Entries, "assigns_removed": counts.Assigns,
		"payload_memories_removed": counts.PayloadMemories,
	}, nil
}

// RetentionSweep disposes closed rooms older than the cutoff. The
// retention policy itself is configuration (RETENTION_DAYS, 0 = off);
// this is only the broom.
func RetentionSweep(ctx context.Context, pool *pg.Pool, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	_, codes, err := repository.ClosedThreadsOlderThan(ctx, pool, cutoff)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, code := range codes {
		if _, err := DisposeThreadByCode(ctx, pool, code, "retention", ""); err != nil {
			continue // best effort per room; the next sweep retries
		}
		n++
	}
	return n, nil
}

// PayloadSweep finishes what disposal deferred: a payload memory kept
// alive at dispose time by a live engagement, contract or delivery
// becomes deletable the moment that protection ends — this pass is
// the retrigger (unlike rooms, orphaned payloads have no event left
// to ride on). Always on; the eligibility core stays the single
// judge, so a still-protected payload simply stays.
func PayloadSweep(ctx context.Context, pool *pg.Pool, batch int) (int, error) {
	ids, err := repository.OrphanThreadPayloadIDs(ctx, pool, batch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		tx, err := pool.TxBegin(ctx)
		if err != nil {
			return n, err
		}
		gone, err := repository.DeleteUnreferencedPayload(ctx, tx, id)
		if err != nil {
			_ = pg.Rollback(tx)
			continue // best effort per payload; the next sweep retries
		}
		if err := tx.Commit(ctx); err != nil {
			return n, err
		}
		if gone {
			n++
		}
	}
	return n, nil
}
