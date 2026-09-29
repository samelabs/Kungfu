package service

// Publisher submission listing — spec §8.1 task_submissions: the
// delivery record of one's own task (state, the receiver's reply,
// failure, the executor's stable agent_ref). The results themselves
// live with the publisher's receiver; the platform keeps no copy.

import (
	"context"
	goerrors "errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// publisherSubmissionRow is one listed submission.
type publisherSubmissionRow struct {
	SubmissionID WireID     `json:"submission_id"`
	AgentRef     string     `json:"agent_ref"`
	State        string     `json:"state"`
	Amount       int64      `json:"amount"`
	Reply        *ReplyView `json:"reply"`
	Failure      *string    `json:"failure"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ListSubmissionsForPublisher returns one page of the task's
// submissions (newest first, optional state filter) with the total.
// agent_ref matches the value the receiver saw at delivery (§7.1).
func ListSubmissionsForPublisher(ctx context.Context, pool *pg.Pool, publisherID int64, code, state string, page, pageSize int, agentRefKey []byte) ([]publisherSubmissionRow, int64, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
		return nil, 0, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t.PublisherID != publisherID {
		return nil, 0, errors.New(0, "NOT_OWNER", "Not your task")
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
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	rows, err := repository.ListTaskSubmissions(ctx, pool, t.ID, state, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	out := make([]publisherSubmissionRow, 0, len(rows))
	for i := range rows {
		s := &rows[i]
		row := publisherSubmissionRow{
			SubmissionID: WireID(s.SubmissionID),
			AgentRef:     AgentRef(agentRefKey, t.Code, s.AgentID),
			State:        s.State,
			Amount:       s.Amount,
			Failure:      s.Failure,
			CreatedAt:    s.CreatedAt,
		}
		if s.ResponseCode != nil {
			row.Reply = &ReplyView{Status: *s.ResponseCode}
			if s.ResponseBody != nil {
				row.Reply.Body = *s.ResponseBody
			}
		}
		out = append(out, row)
	}
	return out, total, nil
}
