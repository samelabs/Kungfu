package repository

// Task 1.0 (WO-1) repository kernel: reads, row-locked reads and the
// money/state primitives for tb_task, tb_task_claim,
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
	"strings"
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
	ID           int64
	Code         string
	PublisherID  int64
	Status       string
	BudgetLocked int64
	Settled      int64
	Reserved     int64
	Refunded     int64
	PausedReason *string
	ClosedReason *string
	Contract     []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
		INSERT INTO tb_task (code, publisher_id, status, contract)
		VALUES ($1, $2, 'paused', $3)
		RETURNING id`, in.Code, in.PublisherID, in.Contract).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task: %w", err)
	}
	return id, nil
}

// UpdateDraftContract replaces the draft contract JSON (draft/paused
// editing). The caller owns the status precondition.
func UpdateTaskContract(ctx context.Context, q pg.Querier, taskID int64, contract []byte) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task SET contract = $2, updated_at = NOW()
		WHERE id = $1`, taskID, contract)
	if err != nil {
		return fmt.Errorf("update contract: %w", err)
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

// FindTasksByPublisherPage returns ONE page of one publisher's tasks,
// newest first, with the total number of matching rows (WO-19 Q2).
// status (exact), code (exact) and keyword (case-insensitive over the
// effective title — the live version's, else the saved draft's) are
// all optional; the WHERE building mirrors the page the caller sees,
// so total and paging stay exact.
func FindTasksByPublisherPage(ctx context.Context, q pg.Querier, publisherID int64, status, keyword, code string, limit, offset int) ([]TaskRow, int64, error) {
	where := ` WHERE tb_task.publisher_id = $1`
	args := []any{publisherID}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(` AND tb_task.status = $%d`, len(args))
	}
	if code != "" {
		args = append(args, code)
		where += fmt.Sprintf(` AND tb_task.code = $%d`, len(args))
	}
	if keyword != "" {
		args = append(args, "%"+EscapeLike(keyword)+"%")
		n := len(args)
		where += fmt.Sprintf(`
		  AND tb_task.contract->>'title'
		      ILIKE $%d ESCAPE '\'`, n)
	}
	// total runs as its own COUNT over the SAME filters: a window
	// COUNT(*) OVER() only exists while the page has rows, so an
	// out-of-range page would report total 0 (WO-19b).
	var total int64
	countQuery := `
		SELECT COUNT(*) FROM tb_task` + where
	if err := q.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	query := `
		SELECT tb_task.id, tb_task.code, tb_task.publisher_id, tb_task.status,
		       tb_task.budget_locked, tb_task.settled, tb_task.reserved, tb_task.refunded,
		       tb_task.paused_reason, tb_task.closed_reason, tb_task.contract,
		       tb_task.created_at, tb_task.updated_at
		FROM tb_task` +
		where + `
		ORDER BY tb_task.created_at DESC, tb_task.id DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var t TaskRow
		if err := rows.Scan(&t.ID, &t.Code, &t.PublisherID, &t.Status, &t.BudgetLocked, &t.Settled, &t.Reserved, &t.Refunded,
			&t.PausedReason, &t.ClosedReason, &t.Contract,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

const taskSelect = `
	SELECT id, code, publisher_id, status,
	       budget_locked, settled, reserved, refunded,
	       paused_reason, closed_reason, contract, created_at, updated_at
	FROM tb_task`

func scanTask(row pgx.Row) (*TaskRow, error) {
	var t TaskRow
	if err := row.Scan(&t.ID, &t.Code, &t.PublisherID, &t.Status, &t.BudgetLocked, &t.Settled, &t.Reserved, &t.Refunded,
		&t.PausedReason, &t.ClosedReason, &t.Contract, &t.CreatedAt, &t.UpdatedAt); err != nil {
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

// CountAgentSubmissionsBatch tallies one agent's submissions across a
// set of tasks in ONE query, keyed by task_id. Tasks with no
// submissions are absent from the map (the zero value applies).
func CountAgentSubmissionsBatch(ctx context.Context, q pg.Querier, agentID int64, taskIDs []int64) (map[int64]AgentSubmissionCounts, error) {
	out := make(map[int64]AgentSubmissionCounts, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT task_id,
		  COUNT(*) FILTER (WHERE state = 'settled'),
		  COUNT(*) FILTER (WHERE state IN ('delivering', 'uncertain')),
		  COUNT(*) FILTER (WHERE state = 'rejected')
		FROM tb_task_submission
		WHERE agent_id = $1 AND task_id = ANY($2)
		GROUP BY task_id`, agentID, taskIDs)
	if err != nil {
		return nil, fmt.Errorf("count agent submissions batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c AgentSubmissionCounts
		if err := rows.Scan(&id, &c.Settled, &c.Inflight, &c.Rejected); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// tb_task_claim
// ---------------------------------------------------------------------------

// ClaimRow is a row of tb_task_claim.
type ClaimRow struct {
	ClaimID   int64
	TaskID    int64
	AgentID   int64
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
		INSERT INTO tb_task_claim (task_id, agent_id, expires_at, deadline, amount)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING claim_id`,
		in.TaskID, in.AgentID, in.ExpiresAt, in.Deadline, in.Amount).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert tb_task_claim: %w", err)
	}
	return id, nil
}

const claimSelect = `
	SELECT claim_id, task_id, agent_id, expires_at, deadline, amount, status, created_at, updated_at
	FROM tb_task_claim`

func scanClaim(row pgx.Row) (*ClaimRow, error) {
	var c ClaimRow
	if err := row.Scan(&c.ClaimID, &c.TaskID, &c.AgentID, &c.ExpiresAt, &c.Deadline, &c.Amount, &c.Status,
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

// AgentSubmissionCounts is the per-(task, agent) submission tally
// behind the §5.3 step-5 limit check (§6.3).
type AgentSubmissionCounts struct {
	Settled  int64
	Inflight int64 // delivering + uncertain
	Rejected int64
}

// CountAgentSubmissions tallies one agent's submissions on one task by
// outcome class.
func CountAgentSubmissions(ctx context.Context, q pg.Querier, taskID, agentID int64) (AgentSubmissionCounts, error) {
	var c AgentSubmissionCounts
	err := q.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE state = 'settled'),
		  COUNT(*) FILTER (WHERE state IN ('delivering', 'uncertain')),
		  COUNT(*) FILTER (WHERE state = 'rejected')
		FROM tb_task_submission
		WHERE task_id = $1 AND agent_id = $2`, taskID, agentID).
		Scan(&c.Settled, &c.Inflight, &c.Rejected)
	if err != nil {
		return AgentSubmissionCounts{}, fmt.Errorf("count agent submissions: %w", err)
	}
	return c, nil
}

// ListExpiredActiveClaims returns up to `limit` active claims whose
// expires_at has passed (oldest first — the §5.2 expiry reclaimer's
// work list).
func ListExpiredActiveClaims(ctx context.Context, q pg.Querier, now time.Time, limit int) ([]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT claim_id FROM tb_task_claim
		WHERE status = 'active' AND expires_at <= $1
		ORDER BY expires_at, claim_id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired claims: %w", err)
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

// LeaseRecoverableSubmissions leases (SKIP LOCKED) up to `limit`
// submissions the recovery pass owns next, refreshing updated_at to
// `now` as the lease:
//   - uncertain not touched for ≥ 30s (redelivery cadence)
//   - delivering not touched for ≥ 15s (§10.4: delivering ≤ 15 秒)
func LeaseRecoverableSubmissions(ctx context.Context, q pg.Querier, now time.Time, limit int) ([]int64, error) {
	rows, err := q.Query(ctx, `
		UPDATE tb_task_submission s
		SET updated_at = $1
		WHERE s.submission_id IN (
			SELECT submission_id FROM tb_task_submission
			WHERE (state = 'uncertain' AND updated_at <= $1::timestamptz - interval '30 seconds')
			   OR (state = 'delivering' AND updated_at <= $1::timestamptz - interval '15 seconds')
			ORDER BY updated_at, submission_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING s.submission_id`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("lease recoverable submissions: %w", err)
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

// UncertainSince is the time the submission FIRST entered uncertain
// (§5.4: the 24h unresolved window starts there).
func UncertainSince(ctx context.Context, q pg.Querier, submissionID int64) (time.Time, error) {
	var at time.Time
	err := q.QueryRow(ctx, `
		SELECT at FROM tb_task_submission_event
		WHERE submission_id = $1 AND to_state = 'uncertain'
		ORDER BY seq LIMIT 1`, submissionID).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	return at, nil
}

// ListTaskSubmissions returns one task's submissions newest first,
// optionally filtered by state.
func ListTaskSubmissions(ctx context.Context, q pg.Querier, taskID int64, state string, limit, offset int) ([]SubmissionRow, error) {
	sql := submissionSelect + ` WHERE task_id = $1`
	args := []any{taskID}
	if state != "" {
		sql += ` AND state = $` + fmt.Sprint(len(args)+1)
		args = append(args, state)
	}
	sql += ` ORDER BY created_at DESC, submission_id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	sql += ` OFFSET $` + fmt.Sprint(len(args)+1)
	args = append(args, offset)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubmissionRow
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// CountTaskSubmissions totals ListTaskSubmissions without paging.
func CountTaskSubmissions(ctx context.Context, q pg.Querier, taskID int64, state string) (int64, error) {
	sql := `SELECT COUNT(*) FROM tb_task_submission WHERE task_id = $1`
	args := []any{taskID}
	if state != "" {
		sql += ` AND state = $2`
		args = append(args, state)
	}
	var n int64
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// TaskStats is the §6.3 30-day statistic set of one task.
type TaskStats struct {
	Settled            int64
	Rejected           int64
	Failed             int64
	TerminalTotal      int64
	MedianReplySeconds *float64 // over settled+rejected durations; nil when none
}

// TaskStats computes the §6.3 statistics for terminals entered at or
// after `since`: reply latency runs from the first event to the
// settled/rejected event (the receiver's reply, including redelivery
// time).
func GetTaskStats(ctx context.Context, q pg.Querier, taskID int64, since time.Time) (TaskStats, error) {
	var s TaskStats
	var median *float64
	err := q.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE state = 'settled'),
		  COUNT(*) FILTER (WHERE state = 'rejected'),
		  COUNT(*) FILTER (WHERE state = 'failed'),
		  COUNT(*),
		  percentile_cont(0.5) WITHIN GROUP (ORDER BY seconds)
		FROM (
		  SELECT sub.state,
		         EXTRACT(EPOCH FROM (
		           (SELECT e.at FROM tb_task_submission_event e
		             WHERE e.submission_id = sub.submission_id
		               AND e.to_state IN ('settled', 'rejected')
		             ORDER BY e.seq DESC LIMIT 1)
		           -
		           (SELECT e.at FROM tb_task_submission_event e
		             WHERE e.submission_id = sub.submission_id
		             ORDER BY e.seq LIMIT 1)
		         ))::double precision AS seconds,
		         (SELECT e.at FROM tb_task_submission_event e
		           WHERE e.submission_id = sub.submission_id
		           ORDER BY e.seq DESC LIMIT 1) AS terminal_at
		  FROM tb_task_submission sub
		  WHERE sub.task_id = $1 AND sub.state IN ('settled', 'rejected', 'failed')
		) t
		WHERE terminal_at IS NOT NULL AND terminal_at >= $2`,
		taskID, since).
		Scan(&s.Settled, &s.Rejected, &s.Failed, &s.TerminalTotal, &median)
	if err != nil {
		return TaskStats{}, fmt.Errorf("task stats: %w", err)
	}
	s.MedianReplySeconds = median
	return s, nil
}

// WorkFilter narrows an open-work page (WO-19 Q1): a keyword matched
// over title/requirements and an exact task code. Both optional.
type WorkFilter struct {
	Keyword string
	Code    string
}

// WorkCandidate is one listable task with its current-version
// contract snapshot (the title/requirements/price source for the
// caller's projection).
type WorkCandidate struct {
	Task     TaskRow
	Contract []byte
}

// EscapeLike escapes the LIKE/ILIKE wildcards of a user keyword so
// they match literally ('%', '_', '\') under ESCAPE '\'.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// FindOpenWorkPage returns ONE page of open, claimable work for an
// agent (agentID 0 = anonymous: no own-task exclusion, no rejection
// cap), newest open first, with the total number of matching rows
// (WO-19 Q1). All §5.1 filtering runs in SQL: status open, a current
// version snapshot, slots >= 1 (available >= price), not the agent's
// own task, and the agent's rejection cap not exhausted. The keyword
// matches title and requirements case-insensitively (wildcards
// escaped by the caller); code is an exact match — a code that is
// not currently claimable simply yields no rows.
func FindOpenWorkPage(ctx context.Context, q pg.Querier, agentID int64, f WorkFilter, limit, offset int) ([]WorkCandidate, int64, error) {
	where := ` WHERE tb_task.status = 'open'
		  AND (tb_task.contract->>'price')::bigint >= 1
		  AND tb_task.budget_locked - tb_task.settled - tb_task.reserved - tb_task.refunded
		      >= (tb_task.contract->>'price')::bigint`
	args := []any{}
	if agentID > 0 {
		args = append(args, agentID)
		n := len(args)
		where += fmt.Sprintf(` AND tb_task.publisher_id <> $%d`, n)
		where += fmt.Sprintf(`
		  AND (SELECT COUNT(*) FROM tb_task_submission s
		        WHERE s.task_id = tb_task.id AND s.agent_id = $%d AND s.state = 'rejected')
		      < COALESCE(NULLIF(tb_task.contract #>> '{limits,max_rejected_per_agent}', '')::bigint, %d)`,
			n, task.DefaultMaxRejectedPerAgent)
	}
	if f.Code != "" {
		args = append(args, f.Code)
		where += fmt.Sprintf(` AND tb_task.code = $%d`, len(args))
	}
	if f.Keyword != "" {
		args = append(args, "%"+EscapeLike(f.Keyword)+"%")
		n := len(args)
		where += fmt.Sprintf(`
		  AND (COALESCE(tb_task.contract->>'title', '') ILIKE $%d ESCAPE '\'
		        OR COALESCE(tb_task.contract->>'requirements', '') ILIKE $%d ESCAPE '\')`, n, n)
	}
	// total runs as its own COUNT over the SAME filters: a window
	// COUNT(*) OVER() only exists while the page has rows, so an
	// out-of-range page would report total 0 (WO-19b).
	var total int64
	countQuery := `
		SELECT COUNT(*) FROM tb_task
		` + where
	if err := q.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	query := `
		SELECT tb_task.id, tb_task.code, tb_task.publisher_id, tb_task.status,
		       tb_task.budget_locked, tb_task.settled, tb_task.reserved, tb_task.refunded,
		       tb_task.paused_reason, tb_task.closed_reason, tb_task.contract,
		       tb_task.created_at, tb_task.updated_at,
		       v.contract
		FROM tb_task
		` +
		where + `
		ORDER BY v.created_at DESC NULLS LAST, tb_task.id DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []WorkCandidate
	for rows.Next() {
		var c WorkCandidate
		if err := rows.Scan(&c.Task.ID, &c.Task.Code, &c.Task.PublisherID, &c.Task.Status,
			&c.Task.BudgetLocked, &c.Task.Settled, &c.Task.Reserved, &c.Task.Refunded,
			&c.Task.PausedReason, &c.Task.ClosedReason, &c.Task.Contract,
			&c.Task.CreatedAt, &c.Task.UpdatedAt, &c.Contract); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ListAgentSubmissions returns one agent's submissions newest first,
// optionally narrowed to one task.
func ListAgentSubmissions(ctx context.Context, q pg.Querier, agentID int64, taskID *int64, limit, offset int) ([]SubmissionRow, error) {
	sql := submissionSelect + ` WHERE agent_id = $1`
	args := []any{agentID}
	if taskID != nil {
		sql += ` AND task_id = $2`
		args = append(args, *taskID)
	}
	sql += ` ORDER BY created_at DESC, submission_id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	sql += ` OFFSET $` + fmt.Sprint(len(args)+1)
	args = append(args, offset)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubmissionRow
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// CountAgentSubmissionsBy totals ListAgentSubmissions without paging.
func CountAgentSubmissionsBy(ctx context.Context, q pg.Querier, agentID int64, taskID *int64) (int64, error) {
	sql := `SELECT COUNT(*) FROM tb_task_submission WHERE agent_id = $1`
	args := []any{agentID}
	if taskID != nil {
		sql += ` AND task_id = $2`
		args = append(args, *taskID)
	}
	var n int64
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// FindOpenReportByReporterTask returns the agent's still-open report
// on a task, or nil (§8.1 work_report idempotency).
func FindOpenReportByReporterTask(ctx context.Context, q pg.Querier, taskID, reporterID int64) (int64, bool, error) {
	var id int64
	err := q.QueryRow(ctx, `
		SELECT id FROM tb_task_report
		WHERE task_id = $1 AND reporter_id = $2 AND status = 'open'
		ORDER BY id LIMIT 1`, taskID, reporterID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find open report: %w", err)
	}
	return id, true, nil
}

// TerminalOutcome is one recent terminal submission: its state and,
// for failed, the failure reason.
type TerminalOutcome struct {
	State   string
	Failure *string
}

// RecentTerminalOutcomes returns the most recent terminal submissions
// of a task (settled / rejected / failed), newest first — the §7.3
// receiver-fault window with the failure reasons attached.
func RecentTerminalOutcomes(ctx context.Context, q pg.Querier, taskID int64, limit int) ([]TerminalOutcome, error) {
	rows, err := q.Query(ctx, `
		SELECT state, failure FROM tb_task_submission
		WHERE task_id = $1 AND state IN ('settled', 'rejected', 'failed')
		ORDER BY updated_at DESC, submission_id DESC
		LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent terminal outcomes: %w", err)
	}
	defer rows.Close()
	var out []TerminalOutcome
	for rows.Next() {
		var o TerminalOutcome
		if err := rows.Scan(&o.State, &o.Failure); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FindActiveClaimByTaskAgent returns the agent's active claim on the
// task, or nil (spec §5.2: at most one active claim per agent+task).
func FindActiveClaimByTaskAgent(ctx context.Context, q pg.Querier, taskID, agentID int64) (*ClaimRow, error) {
	return scanClaim(q.QueryRow(ctx, claimSelect+`
		WHERE task_id = $1 AND agent_id = $2 AND status = 'active'`, taskID, agentID))
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
	SubmissionID int64
	TaskID       int64
	AgentID      int64
	RequestKey   string
	Payload      []byte
	PayloadHash  string
	Amount       int64
	State        string
	ResponseCode *int
	ResponseBody *string
	Failure      *string
	Revises      *int64
	ClaimID      *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	SettledAt    *time.Time
}

// NewSubmissionRow is the insert input; the submission is created in
// delivering with the first SubmissionEvent (cause "submit") in the
// same transaction.
type NewSubmissionRow struct {
	TaskID      int64
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
			(task_id, agent_id, request_key, payload, payload_hash, amount, state, revises, claim_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'delivering', $8, $9)
		RETURNING submission_id`,
		in.TaskID, in.AgentID, in.RequestKey,
		in.Payload, in.PayloadHash, in.Amount, in.Revises, in.ClaimID).Scan(&id)
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
	SELECT submission_id, task_id, agent_id, request_key, payload, payload_hash,
	       amount, state, response_code, response_body, failure, revises, claim_id,
	       created_at, updated_at, settled_at
	FROM tb_task_submission`

func scanSubmission(row pgx.Row) (*SubmissionRow, error) {
	var s SubmissionRow
	if err := row.Scan(&s.SubmissionID, &s.TaskID, &s.AgentID, &s.RequestKey,
		&s.Payload, &s.PayloadHash, &s.Amount, &s.State, &s.ResponseCode, &s.ResponseBody,
		&s.Failure, &s.Revises, &s.ClaimID,
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
	ResponseCode *int    // §7.2 the receiver's status code
	ResponseBody *string // §7.2 the receiver's reply body (bounded)
	Failure      *string // §8.4 failure reason
}

// SetSubmissionState is the ONLY way a submission state is written:
// it validates `event` against the §5.4 kernel, compare-and-swaps the
// state and appends the SubmissionEvent in the same transaction.
// settled_at is stamped automatically when the target is settled; a
// terminal target clears the payload (kept only for redelivery, §9).
func SetSubmissionState(ctx context.Context, tx pgx.Tx, submissionID int64, from, event string, opts *SetSubmissionStateOpts) error {
	to, err := task.SubmissionTransition(from, event)
	if err != nil {
		return err
	}

	var responseCode, responseBody, failure any
	if opts != nil {
		if opts.ResponseCode != nil {
			responseCode = *opts.ResponseCode
		}
		if opts.ResponseBody != nil {
			responseBody = *opts.ResponseBody
		}
		if opts.Failure != nil {
			failure = *opts.Failure
		}
	}

	tag, err := tx.Exec(ctx, `
		UPDATE tb_task_submission
		SET state = $2::varchar,
		    response_code = COALESCE($3::integer, response_code),
		    response_body = COALESCE($4::text, response_body),
		    failure = COALESCE($5::varchar, failure),
		    payload = CASE WHEN $2::varchar IN ('settled', 'rejected', 'failed') THEN NULL ELSE payload END,
		    settled_at = CASE WHEN $2::varchar = 'settled' THEN NOW() ELSE settled_at END,
		    updated_at = NOW()
		WHERE submission_id = $1 AND state = $6::varchar`,
		submissionID, to, responseCode, responseBody, failure, from)
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

// TaskReportRow is a row of tb_task_report.
type TaskReportRow struct {
	ID         int64
	TaskID     int64
	ReporterID int64
	Reason     string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const reportSelect = `
	SELECT id, task_id, reporter_id, reason, status, created_at, updated_at
	FROM tb_task_report`

func scanReport(row pgx.Row) (*TaskReportRow, error) {
	var r TaskReportRow
	if err := row.Scan(&r.ID, &r.TaskID, &r.ReporterID, &r.Reason, &r.Status,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// FindReportByID loads one report.
func FindReportByID(ctx context.Context, q pg.Querier, reportID int64) (*TaskReportRow, error) {
	return scanReport(q.QueryRow(ctx, reportSelect+` WHERE id = $1`, reportID))
}

// FindReportByIDForUpdate loads a report holding the row lock.
func FindReportByIDForUpdate(ctx context.Context, q pg.Querier, reportID int64) (*TaskReportRow, error) {
	return scanReport(q.QueryRow(ctx, reportSelect+` WHERE id = $1 FOR UPDATE`, reportID))
}

// SetReportStatus compare-and-swaps a report status (open → dismissed
// by the admin resolution, open → actioned when its task is closed).
func SetReportStatus(ctx context.Context, q pg.Querier, reportID int64, from, to string) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_report SET status = $2, updated_at = NOW()
		WHERE id = $1 AND status = $3`, reportID, to, from)
	if err != nil {
		return fmt.Errorf("set report status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskStateConflict
	}
	return nil
}

// ActionOpenReports marks every open report of a task actioned — the
// report disposition of a platform close (WO-8b) — returning how many
// rows moved.
func ActionOpenReports(ctx context.Context, q pg.Querier, taskID int64) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_report SET status = 'actioned', updated_at = NOW()
		WHERE task_id = $1 AND status = 'open'`, taskID)
	if err != nil {
		return 0, fmt.Errorf("action open reports: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountTaskSubmissionsByState tallies one task's submissions per
// state (the admin task detail's state counts).
func CountTaskSubmissionsByState(ctx context.Context, q pg.Querier, taskID int64) (map[string]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT state, COUNT(*) FROM tb_task_submission
		WHERE task_id = $1 GROUP BY state ORDER BY state`, taskID)
	if err != nil {
		return nil, fmt.Errorf("count submissions by state: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
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

// GetTaskStatsBatch computes the §6.3 statistics for every task in
// `ids` in ONE query, keyed by task_id — the batch twin of
// GetTaskStats, identical in scope and cutoff semantics.
func GetTaskStatsBatch(ctx context.Context, q pg.Querier, ids []int64, since time.Time) (map[int64]TaskStats, error) {
	out := make(map[int64]TaskStats, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT t.task_id,
		  COUNT(*) FILTER (WHERE t.state = 'settled'),
		  COUNT(*) FILTER (WHERE t.state = 'rejected'),
		  COUNT(*) FILTER (WHERE t.state = 'failed'),
		  COUNT(*),
		  percentile_cont(0.5) WITHIN GROUP (ORDER BY t.seconds)
		FROM (
		  SELECT sub.task_id, sub.state,
		         EXTRACT(EPOCH FROM (
		           (SELECT e.at FROM tb_task_submission_event e
		             WHERE e.submission_id = sub.submission_id
		               AND e.to_state IN ('settled', 'rejected')
		             ORDER BY e.seq DESC LIMIT 1)
		           -
		           (SELECT e.at FROM tb_task_submission_event e
		             WHERE e.submission_id = sub.submission_id
		             ORDER BY e.seq LIMIT 1)
		         ))::double precision AS seconds,
		         (SELECT e.at FROM tb_task_submission_event e
		           WHERE e.submission_id = sub.submission_id
		           ORDER BY e.seq DESC LIMIT 1) AS terminal_at
		  FROM tb_task_submission sub
		  WHERE sub.task_id = ANY($1) AND sub.state IN ('settled', 'rejected', 'failed')
		) t
		WHERE t.terminal_at IS NOT NULL AND t.terminal_at >= $2
		GROUP BY t.task_id`, ids, since)
	if err != nil {
		return nil, fmt.Errorf("task stats batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s TaskStats
		var median *float64
		if err := rows.Scan(&id, &s.Settled, &s.Rejected, &s.Failed, &s.TerminalTotal, &median); err != nil {
			return nil, err
		}
		s.MedianReplySeconds = median
		out[id] = s
	}
	return out, rows.Err()
}

// CountActiveClaims returns how many of the task's claims are valid
// RIGHT NOW: status active and not past expires_at (the expiry
// reclaimer is asynchronous, so an expired-but-unswept claim is not
// counted). WO-18: the publisher's task_get stats.
func CountActiveClaims(ctx context.Context, q pg.Querier, taskID int64, now time.Time) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_task_claim
		WHERE task_id = $1 AND status = 'active' AND expires_at > $2`,
		taskID, now).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active claims: %w", err)
	}
	return n, nil
}
