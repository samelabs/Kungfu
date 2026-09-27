package service

// Recovery pass (WO-5b): the uncertain redelivery loop, the spec
// §5.4 / §10.4 time bound. The worker leases their rows (SKIP LOCKED + updated_at refresh), recheck
// under the row lock before writing, and reuse the WO-5a delivery
// outcome machinery (same Idempotency-Key redeliveries, §7.2 mapping,
// §7.3 fault counting).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// Recovery cadence and windows (§5.4, §10.4, §11).
const (
	uncertainUnresolvedAfter = 24 * time.Hour
)

// RecoverSubmissions runs one recovery pass over leased rows:
//
//   - a submission stuck in delivering for > 15s first transitions to
//     uncertain (the §5.4 timeout edge), then continues below;
//   - uncertain for ≥ 24h (since it FIRST entered uncertain) becomes
//     failed(DELIVERY_UNRESOLVED) with its reservation released and
//     joins the §7.3 fault count;
//   - otherwise the submission is redelivered via DeliverSubmission
//     (same Idempotency-Key, §7.2 mapping unchanged).
//
// It returns the number of submissions handled in this pass.
func RecoverSubmissions(ctx context.Context, pool *pg.Pool, agentRefKey []byte, now time.Time, batch int) (int, error) {
	ids, err := repository.LeaseRecoverableSubmissions(ctx, pool, now, batch)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, id := range ids {
		sub, err := repository.FindSubmissionByID(ctx, pool, id)
		if err != nil || sub == nil {
			continue
		}
		if sub.State == task.SubDelivering {
			// §5.4: delivering ≤ 15s, then uncertain. The conversion
			// ENDS this round for the row — redelivery follows the 30s
			// uncertain cadence on a later pass (§7c).
			if err := writeDeliveryOutcome(ctx, pool, sub, task.EventTimeout, nil, nil, nil); err == nil {
				handled++
			}
			continue
		}
		if sub.State == task.SubUncertain {
			if since, err := repository.UncertainSince(ctx, pool, id); err == nil &&
				now.Sub(since) >= uncertainUnresolvedAfter {
				reason := "DELIVERY_UNRESOLVED"
				if err := writeDeliveryOutcome(ctx, pool, sub, task.EventUnresolved,
					&repository.SetSubmissionStateOpts{Failure: &reason}, nil,
					func(ctx context.Context, tx pgx.Tx) error {
						return repository.ReleaseTaskReservation(ctx, tx, sub.TaskID, sub.Amount)
					}); err == nil {
					_ = maybePauseForReceiverFault(ctx, pool, sub.TaskID)
				}
				handled++
				continue
			}
			// redelivery cadence (§5.4: every 30s) — the lease already
			// guarantees updated_at <= now-30s, i.e. at least 30s since
			// entering uncertain or the last attempt
			if _, err := DeliverSubmission(ctx, pool, id, agentRefKey, now); err != nil {
				continue
			}
		}
		handled++
	}
	return handled, nil
}
