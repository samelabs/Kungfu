package service

// Publisher verdicts and submission listing (WO-5b) — spec §6.1, §6.2
// (async judgment path), §8.4 publisher-side errors.

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

// SubmitVerdict applies the publisher's judgment to an under_review
// submission (§6.2). If the review window has already closed, the
// platform first applies the timeout acceptance (§6.2 amendment) and
// then returns NOT_UNDER_REVIEW with the settled state. Verdict
// bodies are validated with the contract's own criteria (§6.1);
// invalid bodies leave the submission untouched.
func SubmitVerdict(ctx context.Context, pool *pg.Pool, publisherID int64, submissionID int64, body json.RawMessage, now time.Time) (submissionView, error) {
	sub, err := repository.FindSubmissionByID(ctx, pool, submissionID)
	if goerrors.Is(err, pgx.ErrNoRows) || sub == nil {
		return submissionView{}, errors.New(404, "SUBMISSION_NOT_FOUND", "Submission not found")
	}
	if err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	t, err := repository.FindTaskByID(ctx, pool, sub.TaskID)
	if err != nil || t == nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if t.PublisherID != publisherID {
		return submissionView{}, errors.New(403, "NOT_OWNER", "Not your task")
	}

	// §6.2 amendment: past the deadline (and still under review) the
	// platform settles by timeout FIRST, then reports NOT_UNDER_REVIEW.
	if sub.State == task.SubUnderReview && sub.ReviewDeadline != nil && !now.Before(*sub.ReviewDeadline) {
		if err := settleReviewTimeout(context.WithoutCancel(ctx), pool, sub); err != nil {
			return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		return submissionView{}, errors.NewWithDetails(409, "NOT_UNDER_REVIEW",
			"The review window closed; the submission was accepted by timeout",
			map[string]interface{}{"state": task.SubSettled})
	}
	if sub.State != task.SubUnderReview {
		return submissionView{}, errors.NewWithDetails(409, "NOT_UNDER_REVIEW",
			fmt.Sprintf("Submission is %s, not under review", sub.State),
			map[string]interface{}{"state": sub.State})
	}

	contract, err := versionContract(ctx, pool, t.ID, sub.Version)
	if err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	verdict, err := task.ParseVerdict(body, contract.Acceptance.Criteria)
	if err != nil {
		return submissionView{}, errors.NewWithDetails(400, "VERDICT_INVALID",
			"The verdict does not satisfy §6.1",
			map[string]interface{}{"message": err.Error()})
	}
	verdict.Source = "publisher"
	raw, err := json.Marshal(verdict)
	if err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Internal error")
	}

	event := task.EventReject
	if verdict.Accepted {
		event = task.EventAccept
	}

	writeCtx := context.WithoutCancel(ctx)
	tx, err := pool.TxBegin(writeCtx)
	if err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	current, err := repository.FindSubmissionByIDForUpdate(writeCtx, tx, submissionID)
	if err != nil || current == nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if current.State != task.SubUnderReview {
		return submissionView{}, errors.NewWithDetails(409, "NOT_UNDER_REVIEW",
			fmt.Sprintf("Submission is %s, not under review", current.State),
			map[string]interface{}{"state": current.State})
	}
	if err := repository.SetSubmissionState(writeCtx, tx, submissionID, task.SubUnderReview,
		event, &repository.SetSubmissionStateOpts{Verdict: raw}); err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	switch event {
	case task.EventAccept:
		if err := repository.SettleTaskSubmission(writeCtx, pool, tx, current.TaskID,
			submissionID, current.AgentID, current.Amount); err != nil {
			return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
		}
	case task.EventReject:
		if err := repository.ReleaseTaskReservation(writeCtx, tx, current.TaskID, current.Amount); err != nil {
			return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
		}
	}
	if err := tx.Commit(writeCtx); err != nil {
		return submissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}

	return submissionViewByID(writeCtx, pool, submissionID)
}

// publisherSubmissionRow is one listed submission (§6.2 async-no-
// receiver path: publishers read payloads and judge from this list).
type publisherSubmissionRow struct {
	SubmissionID   int64           `json:"submission_id"`
	AgentRef       string          `json:"agent_ref"`
	Version        int32           `json:"version"`
	State          string          `json:"state"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Verdict        []byte          `json:"verdict,omitempty"`
	Failure        *string         `json:"failure,omitempty"`
	ReviewDeadline *time.Time      `json:"review_deadline,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ListSubmissionsForPublisher returns one page of the task's
// submissions (newest first, optional state filter) with the total.
// agent_ref matches the value the receiver saw at delivery (§7.1).
func ListSubmissionsForPublisher(ctx context.Context, pool *pg.Pool, publisherID int64, code, state string, page, pageSize int, agentRefKey []byte) ([]publisherSubmissionRow, int64, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
		return nil, 0, errors.New(404, "NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if t.PublisherID != publisherID {
		return nil, 0, errors.New(403, "NOT_OWNER", "Not your task")
	}
	if state != "" {
		valid := false
		for _, s := range task.SubmissionStates {
			if s == state {
				valid = true
			}
		}
		if !valid {
			return nil, 0, errors.NewWithDetails(400, "VALIDATION_FAILED",
				"Unknown submission state",
				map[string]interface{}{"errors": []map[string]string{
					{"field": "state", "message": "must be one of the §5.4 states"}}})
		}
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	if page < 1 {
		page = 1
	}
	total, err := repository.CountTaskSubmissions(ctx, pool, t.ID, state)
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	rows, err := repository.ListTaskSubmissions(ctx, pool, t.ID, state, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	out := make([]publisherSubmissionRow, 0, len(rows))
	for i := range rows {
		s := &rows[i]
		out = append(out, publisherSubmissionRow{
			SubmissionID:   s.SubmissionID,
			AgentRef:       AgentRef(agentRefKey, t.Code, s.AgentID),
			Version:        s.Version,
			State:          s.State,
			Payload:        s.Payload,
			Verdict:        s.Verdict,
			Failure:        s.Failure,
			ReviewDeadline: s.ReviewDeadline,
			CreatedAt:      s.CreatedAt,
		})
	}
	return out, total, nil
}
