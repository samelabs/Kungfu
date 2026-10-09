package service

// Claim mechanism — spec §5.2 (claim / renew / release / expiry) with
// the §5.3 step-5 limit tallies. Every operation is one transaction
// with the task (or claim) row locked; time comes from the caller's
// `now` so tests control the clock. Error codes are the §8.4 executor
// catalogue, verbatim: TASK_NOT_FOUND, TASK_NOT_OPEN (details.status),
// OWN_TASK, SUBMISSION_LIMIT (details.limit), SLOTS_EXHAUSTED,
// CLAIM_INVALID.

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// taskNotOpen is the shared §8.4 TASK_NOT_OPEN error: the status plus,
// when the platform recorded one, the reason (paused_reason for a
// platform pause, closed_reason for a platform close) so executors
// learn why the task stopped taking work.
func taskNotOpen(t *repository.TaskRow) *errors.AppError {
	details := map[string]interface{}{"status": t.Status}
	if t.Status == task.TaskPaused && t.PausedReason != nil {
		details["reason"] = *t.PausedReason
	}
	if t.Status == task.TaskClosed && t.ClosedReason != nil {
		details["reason"] = *t.ClosedReason
	}
	return errors.NewWithDetails(0, "TASK_NOT_OPEN",
		fmt.Sprintf("Task is %s, not open", t.Status), details)
}

// claimView is the §5.2 return structure. ContractVersion is the
// contract version this engagement bound (Task 1.1 §7.1) —
// submissions under this claim are checked and delivered against
// it, whatever the task's current version says later.
type claimView struct {
	ClaimID         WireID    `json:"claim_id"`
	TaskCode        string    `json:"task_code"`
	ContractVersion int64     `json:"contract_version"`
	ExpiresAt       time.Time `json:"expires_at"`
	Deadline        time.Time `json:"deadline"`
	Amount          int64     `json:"amount"`
	Status          string    `json:"status"`
}

func newClaimView(c *repository.ClaimRow, code string) claimView {
	return claimView{
		ClaimID:  WireID(c.ClaimID),
		TaskCode: code,

		ContractVersion: c.ContractVersion,
		ExpiresAt:       c.ExpiresAt,
		Deadline:        c.Deadline,
		Amount:          c.Amount,
		Status:          c.Status,
	}
}

// effectiveContract decodes the task's one current contract.
func effectiveContract(ctx context.Context, q pg.Querier, t *repository.TaskRow) (task.Contract, error) {
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return task.Contract{}, err
	}
	return contract, nil
}

// harnessSnapshotEntry is one input version of an engagement's
// harness, as recorded on claim-less submissions (Task 1.1 §7.1).
type harnessSnapshotEntry struct {
	Code     string `json:"code"`
	Revision int64  `json:"revision"`
}

// resolvedHarnessPin is one harness_ref resolved against the
// publisher's CURRENT memories.
type resolvedHarnessPin struct {
	Code     string
	MemoryID int64
	Revision int64
}

// resolveHarnessRefs resolves a contract's harness_refs to the
// publisher's current memory revisions (Task 1.1 §7.3: the
// engagement binds the required input versions). A ref whose memory
// is already deleted resolves to nothing — its reads keep the live
// HARNESS_REF_NOT_FOUND behavior of Task 1.0.
func resolveHarnessRefs(ctx context.Context, q pg.Querier, publisherID int64, refs []string) ([]resolvedHarnessPin, error) {
	pins := make([]resolvedHarnessPin, 0, len(refs))
	for _, ref := range refs {
		k, err := repository.FindOwnedActiveKungfuByCode(ctx, q, publisherID, ref)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if k == nil {
			continue
		}
		pins = append(pins, resolvedHarnessPin{Code: k.Code, MemoryID: k.ID, Revision: k.Revision})
	}
	return pins, nil
}

// ClaimTask is the §5.2 work_claim operation: reserve one price under
// a new claim (idempotent per agent+task — an existing active claim is
// returned as-is).
func ClaimTask(ctx context.Context, pool *pg.Pool, agentID int64, code string, now time.Time) (claimView, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	t, err := repository.FindTaskByCodeForUpdate(ctx, tx, code)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return claimView{}, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t.Status != task.TaskOpen {
		return claimView{}, taskNotOpen(t)
	}
	if t.PublisherID == agentID {
		return claimView{}, errors.New(0, "OWN_TASK", "You cannot claim your own task")
	}

	// Idempotency (spec §5.2 amendment): an existing active claim is
	// returned without a new reservation — but an EXPIRED-yet-active
	// claim (the expiry worker has not swept it) is expired in this
	// transaction, its reservation released, and a fresh claim issued.
	existing, err := repository.FindActiveClaimByTaskAgent(ctx, tx, t.ID, agentID)
	if err != nil && !goerrors.Is(err, pgx.ErrNoRows) {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if existing != nil {
		if existing.ExpiresAt.After(now) {
			return newClaimView(existing, t.Code), nil
		}
		if err := repository.ApplyClaimStatus(ctx, tx, existing.ClaimID, task.ClaimActive, task.EventClaimExpire); err != nil {
			return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if err := repository.ReleaseTaskReservation(ctx, tx, t.ID, existing.Amount); err != nil {
			return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}

	contract, err := effectiveContract(ctx, tx, t)
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}

	// §5.3 step 5: the rejected cap.
	counts, err := repository.CountAgentSubmissions(ctx, tx, t.ID, agentID, rejectionsSince(now))
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := checkRejectionLimit(counts, contract, now); err != nil {
		return claimView{}, err
	}

	// §4 derived: slots = ⌊available / price⌋ must be ≥ 1.
	available := t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	if contract.Price <= 0 || available/contract.Price < 1 {
		return claimView{}, errors.New(0, "SLOTS_EXHAUSTED", "No claimable budget left")
	}

	ttl := int64(task.DefaultClaimTTLSeconds)
	if contract.Claim.TTL != nil {
		ttl = *contract.Claim.TTL
	}
	maxDur := int64(task.DefaultClaimMaxDuration)
	if contract.Claim.MaxDuration != nil {
		maxDur = *contract.Claim.MaxDuration
	}

	claimID, err := repository.InsertClaim(ctx, tx, repository.NewClaimRow{
		TaskID:  t.ID,
		AgentID: agentID,

		ContractVersion: t.ContractVersion,
		ExpiresAt:       now.Add(time.Duration(ttl) * time.Second),
		Deadline:        now.Add(time.Duration(maxDur) * time.Second),
		Amount:          contract.Price,
	})
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	// Task 1.1 §7.3: the confirmed engagement binds the required input
	// versions — freeze the current revision of every harness_ref in
	// the claim's own transaction (deleted memories pin nothing).
	pins, err := resolveHarnessRefs(ctx, tx, t.PublisherID, contract.HarnessRefs)
	if err != nil {
		return claimView{}, err
	}
	if len(pins) > 0 {
		repo := make([]repository.HarnessPin, 0, len(pins))
		for _, p := range pins {
			repo = append(repo, repository.HarnessPin{MemoryID: p.MemoryID, Revision: p.Revision})
		}
		if err := repository.InsertClaimHarnessRevisions(ctx, tx, claimID, repo); err != nil {
			return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}
	if err := repository.ReserveTaskAmount(ctx, tx, t.ID, contract.Price); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	claim, err := repository.FindClaimByID(ctx, pool, claimID)
	if err != nil || claim == nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return newClaimView(claim, t.Code), nil
}

// RenewClaim is the §5.2 work_claim_renew operation:
// expires_at = min(now + ttl, deadline), never past the deadline.
// Lock order Task → Claim (the global §7c ordering): the claim row is
// first read UNLOCKED for its task_id, the Task row is locked, then
// the claim is locked and revalidated — a claim that changed in
// between surfaces as CLAIM_INVALID.
func RenewClaim(ctx context.Context, pool *pg.Pool, agentID, claimID int64, now time.Time) (claimView, error) {
	hint, err := repository.FindClaimByID(ctx, pool, claimID)
	if goerrors.Is(err, pgx.ErrNoRows) || hint == nil {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim not found")
	}
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	t, err := repository.FindTaskByIDForUpdate(ctx, tx, hint.TaskID)
	if err != nil || t == nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	claim, err := repository.FindClaimByIDForUpdate(ctx, tx, claimID)
	if goerrors.Is(err, pgx.ErrNoRows) || claim == nil {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim not found")
	}
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if claim.AgentID != agentID || claim.Status != task.ClaimActive || !claim.ExpiresAt.After(now) {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim is not active for you")
	}
	if t.Status != task.TaskOpen {
		return claimView{}, taskNotOpen(t)
	}
	if !now.Before(claim.Deadline) {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim deadline reached")
	}

	// TTL comes from the task's current contract (§5.2): a paused edit
	// applies to renewals of existing claims, too.
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	ttl := int64(task.DefaultClaimTTLSeconds)
	if contract.Claim.TTL != nil {
		ttl = *contract.Claim.TTL
	}
	expires := now.Add(time.Duration(ttl) * time.Second)
	if expires.After(claim.Deadline) {
		expires = claim.Deadline
	}
	if err := repository.RenewClaim(ctx, tx, claimID, task.ClaimActive, expires); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	after, err := repository.FindClaimByID(ctx, pool, claimID)
	if err != nil || after == nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return newClaimView(after, t.Code), nil
}

// ReleaseClaim is the §5.2 work_release operation: released + the
// reservation returns to the task's available budget. Lock order
// Task → Claim, like RenewClaim.
func ReleaseClaim(ctx context.Context, pool *pg.Pool, agentID, claimID int64, now time.Time) (claimView, error) {
	hint, err := repository.FindClaimByID(ctx, pool, claimID)
	if goerrors.Is(err, pgx.ErrNoRows) || hint == nil {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim not found")
	}
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	t, err := repository.FindTaskByIDForUpdate(ctx, tx, hint.TaskID)
	if err != nil || t == nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	claim, err := repository.FindClaimByIDForUpdate(ctx, tx, claimID)
	if goerrors.Is(err, pgx.ErrNoRows) || claim == nil {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim not found")
	}
	if err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if claim.AgentID != agentID || claim.Status != task.ClaimActive {
		return claimView{}, errors.New(0, "CLAIM_INVALID", "Claim is not active for you")
	}
	if err := repository.ApplyClaimStatus(ctx, tx, claimID, task.ClaimActive, task.EventClaimRelease); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.ReleaseTaskReservation(ctx, tx, claim.TaskID, claim.Amount); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	after, err := repository.FindClaimByID(ctx, pool, claimID)
	if err != nil || after == nil {
		return claimView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return newClaimView(after, t.Code), nil
}

// ExpireClaims is the §5.2 expiry reclaimer: every still-active claim
// whose expires_at has passed becomes expired and its reservation is
// released, one transaction per claim under the row lock. It returns
// the number of claims expired in this pass.
func ExpireClaims(ctx context.Context, pool *pg.Pool, now time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = 100
	}
	ids, err := repository.ListExpiredActiveClaims(ctx, pool, now, batch)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, id := range ids {
		hint, err := repository.FindClaimByID(ctx, pool, id)
		if err != nil || hint == nil {
			continue
		}
		tx, err := pool.TxBegin(ctx)
		if err != nil {
			return expired, err
		}
		// lock order Task → Claim; the task lock also serializes the
		// reservation release against concurrent claims
		if _, err := repository.FindTaskByIDForUpdate(ctx, tx, hint.TaskID); err != nil {
			_ = pg.Rollback(tx)
			return expired, err
		}
		claim, err := repository.FindClaimByIDForUpdate(ctx, tx, id)
		if err != nil {
			_ = pg.Rollback(tx)
			return expired, err
		}
		if claim == nil || claim.Status != task.ClaimActive || claim.ExpiresAt.After(now) {
			_ = pg.Rollback(tx)
			continue
		}
		if err := repository.ApplyClaimStatus(ctx, tx, id, task.ClaimActive, task.EventClaimExpire); err != nil {
			_ = pg.Rollback(tx)
			return expired, err
		}
		if err := repository.ReleaseTaskReservation(ctx, tx, claim.TaskID, claim.Amount); err != nil {
			_ = pg.Rollback(tx)
			return expired, err
		}
		if err := tx.Commit(ctx); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}
