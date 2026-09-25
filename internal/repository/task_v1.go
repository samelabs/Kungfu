package repository

// Task 1.0 (WO-1) repository kernel: reads, row-locked reads and the
// money/state primitives for tb_task, tb_task_version, tb_task_claim,
// tb_task_submission, tb_task_submission_event and tb_task_report
// (migrations/015_task_v1.sql, spec docs/task-spec-1.0.md).
//
// Rules fixed here for every later work order:
//   - a Submission state is written ONLY by SetSubmissionState, which
//     appends the matching SubmissionEvent inside the same call
//     (spec §10 item 9; there is no separate state-write entry);
//   - every status change is validated by the internal/task
//     transition kernel first, then applied as a compare-and-swap on
//     the current status, so a concurrent writer surfaces as
//     ErrTaskStateConflict instead of a silent overwrite;
//   - money primitives (Lock/Fund/Settle/Refund) move the task
//     counters and the credits ledger in the SAME transaction;
//     reserve/release only move task counters (the money stays
//     locked inside the task, spec §2 Ledger);
//   - lock order is always task row → bot row (credits.Record locks
//     the bot), keeping every writer deadlock-free.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/task"
)

// ErrTaskStateConflict: a compare-and-swap status update matched no
// row — the entity was concurrently changed or does not exist.
var ErrTaskStateConflict = errors.New("task entity state changed concurrently")

// ErrNoAvailableBudget: reserving would exceed the task's available
// budget (budget_locked − settled − reserved − refunded).
var ErrNoAvailableBudget = errors.New("task available budget exhausted")

// ErrInsufficientReservation: releasing more than the current
// reservation.
var ErrInsufficientReservation = errors.New("release exceeds task reservation")

// ---------------------------------------------------------------------------
// tb_task
// ---------------------------------------------------------------------------

// TaskRow is a row of tb_task. DraftContract is the raw current
// contract JSON (016) — authoritative while draft or paused.
type TaskRow struct {
	ID            int64
	Code          string
	PublisherID   int64
	Status        string
	Version       int32
	BudgetLocked  int64
	Settled       int64
	Reserved      int64
	Refunded      int64
	PausedReason  *string
	ClosedReason  *string
	DraftContract []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewTaskRow is the insert input for a draft task.
type NewTaskRow struct {
	Code        string
	PublisherID int64
	Contract    []byte
}

// InsertTask creates a draft task with zeroed counters. Budget
// locking is a separate primitive (LockTaskBudget).
func InsertTask(ctx context.Context, q pg.Querier, in NewTaskRow) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_task (code, publisher_id, status, version, draft_contract)
		VALUES ($1, $2, 'draft', 0, $3)
		RETURNING id`, in.Code, in.PublisherID, in.Contract).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task: %w", err)
	}
	return id, nil
}

// UpdateDraftContract replaces the draft contract JSON (draft/paused
// editing). The caller owns the status precondition.
func UpdateDraftContract(ctx context.Context, q pg.Querier, taskID int64, contract []byte) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET draft_contract = $2, updated_at = NOW()
		WHERE id = $1`, taskID, contract)
	if err != nil {
		return fmt.Errorf("update draft_contract: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// SetTaskVersion points the task at its now-effective version. Pair
// with ApplyTaskStatus(open) inside the same transaction.
func SetTaskVersion(ctx context.Context, q pg.Querier, taskID int64, version int32) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET version = $2, updated_at = NOW()
		WHERE id = $1`, taskID, version)
	if err != nil {
		return fmt.Errorf("set task version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// TaskCodeExists reports whether a task code is taken (publiccode
// uniqueness probe).
func TaskCodeExists(ctx context.Context, q pg.Querier, code string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tb_task WHERE code = $1)`, code).Scan(&exists)
	return exists, err
}

// ListTasksByPublisher returns one publisher's tasks, newest first.
func ListTasksByPublisher(ctx context.Context, q pg.Querier, publisherID int64) ([]TaskRow, error) {
	rows, err := q.Query(ctx, taskSelect+`
		WHERE publisher_id = $1
		ORDER BY created_at DESC, id DESC`, publisherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

const taskSelect = `
	SELECT id, code, publisher_id, status, version,
	       budget_locked, settled, reserved, refunded,
	       paused_reason, closed_reason, draft_contract, created_at, updated_at
	FROM tb_task`

func scanTask(row pgx.Row) (*TaskRow, error) {
	var t TaskRow
	if err := row.Scan(&t.ID, &t.Code, &t.PublisherID, &t.Status, &t.Version,
		&t.BudgetLocked, &t.Settled, &t.Reserved, &t.Refunded,
		&t.PausedReason, &t.ClosedReason, &t.DraftContract, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// FindTaskByID loads a task by id.
func FindTaskByID(ctx context.Context, q pg.Querier, id int64) (*TaskRow, error) {
	return scanTask(q.QueryRow(ctx, taskSelect+` WHERE id = $1`, id))
}

// FindTaskByIDForUpdate loads a task by id holding the row lock
// (SELECT ... FOR UPDATE) inside the caller's transaction.
func FindTaskByIDForUpdate(ctx context.Context, q pg.Querier, id int64) (*TaskRow, error) {
	return scanTask(q.QueryRow(ctx, taskSelect+` WHERE id = $1 FOR UPDATE`, id))
}

// FindTaskByCode loads a task by code.
func FindTaskByCode(ctx context.Context, q pg.Querier, code string) (*TaskRow, error) {
	return scanTask(q.QueryRow(ctx, taskSelect+` WHERE code = $1`, code))
}

// FindTaskByCodeForUpdate loads a task by code holding the row lock.
func FindTaskByCodeForUpdate(ctx context.Context, q pg.Querier, code string) (*TaskRow, error) {
	return scanTask(q.QueryRow(ctx, taskSelect+` WHERE code = $1 FOR UPDATE`, code))
}

// ApplyTaskStatus validates `event` against the §4 kernel and moves
// the task from `from` to the resulting status as a compare-and-swap.
// The reason columns (paused_reason / closed_reason) are set when
// provided and cleared when the status leaves that reason's scope.
func ApplyTaskStatus(ctx context.Context, q pg.Querier, taskID int64, from, event string, reason *string) error {
	to, err := task.TaskTransition(from, event)
	if err != nil {
		return err
	}
	var n int
	if err := q.QueryRow(ctx, `
		UPDATE tb_task
		SET status = $2::varchar,
		    paused_reason = CASE WHEN $2::varchar = 'paused' THEN $3::varchar ELSE NULL END,
		    closed_reason = CASE WHEN $2::varchar = 'closed' THEN $3::varchar ELSE NULL END,
		    updated_at = NOW()
		WHERE id = $1 AND status = $4::varchar
		RETURNING 1`, taskID, to, reason, from).Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskStateConflict
		}
		return fmt.Errorf("apply task status: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// tb_task_version
// ---------------------------------------------------------------------------

// TaskVersionRow is a row of tb_task_version. Contract and Harness
// are the raw JSON snapshots (spec §3 Contract, §2 TaskVersion).
type TaskVersionRow struct {
	TaskID    int64
	Version   int32
	Contract  []byte
	Harness   []byte
	CreatedAt time.Time
}

// InsertTaskVersion stores an immutable version snapshot.
func InsertTaskVersion(ctx context.Context, q pg.Querier, taskID int64, version int32, contract, harness []byte) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_task_version (task_id, version, contract, harness)
		VALUES ($1, $2, $3, $4)`, taskID, version, contract, harness)
	if err != nil {
		return fmt.Errorf("insert tb_task_version: %w", err)
	}
	return nil
}

// FindTaskVersion loads one version snapshot.
func FindTaskVersion(ctx context.Context, q pg.Querier, taskID int64, version int32) (*TaskVersionRow, error) {
	var v TaskVersionRow
	err := q.QueryRow(ctx, `
		SELECT task_id, version, contract, harness, created_at
		FROM tb_task_version WHERE task_id = $1 AND version = $2`,
		taskID, version).Scan(&v.TaskID, &v.Version, &v.Contract, &v.Harness, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ---------------------------------------------------------------------------
// tb_task_claim
// ---------------------------------------------------------------------------

// ClaimRow is a row of tb_task_claim.
type ClaimRow struct {
	ClaimID   int64
	TaskID    int64
	AgentID   int64
	Version   int32
	ExpiresAt time.Time
	Deadline  time.Time
	Amount    int64
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewClaimRow is the insert input; the claim starts active.
type NewClaimRow struct {
	TaskID    int64
	AgentID   int64
	Version   int32
	ExpiresAt time.Time
	Deadline  time.Time
	Amount    int64
}

// InsertClaim creates an active claim. The task reservation is a
// separate primitive (ReserveTaskAmount) — compose both in one
// transaction (spec §5.2 work_claim).
func InsertClaim(ctx context.Context, q pg.Querier, in NewClaimRow) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_task_claim (task_id, agent_id, version, expires_at, deadline, amount)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING claim_id`,
		in.TaskID, in.AgentID, in.Version, in.ExpiresAt, in.Deadline, in.Amount).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task_claim: %w", err)
	}
	return id, nil
}

const claimSelect = `
	SELECT claim_id, task_id, agent_id, version, expires_at, deadline, amount, status, created_at, updated_at
	FROM tb_task_claim`

func scanClaim(row pgx.Row) (*ClaimRow, error) {
	var c ClaimRow
	if err := row.Scan(&c.ClaimID, &c.TaskID, &c.AgentID, &c.Version,
		&c.ExpiresAt, &c.Deadline, &c.Amount, &c.Status,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// FindClaimByID loads a claim.
func FindClaimByID(ctx context.Context, q pg.Querier, claimID int64) (*ClaimRow, error) {
	return scanClaim(q.QueryRow(ctx, claimSelect+` WHERE claim_id = $1`, claimID))
}

// FindClaimByIDForUpdate loads a claim holding the row lock.
func FindClaimByIDForUpdate(ctx context.Context, q pg.Querier, claimID int64) (*ClaimRow, error) {
	return scanClaim(q.QueryRow(ctx, claimSelect+` WHERE claim_id = $1 FOR UPDATE`, claimID))
}

// RenewClaim extends expires_at for an active claim: min(now + ttl,
// deadline) is computed by the caller; here the CAS keeps the claim
// active and bounded by its deadline column (spec §5.2).
func RenewClaim(ctx context.Context, q pg.Querier, claimID int64, from string, expiresAt time.Time) error {
	if _, err := task.ClaimTransition(from, task.EventClaimRenew); err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_claim
		SET expires_at = $2, updated_at = NOW()
		WHERE claim_id = $1 AND status = 'active' AND $2 <= deadline`,
		claimID, expiresAt)
	if err != nil {
		return fmt.Errorf("renew claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskStateConflict
	}
	return nil
}

// ApplyClaimStatus validates `event` against the §5.2 kernel and
// moves the claim from `from` as a compare-and-swap. Counter moves
// (reservation hand-off) are separate primitives composed by the
// caller in the same transaction.
func ApplyClaimStatus(ctx context.Context, q pg.Querier, claimID int64, from, event string) error {
	to, err := task.ClaimTransition(from, event)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_claim SET status = $2, updated_at = NOW()
		WHERE claim_id = $1 AND status = $3`,
		claimID, to, from)
	if err != nil {
		return fmt.Errorf("apply claim status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskStateConflict
	}
	return nil
}

// ---------------------------------------------------------------------------
// tb_task_submission (+ events)
// ---------------------------------------------------------------------------

// SubmissionRow is a row of tb_task_submission. Payload and Verdict
// are raw JSON; both are NULL after retention cleanup (spec §9).
type SubmissionRow struct {
	SubmissionID   int64
	TaskID         int64
	Version        int32
	AgentID        int64
	RequestKey     string
	Payload        []byte
	PayloadHash    string
	Amount         int64
	State          string
	Verdict        []byte
	Failure        *string
	Revises        *int64
	ClaimID        *int64
	ReviewDeadline *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SettledAt      *time.Time
}

// NewSubmissionRow is the insert input; the submission is created in
// delivering with the first SubmissionEvent (cause "submit") in the
// same transaction.
type NewSubmissionRow struct {
	TaskID      int64
	Version     int32
	AgentID     int64
	RequestKey  string
	Payload     []byte
	PayloadHash string
	Amount      int64
	Revises     *int64
	ClaimID     *int64
}

// InsertSubmission creates the submission and its first event. Call
// inside a transaction that also reserves the amount (spec §5.3).
func InsertSubmission(ctx context.Context, tx pgx.Tx, in NewSubmissionRow) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO tb_task_submission
			(task_id, version, agent_id, request_key, payload, payload_hash, amount, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'delivering')
		RETURNING submission_id`,
		in.TaskID, in.Version, in.AgentID, in.RequestKey,
		in.Payload, in.PayloadHash, in.Amount).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task_submission: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tb_task_submission_event (submission_id, seq, from_state, to_state, cause)
		VALUES ($1, 1, NULL, 'delivering', $2)`, id, task.EventSubmit); err != nil {
		return 0, fmt.Errorf("insert first submission event: %w", err)
	}
	return id, nil
}

const submissionSelect = `
	SELECT submission_id, task_id, version, agent_id, request_key, payload, payload_hash,
	       amount, state, verdict, failure, revises, claim_id, review_deadline,
	       created_at, updated_at, settled_at
	FROM tb_task_submission`

func scanSubmission(row pgx.Row) (*SubmissionRow, error) {
	var s SubmissionRow
	if err := row.Scan(&s.SubmissionID, &s.TaskID, &s.Version, &s.AgentID, &s.RequestKey,
		&s.Payload, &s.PayloadHash, &s.Amount, &s.State, &s.Verdict, &s.Failure,
		&s.Revises, &s.ClaimID, &s.ReviewDeadline,
		&s.CreatedAt, &s.UpdatedAt, &s.SettledAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// FindSubmissionByID loads a submission.
func FindSubmissionByID(ctx context.Context, q pg.Querier, submissionID int64) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+` WHERE submission_id = $1`, submissionID))
}

// FindSubmissionByIDForUpdate loads a submission holding the row lock.
func FindSubmissionByIDForUpdate(ctx context.Context, q pg.Querier, submissionID int64) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+` WHERE submission_id = $1 FOR UPDATE`, submissionID))
}

// FindSubmissionByIdentity resolves the idempotency identity
// (agent, task, request_key) — spec §5.3 step 2 and §10 item 6.
func FindSubmissionByIdentity(ctx context.Context, q pg.Querier, taskID, agentID int64, requestKey string) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx,
		submissionSelect+` WHERE agent_id = $1 AND task_id = $2 AND request_key = $3`,
		agentID, taskID, requestKey))
}

// SubmissionEventRow is a row of tb_task_submission_event.
type SubmissionEventRow struct {
	SubmissionID int64
	Seq          int32
	FromState    *string
	ToState      string
	Cause        string
	At           time.Time
}

// ListSubmissionEvents returns the append-only event history in seq
// order.
func ListSubmissionEvents(ctx context.Context, q pg.Querier, submissionID int64) ([]SubmissionEventRow, error) {
	rows, err := q.Query(ctx, `
		SELECT submission_id, seq, from_state, to_state, cause, at
		FROM tb_task_submission_event WHERE submission_id = $1 ORDER BY seq`,
		submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubmissionEventRow
	for rows.Next() {
		var e SubmissionEventRow
		if err := rows.Scan(&e.SubmissionID, &e.Seq, &e.FromState, &e.ToState, &e.Cause, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetSubmissionStateOpts carries the optional facts a transition may
// record; nil leaves everything untouched.
type SetSubmissionStateOpts struct {
	Verdict        []byte     // §6.1 Verdict (rejected / settled)
	Failure        *string    // §8.4 failure reason
	ReviewDeadline *time.Time // under_review cutoff
}

// SetSubmissionState is the ONLY way a submission state is written:
// it validates `event` against the §5.4 kernel, compare-and-swaps the
// state and appends the SubmissionEvent in the same transaction.
// settled_at is stamped automatically when the target is settled.
func SetSubmissionState(ctx context.Context, tx pgx.Tx, submissionID int64, from, event string, opts *SetSubmissionStateOpts) error {
	to, err := task.SubmissionTransition(from, event)
	if err != nil {
		return err
	}

	var verdict, failure, reviewDeadline any
	if opts != nil {
		verdict, failure, reviewDeadline = opts.Verdict, opts.Failure, opts.ReviewDeadline
	}

	tag, err := tx.Exec(ctx, `
		UPDATE tb_task_submission
		SET state = $2::varchar,
		    verdict = COALESCE($3::jsonb, verdict),
		    failure = COALESCE($4::varchar, failure),
		    review_deadline = CASE
		        WHEN $2::varchar = 'under_review' THEN COALESCE($5::timestamptz, review_deadline)
		        ELSE review_deadline END,
		    settled_at = CASE WHEN $2::varchar = 'settled' THEN NOW() ELSE settled_at END,
		    updated_at = NOW()
		WHERE submission_id = $1 AND state = $6::varchar`,
		submissionID, to, verdict, failure, reviewDeadline, from)
	if err != nil {
		return fmt.Errorf("set submission state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskStateConflict
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO tb_task_submission_event (submission_id, seq, from_state, to_state, cause)
		SELECT $1, COALESCE(MAX(seq), 0) + 1, $2, $3, $4
		FROM tb_task_submission_event WHERE submission_id = $1`,
		submissionID, from, to, event); err != nil {
		return fmt.Errorf("append submission event: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// tb_task_report
// ---------------------------------------------------------------------------

// InsertTaskReport records a report (status open) — spec §2 Report.
func InsertTaskReport(ctx context.Context, q pg.Querier, taskID, reporterID int64, reason string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tb_task_report (task_id, reporter_id, reason)
		VALUES ($1, $2, $3)
		RETURNING id`, taskID, reporterID, reason).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task_report: %w", err)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Money primitives — task counters + credits ledger in one
// transaction (spec §2 Ledger, §10 items 1–3).
// ---------------------------------------------------------------------------

func taskRef(taskID int64) (string, string) {
	return task.RefTypeTask, fmt.Sprint(taskID)
}

func submissionRef(submissionID int64) (string, string) {
	return task.RefTypeSubmission, fmt.Sprint(submissionID)
}

// LockTaskBudget moves `amount` from the publisher's balance into
// the task (budget_locked += amount) and writes the lock_task ledger
// row — the create transition's money half (spec §4 create).
func LockTaskBudget(ctx context.Context, pool *pg.Pool, tx pgx.Tx, taskID, publisherID, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("lock amount must be positive, got %d", amount)
	}
	if err := bumpTaskCounter(ctx, tx, taskID, "budget_locked", +amount); err != nil {
		return err
	}
	refType, refID := taskRef(taskID)
	if _, err := credits.Record(ctx, pool, tx, publisherID, task.LedgerTypeLock, -amount, &refType, &refID); err != nil {
		return fmt.Errorf("lock_task ledger: %w", err)
	}
	return nil
}

// FundTaskBudget adds budget to a non-closed task: budget_locked +=
// amount plus the fund_task ledger row (spec §4 fund).
func FundTaskBudget(ctx context.Context, pool *pg.Pool, tx pgx.Tx, taskID, publisherID, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("fund amount must be positive, got %d", amount)
	}
	if err := bumpTaskCounter(ctx, tx, taskID, "budget_locked", +amount); err != nil {
		return err
	}
	refType, refID := taskRef(taskID)
	if _, err := credits.Record(ctx, pool, tx, publisherID, task.LedgerTypeFund, -amount, &refType, &refID); err != nil {
		return fmt.Errorf("fund_task ledger: %w", err)
	}
	return nil
}

// ReserveTaskAmount moves `amount` from the task's available budget
// into reserved (spec §4 derived amounts). No ledger row: the money
// stays locked inside the task. Fails with ErrNoAvailableBudget when
// available < amount.
func ReserveTaskAmount(ctx context.Context, q pg.Querier, taskID, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("reserve amount must be positive, got %d", amount)
	}
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET reserved = reserved + $2, updated_at = NOW()
		WHERE id = $1
		  AND budget_locked - settled - reserved - refunded >= $2`,
		taskID, amount)
	if err != nil {
		return fmt.Errorf("reserve task amount: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAvailableBudget
	}
	return nil
}

// ReleaseTaskReservation returns a reservation to available
// (reserved −= amount) — claim release/expiry and rejected/failed
// submissions (spec §5.2, §5.4).
func ReleaseTaskReservation(ctx context.Context, q pg.Querier, taskID, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("release amount must be positive, got %d", amount)
	}
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET reserved = reserved - $2, updated_at = NOW()
		WHERE id = $1 AND reserved >= $2`,
		taskID, amount)
	if err != nil {
		return fmt.Errorf("release task reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInsufficientReservation
	}
	return nil
}

// SettleTaskSubmission settles one submission: the reservation
// becomes settled (reserved −= amount, settled += amount) and the
// agent is paid with an earn_task ledger row referencing the
// submission — acceptance and settlement in the same transaction
// (spec §5.4, §10 item 3).
func SettleTaskSubmission(ctx context.Context, pool *pg.Pool, tx pgx.Tx, taskID, submissionID, agentID, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("settle amount must be positive, got %d", amount)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tb_task
		SET reserved = reserved - $2, settled = settled + $2, updated_at = NOW()
		WHERE id = $1 AND reserved >= $2`,
		taskID, amount)
	if err != nil {
		return fmt.Errorf("settle task counters: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInsufficientReservation
	}
	refType, refID := submissionRef(submissionID)
	if _, err := credits.Record(ctx, pool, tx, agentID, task.LedgerTypeEarn, +amount, &refType, &refID); err != nil {
		return fmt.Errorf("earn_task ledger: %w", err)
	}
	return nil
}

// RefundTaskAvailable returns the task's available budget to the
// publisher (refunded += available, refund_task ledger row) — spec
// §4 refund. Returns the refunded amount (0 when nothing available).
func RefundTaskAvailable(ctx context.Context, pool *pg.Pool, tx pgx.Tx, taskID, publisherID int64) (int64, error) {
	var available int64
	if err := tx.QueryRow(ctx, `
		SELECT budget_locked - settled - reserved - refunded
		FROM tb_task WHERE id = $1 FOR UPDATE`,
		taskID).Scan(&available); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("task %d not found", taskID)
		}
		return 0, fmt.Errorf("refund task available: %w", err)
	}
	if available <= 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tb_task SET refunded = refunded + $2, updated_at = NOW()
		WHERE id = $1`, taskID, available); err != nil {
		return 0, fmt.Errorf("refund task available: %w", err)
	}
	refType, refID := taskRef(taskID)
	if _, err := credits.Record(ctx, pool, tx, publisherID, task.LedgerTypeRefund, +available, &refType, &refID); err != nil {
		return 0, fmt.Errorf("refund_task ledger: %w", err)
	}
	return available, nil
}

func bumpTaskCounter(ctx context.Context, q pg.Querier, taskID int64, column string, delta int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET `+column+` = `+column+` + $2, updated_at = NOW()
		WHERE id = $1`, taskID, delta)
	if err != nil {
		return fmt.Errorf("bump task %s: %w", column, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// IsUniqueViolation reports a 23505 unique-constraint rejection.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
