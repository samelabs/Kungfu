package service

// Submission intake — spec §5.3 acceptance steps 2–8 in the exact
// order (step 1, identity and rate limiting, belongs to the protocol
// layer). Everything after the checks is ONE transaction: the task row
// is locked, steps c–f re-verified, the claim (if any) is used and
// carries its own reservation, otherwise the price is reserved, and
// the submission row is created with its first event by the repository
// primitive. Any failure leaves no submission and no reservation
// change. Delivery is WO-5.

import (
	"bytes"
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// §3/§5.3 payload cap and §5.3 request_key grammar.
const (
	maxPayloadBytes = 512 * 1024
)

var requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// SubmitInput is the §5.3 submission request.
type SubmitInput struct {
	Code       string          `json:"code"`
	RequestKey string          `json:"request_key"`
	Payload    json.RawMessage `json:"payload"`
	ClaimID    *WireID         `json:"claim_id,omitempty"`
	Revises    *WireID         `json:"revises,omitempty"`
}

// ReplyView is the receiver's reply exactly as recorded (§7.2): the
// status code and the body (first 4 000 bytes), never rewritten.
type ReplyView struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// SubmissionView is the intake + delivery return structure.
type SubmissionView struct {
	SubmissionID WireID     `json:"submission_id"`
	TaskCode     string     `json:"task_code"`
	State        string     `json:"state"`
	Amount       int64      `json:"amount"`
	Paid         int64      `json:"paid"`
	Reply        *ReplyView `json:"reply"`
	Failure      *string    `json:"failure"`
	CreatedAt    time.Time  `json:"created_at"`
}

func newSubmissionView(s *repository.SubmissionRow, code string) SubmissionView {
	v := SubmissionView{
		SubmissionID: WireID(s.SubmissionID),
		TaskCode:     code,
		State:        s.State,
		Amount:       s.Amount,
		Failure:      s.Failure,
		CreatedAt:    s.CreatedAt,
	}
	if s.ResponseCode != nil {
		v.Reply = &ReplyView{Status: *s.ResponseCode}
		if s.ResponseBody != nil {
			v.Reply.Body = *s.ResponseBody
		}
	}
	if s.State == task.SubSettled {
		v.Paid = s.Amount
	}
	return v
}

// SubmitWork is the §5.3 intake. Acceptance steps run in the spec's
// order; the write phase re-verifies c–f under the task row lock. The
// accepted submission is then delivered SYNCHRONOUSLY (§5.4) and the
// post-delivery view returned; agentRefKey keys the per-task anonymous
// agent_ref (§7.1).
func SubmitWork(ctx context.Context, pool *pg.Pool, agentID int64, in SubmitInput, agentRefKey []byte, now time.Time) (SubmissionView, error) {
	// (a) request_key format and payload size — before any query.
	if !requestKeyPattern.MatchString(in.RequestKey) {
		return SubmissionView{}, errors.New(400, "INVALID_REQUEST_KEY",
			"request_key must be 1-128 chars of [A-Za-z0-9._~-]")
	}
	if len(in.Payload) > maxPayloadBytes {
		return SubmissionView{}, errors.New(400, "PAYLOAD_TOO_LARGE",
			fmt.Sprintf("payload must be at most %d bytes, got %d", maxPayloadBytes, len(in.Payload)))
	}
	payloadHash := task.PayloadHash(in.Payload)
	claimID, revises := in.ClaimID.Int64Ptr(), in.Revises.Int64Ptr()

	// (b) idempotency: same (agent, task, request_key).
	t, err := repository.FindTaskByCode(ctx, pool, in.Code)
	if goerrors.Is(err, pgx.ErrNoRows) {
		// fall through to (c) which reports TASK_NOT_FOUND
	} else if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t != nil {
		if existing, err := repository.FindSubmissionByIdentity(ctx, pool, t.ID, agentID, in.RequestKey); err == nil && existing != nil {
			if existing.PayloadHash == payloadHash {
				return newSubmissionView(existing, t.Code), nil
			}
			return SubmissionView{}, errors.New(0, "IDEMPOTENCY_CONFLICT",
				"request_key was already used with a different payload")
		} else if err != nil && !goerrors.Is(err, pgx.ErrNoRows) {
			return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}

	// (c) task existence / then (d) own task — ownership precedes any
	// claim parsing (§5.3: existence is step 3, OWN_TASK step 4, the
	// claim itself step 6), so a publisher probing its own task with a
	// bogus claim_id hears OWN_TASK, never CLAIM_INVALID. Every
	// submission is checked against the task's current contract.
	if t == nil {
		return SubmissionView{}, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if t.PublisherID == agentID {
		return SubmissionView{}, errors.New(0, "OWN_TASK", "You cannot submit to your own task")
	}
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	if in.ClaimID == nil {
		if t.Status != task.TaskOpen {
			return SubmissionView{}, taskNotOpen(t)
		}
		if slotsFor(t, contract) < 1 {
			return SubmissionView{}, errors.New(0, "SLOTS_EXHAUSTED", "No claimable budget left")
		}
	}

	// (e) limits — same tallies as WO-3.
	counts, err := repository.CountAgentSubmissions(ctx, pool, t.ID, agentID, rejectionsSince(now))
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := checkRejectionLimit(counts, contract, now); err != nil {
		return SubmissionView{}, err
	}

	// (f) claim: the current contract decides whether one is
	// required; a carried claim must be this agent's active claim on
	// this task, unexpired (spec §5.2).
	if in.ClaimID == nil {
		if contract.Claim.Required {
			return SubmissionView{}, errors.New(400, "CLAIM_REQUIRED", "This task requires a claim")
		}
	} else if err := checkClaim(ctx, pool, *claimID, agentID, t.ID, now); err != nil {
		return SubmissionView{}, err
	}

	// (g) revises.
	if in.Revises != nil {
		if err := checkRevises(ctx, pool, *revises, agentID, t.ID); err != nil {
			return SubmissionView{}, err
		}
	}

	// (h) payload: a JSON object, schema-valid, credential-free.
	var decoded map[string]any
	if err := json.Unmarshal(in.Payload, &decoded); err != nil || decoded == nil {
		// JSON null unmarshals into a nil map without error — a null
		// payload is not a JSON object
		return SubmissionView{}, schemaMismatch([]task.PointerError{{
			Pointer: "", Message: "payload must be a JSON object",
		}})
	}
	schema, err := taskSchema(ctx, pool, t.ID)
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if errs := payloadSchemaErrors(schema, in.Payload); len(errs) > 0 {
		return SubmissionView{}, schemaMismatch(errs)
	}
	if ptrs := task.ScanCredentials(in.Payload); len(ptrs) > 0 {
		return SubmissionView{}, errors.NewWithDetails(400, "CREDENTIAL_IN_PAYLOAD",
			"payload contains credential-shaped strings",
			map[string]interface{}{"pointer": ptrs[0]})
	}

	// -- write phase: one transaction, re-verify c–f under the lock --
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	locked, err := repository.FindTaskByCodeForUpdate(ctx, tx, in.Code)
	if err != nil || locked == nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	// (d)+(e) under the lock: ownership is immutable, the tallies are
	// not (a concurrent intake may have committed in between).
	if locked.PublisherID == agentID {
		return SubmissionView{}, errors.New(0, "OWN_TASK", "You cannot submit to your own task")
	}
	// The contract may have changed between the pre-check and the
	// lock (a paused edit commits in between). Re-verify the
	// contract-dependent checks against the locked row's contract.
	var lockedContract task.Contract
	if err := json.Unmarshal(locked.Contract, &lockedContract); err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	if !bytes.Equal(locked.Contract, t.Contract) {
		if in.ClaimID == nil {
			if lockedContract.Claim.Required {
				return SubmissionView{}, errors.New(400, "CLAIM_REQUIRED", "This task requires a claim")
			}
		}
		if errs := payloadSchemaErrors(lockedContract.Output.Schema, in.Payload); len(errs) > 0 {
			return SubmissionView{}, schemaMismatch(errs)
		}
	}
	lockedCounts, err := repository.CountAgentSubmissions(ctx, tx, locked.ID, agentID, rejectionsSince(now))
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := checkRejectionLimit(lockedCounts, lockedContract, now); err != nil {
		return SubmissionView{}, err
	}
	amount := lockedContract.Price
	if in.ClaimID == nil {
		if locked.Status != task.TaskOpen {
			return SubmissionView{}, taskNotOpen(locked)
		}
		if err := repository.ReserveTaskAmount(ctx, tx, locked.ID, amount); err != nil {
			if goerrors.Is(err, repository.ErrNoAvailableBudget) {
				return SubmissionView{}, errors.New(0, "SLOTS_EXHAUSTED", "No claimable budget left")
			}
			return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	} else {
		claim, err := repository.FindClaimByIDForUpdate(ctx, tx, *claimID)
		if err != nil || claim == nil ||
			claim.AgentID != agentID || claim.TaskID != locked.ID ||
			claim.Status != task.ClaimActive || !claim.ExpiresAt.After(now) {
			return SubmissionView{}, errors.New(0, "CLAIM_INVALID", "Claim is not active for you on this task")
		}
		if err := repository.ApplyClaimStatus(ctx, tx, claim.ClaimID, task.ClaimActive, task.EventClaimUse); err != nil {
			return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		amount = claim.Amount
	}

	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: locked.ID,

		AgentID:     agentID,
		RequestKey:  in.RequestKey,
		Payload:     in.Payload,
		PayloadHash: payloadHash,
		Amount:      amount,
		Revises:     revises,
		ClaimID:     claimID,
	})
	if err != nil {
		if repository.IsUniqueViolation(err) {
			// Concurrent twin with the same (agent, task, request_key):
			// fall back to the idempotency contract.
			if existing, ferr := repository.FindSubmissionByIdentity(ctx, pool, locked.ID, agentID, in.RequestKey); ferr == nil && existing != nil {
				if existing.PayloadHash == payloadHash {
					return newSubmissionView(existing, locked.Code), nil
				}
				return SubmissionView{}, errors.New(0, "IDEMPOTENCY_CONFLICT",
					"request_key was already used with a different payload")
			}
		}
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	// Synchronous delivery (§5.4): POST to the receiver, whose reply
	// decides the outcome.
	return DeliverSubmission(ctx, pool, subID, agentRefKey, now)
}

// checkClaim is §5.3 step 6 for a carried claim: this agent's ACTIVE
// claim on this task, unexpired.
func checkClaim(ctx context.Context, pool *pg.Pool, claimID, agentID, taskID int64, now time.Time) error {
	claim, err := repository.FindClaimByID(ctx, pool, claimID)
	if goerrors.Is(err, pgx.ErrNoRows) || claim == nil {
		return errors.New(0, "CLAIM_INVALID", "Claim not found")
	}
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if claim.AgentID != agentID || claim.TaskID != taskID ||
		claim.Status != task.ClaimActive || !claim.ExpiresAt.After(now) {
		return errors.New(0, "CLAIM_INVALID", "Claim is not active for you on this task")
	}
	return nil
}

// rejectedCapFor is the §3 缺省 5 unless the contract overrides.
func rejectedCapFor(contract task.Contract) int64 {
	if contract.Limits.MaxRejectedPerAgent != nil {
		return *contract.Limits.MaxRejectedPerAgent
	}
	return task.DefaultMaxRejectedPerAgent
}

// rejectionWindow is the rolling window of the rejection limit.
const rejectionWindow = task.RejectionWindowHours * time.Hour

// rejectionsSince is the start of the rejection-limit window at now.
func rejectionsSince(now time.Time) time.Time {
	return now.Add(-rejectionWindow)
}

// checkRejectionLimit enforces limits.max_rejected_per_agent over the
// rolling window. At the limit it returns SUBMISSION_LIMIT with the
// facts an executor needs to act: how many rejections count now, the
// cap, when the oldest of them stops counting (retry_after_at) and the
// seconds until then (retry_after, which drives next_action "wait").
func checkRejectionLimit(counts repository.AgentSubmissionCounts, contract task.Contract, now time.Time) error {
	max := rejectedCapFor(contract)
	if counts.RejectedInWindow < max {
		return nil
	}
	retryAt := now.Add(rejectionWindow)
	if counts.OldestRejectedInWindow != nil {
		retryAt = counts.OldestRejectedInWindow.Add(rejectionWindow)
	}
	wait := int(math.Ceil(retryAt.Sub(now).Seconds()))
	if wait < 1 {
		wait = 1
	}
	at := retryAt.UTC().Format(time.RFC3339)
	return errors.NewWithDetails(0, "SUBMISSION_LIMIT",
		fmt.Sprintf("You have %d rejected submissions on this task in the last %d hours (limit %d). "+
			"You can submit again after %s. Read reply.body of your rejected submissions "+
			"(work_history with this code) to see why they were rejected.",
			counts.RejectedInWindow, task.RejectionWindowHours, max, at),
		map[string]interface{}{
			"limit":          "rejected",
			"max":            max,
			"rejected_24h":   counts.RejectedInWindow,
			"window_hours":   task.RejectionWindowHours,
			"retry_after_at": at,
			"retry_after":    wait,
		})
}

// taskSchema returns the output.schema of a task's contract.
func taskSchema(ctx context.Context, pool *pg.Pool, taskID int64) ([]byte, error) {
	t, err := repository.FindTaskByID(ctx, pool, taskID)
	if err != nil || t == nil {
		return nil, err
	}
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return nil, err
	}
	return contract.Output.Schema, nil
}

// slotsFor is the §4 derived slot count for an open task.
func slotsFor(t *repository.TaskRow, contract task.Contract) int64 {
	available := t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	if contract.Price <= 0 {
		return 0
	}
	return available / contract.Price
}

// checkRevises enforces the §5.3 revises rule: the target must be the
// SAME agent's REJECTED submission on the SAME task.
func checkRevises(ctx context.Context, pool *pg.Pool, revises, agentID, taskID int64) error {
	target, err := repository.FindSubmissionByID(ctx, pool, revises)
	if goerrors.Is(err, pgx.ErrNoRows) || target == nil {
		return errors.New(400, "INVALID_REVISES", "revises target not found")
	}
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if target.AgentID != agentID || target.TaskID != taskID || target.State != task.SubRejected {
		return errors.New(400, "INVALID_REVISES",
			"revises must target your own rejected submission on this task")
	}
	return nil
}

// payloadSchemaErrors checks the payload against the contract's
// output.schema; a contract without one (§3: optional) has no
// structure check beyond "a JSON object".
func payloadSchemaErrors(schema, payload []byte) []task.PointerError {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	return task.ValidatePayloadForTask(schema, payload)
}

func schemaMismatch(errs []task.PointerError) *errors.AppError {
	items := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		items = append(items, map[string]string{"pointer": e.Pointer, "message": e.Message})
	}
	return errors.NewWithDetails(400, "SCHEMA_MISMATCH",
		"payload does not match the task schema",
		map[string]interface{}{"errors": items})
}
