package service

// Task 1.2 (WO-32) — §7.2 restricted audience on the service layer:
// name resolution at create, the immutability rule at update, and
// the one visibility gate every work-surface read of a task passes.
// The gate maps an out-of-audience caller to the SAME TASK_NOT_FOUND
// a missing task produces — same code, same message, same lookup
// path (§12 minimal disclosure: a restricted task's existence is
// known only to the author and the agents it names).

import (
	"context"
	"fmt"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// resolveAudience resolves a restricted audience's names to account
// ids (§7.2): every name must be a known agent — any status, an
// audience seat is a fact fixed at creation, and a re-enabled
// account picks its seats back up — and must not be the publisher
// (§7.2: the author MUST NOT take its own task, so naming itself is
// a validation error, not a silent no-op). All violations are
// reported at once, harness_refs-style. An open audience resolves to
// nothing.
func resolveAudience(ctx context.Context, pool *pg.Pool, publisherID int64, aud task.Audience) ([]int64, error) {
	if aud.Type != task.AudienceRestricted {
		return nil, nil
	}
	var errs []task.FieldError
	ids := make([]int64, 0, len(aud.Agents))
	for i, name := range aud.Agents {
		id, err := repository.FindBotIDByName(ctx, pool, name)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if id == nil {
			errs = append(errs, task.FieldError{
				Field:   fmt.Sprintf("audience.agents[%d]", i),
				Message: fmt.Sprintf("%q is not a known agent name", name),
			})
			continue
		}
		if *id == publisherID {
			errs = append(errs, task.FieldError{
				Field:   fmt.Sprintf("audience.agents[%d]", i),
				Message: fmt.Sprintf("%q is you: the publisher may not be part of the audience (you cannot take your own task)", name),
			})
			continue
		}
		ids = append(ids, *id)
	}
	if len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	return ids, nil
}

// requireTaskAudience is the §9/§12 gate of the work surface: the
// publisher and (for a restricted task) its named agents may read
// and act; everyone else gets the exact TASK_NOT_FOUND of a missing
// task. The audience is immutable, so one check at lookup time
// covers the whole operation — nothing can change it mid-flight.
func requireTaskAudience(ctx context.Context, q pg.Querier, t *repository.TaskRow, agentID int64) error {
	if t.PublisherID == agentID {
		return nil // §9: the author always reads their own task
	}
	ok, err := repository.TaskAudienceVisible(ctx, q, t.ID, agentID)
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if !ok {
		return errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	return nil
}

// requireOwnerOrAudience is the publisher-side gate (WO-32a): the
// publisher proceeds; anyone else first passes the audience gate —
// an out-of-audience caller gets the missing-task TASK_NOT_FOUND
// (§12: the publisher tools must not become an existence oracle),
// while an in-audience non-publisher keeps hearing NOT_OWNER. Open
// tasks behave exactly as before (the gate passes everyone).
func requireOwnerOrAudience(ctx context.Context, q pg.Querier, t *repository.TaskRow, callerID int64) error {
	if t.PublisherID == callerID {
		return nil
	}
	if err := requireTaskAudience(ctx, q, t, callerID); err != nil {
		return err
	}
	return errors.New(0, "NOT_OWNER", "Not your task")
}

// offeredWorkCount is the §8 "tasks" side of the opportunities
// projection: restricted tasks naming the agent that are still
// offerable (open, slots, eligibility, no active claim, active
// account). Offered, never owed — the number binds no one.
func offeredWorkCount(ctx context.Context, pool *pg.Pool, agentID int64, now time.Time) (int64, error) {
	return repository.CountOfferedWork(ctx, pool, agentID, now)
}
