package service

// Executor reports (§8.1 work_report) and data retention (§9).
// Retention clears closed tasks' harness material
// after 30 days; payloads are already cleared when a submission is
// decided.

import (
	"context"
	goerrors "errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// reportReasonMaxRunes is the §-derived bound (tb_task_report.reason
// is VARCHAR(2000)).
const reportReasonMaxRunes = 2000

// ReportTask files a report against a task (§8.1 work_report).
// Re-reporting while a previous report is still open returns that
// report (idempotent). Reporting one's own task is allowed.
func ReportTask(ctx context.Context, pool *pg.Pool, agentID int64, code, reason string) (map[string]any, error) {
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > reportReasonMaxRunes {
		return nil, errors.NewWithDetails(400, "VALIDATION_FAILED",
			"reason must be 1-2000 characters",
			map[string]any{"errors": []map[string]string{
				{"field": "reason", "message": fmt.Sprintf("must be 1–%d characters after trimming, got %d",
					reportReasonMaxRunes, n)}}})
	}
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t.Status == task.TaskPaused {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}

	if id, ok, err := repository.FindOpenReportByReporterTask(ctx, pool, t.ID, agentID); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	} else if ok {
		return map[string]any{"report_id": WireID(id), "status": "open"}, nil
	}
	id, err := repository.InsertTaskReport(ctx, pool, t.ID, agentID, reason)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return map[string]any{"report_id": WireID(id), "status": "open"}, nil
}

// retentionWindow is §9: harness material is kept 30 days after the
// task closes.
const retentionWindow = 30 * 24 * time.Hour
