package service

// Platform task governance (WO-8b): the /samelabs console's actions
// over the Task 1.0 model. PlatformCloseTask is the §4 平台关闭 close —
// the same close core as the publisher's CloseTask (applyStatusChange:
// active claims keep their reservation and may still submit until
// expiry, no renewal; in-flight submissions complete) with event
// platform_close, a mandatory closed_reason, and every still-open
// report of the task actioned. ResolveReport disposes of one report:
// dismissed, or the reported task closed through that same core.
//
// Both actions write the existing admin audit trail
// (tb_admin_audit_logs) inside the SAME transaction as the mutation —
// the WithAuditTx invariant of internal/admin, applied here because
// the mutation lives in this service's transaction.

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// platformCloseReasonMaxRunes bounds closed_reason: the column is
// VARCHAR(500), §6.1 bounds reasons at 500 and "characters" count as
// runes (WO-2a).
const platformCloseReasonMaxRunes = 500

// Report statuses — §2 Report (the tb_task_report CHECK). The task
// kernel owns no transition machine for them; they live with the SQL.
const (
	reportOpen      = "open"
	reportDismissed = "dismissed"
	reportActioned  = "actioned"
)

// PlatformCloseTask closes any non-closed task as platform governance
// (§4 平台关闭, event platform_close) with a 1–500 character reason.
// Reports still open on the task are actioned in the same
// transaction. The claim/reservation handling is the publisher
// close's own (applyStatusChange): nothing is released up front.
func PlatformCloseTask(ctx context.Context, pool *pg.Pool, adminID int64, code, reason string) (map[string]interface{}, error) {
	return platformClose(ctx, pool, adminID, code, reason, nil)
}

// platformClose is the shared close path: PlatformCloseTask with no
// report context, ResolveReport's close action with the report id.
func platformClose(ctx context.Context, pool *pg.Pool, adminID int64, code, reason string, reportID *int64) (map[string]interface{}, error) {
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > platformCloseReasonMaxRunes {
		return nil, validationFailed([]task.FieldError{{
			Field:   "reason",
			Message: fmt.Sprintf("must be 1–%d characters after trimming, got %d", platformCloseReasonMaxRunes, n),
		}})
	}

	var actioned int64
	after, err := applyStatusChange(ctx, pool, code, nil, task.EventPlatformClose, &reason,
		func(ctx context.Context, tx pgx.Tx, t *repository.TaskRow) error {
			admin, err := repository.FindAdminByID(ctx, tx, adminID)
			if err != nil {
				return errors.New(0, "INTERNAL_ERROR", "Database error")
			}
			if admin == nil {
				return errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
			}
			if actioned, err = repository.ActionOpenReports(ctx, tx, t.ID); err != nil {
				return errors.New(0, "INTERNAL_ERROR", "Database error")
			}
			before, err := auditJSON(map[string]any{"status": t.Status})
			if err != nil {
				return err
			}
			afterJSON, err := auditJSON(map[string]any{
				"status": task.TaskClosed, "closed_reason": reason, "reports_actioned": actioned,
			})
			if err != nil {
				return err
			}
			entry := &model.AdminAuditLog{
				ActorAdminID:  &adminID,
				ActorUsername: admin.Username,
				Action:        "task.platform_close",
				TargetType:    strPtr("task"),
				TargetID:      strPtr(t.Code),
				Success:       true,
				BeforeJSON:    before,
				AfterJSON:     afterJSON,
			}
			if reportID != nil {
				meta, err := auditJSON(map[string]any{"report_id": *reportID, "via": "report"})
				if err != nil {
					return err
				}
				entry.MetadataJSON = meta
			}
			if err := repository.InsertAdminAuditLog(ctx, tx, entry); err != nil {
				return errors.New(0, "INTERNAL_ERROR", "Database error")
			}
			if reportID != nil {
				if err := insertReportResolvedAudit(ctx, tx, adminID, admin.Username, *reportID, t.Code,
					reportOpen, reportActioned, "report.close"); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	view, err := taskView(ctx, pool, after)
	if err != nil {
		return nil, err
	}
	view["reports_actioned"] = actioned
	return view, nil
}

// ResolveReport disposes of one task report (WO-8b): action "dismiss"
// marks it dismissed; action "close" closes the reported task through
// the platform close, which actions every open report of the task —
// the resolved one included — in that transaction.
func ResolveReport(ctx context.Context, pool *pg.Pool, adminID int64, reportID int64, action string) (map[string]any, error) {
	switch action {
	case "dismiss", "close":
	default:
		return nil, validationFailed([]task.FieldError{{
			Field: "action", Message: `must be "dismiss" or "close"`,
		}})
	}

	report, err := repository.FindReportByID(ctx, pool, reportID)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New(404, "NOT_FOUND", "Report not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if report == nil {
		return nil, errors.New(404, "NOT_FOUND", "Report not found")
	}

	if action == "close" {
		// The report itself needs no separate write: the close's
		// ActionOpenReports moves every open report of the task
		// (this one included) to actioned.
		code, err := taskCodeByID(ctx, pool, report.TaskID)
		if err != nil {
			return nil, err
		}
		view, err := platformClose(ctx, pool, adminID, code,
			fmt.Sprintf("Closed by platform governance (report #%d)", reportID), &reportID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"report_id": reportID, "status": reportActioned, "task": view,
		}, nil
	}

	// dismiss: open → dismissed in one transaction with its audit row.
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	locked, err := repository.FindReportByIDForUpdate(ctx, tx, reportID)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New(404, "NOT_FOUND", "Report not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if locked.Status != reportOpen {
		return nil, errors.NewWithDetails(0, "INVALID_STATE",
			fmt.Sprintf("Report is not open (status %q)", locked.Status),
			map[string]interface{}{"status": locked.Status})
	}
	admin, err := repository.FindAdminByID(ctx, tx, adminID)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if admin == nil {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	if err := repository.SetReportStatus(ctx, tx, reportID, reportOpen, reportDismissed); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := insertReportResolvedAudit(ctx, tx, adminID, admin.Username, reportID, "",
		reportOpen, reportDismissed, "report.dismiss"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return map[string]any{"report_id": reportID, "status": reportDismissed}, nil
}

// insertReportResolvedAudit appends the report-resolution audit row on
// the caller's transaction. The task code is only known for the close
// path; the dismiss row targets the report alone.
func insertReportResolvedAudit(ctx context.Context, tx pgx.Tx, adminID int64, username string,
	reportID int64, taskCode, from, to, action string) error {
	before, err := auditJSON(map[string]any{"status": from})
	if err != nil {
		return err
	}
	after := map[string]any{"status": to}
	if taskCode != "" {
		after["task"] = taskCode
	}
	afterJSON, err := auditJSON(after)
	if err != nil {
		return err
	}
	entry := &model.AdminAuditLog{
		ActorAdminID:  &adminID,
		ActorUsername: username,
		Action:        action,
		TargetType:    strPtr("task_report"),
		TargetID:      strPtr(fmt.Sprint(reportID)),
		Success:       true,
		BeforeJSON:    before,
		AfterJSON:     afterJSON,
	}
	if err := repository.InsertAdminAuditLog(ctx, tx, entry); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return nil
}

// auditJSON marshals plain audit facts; an unmarshalable fact aborts
// the mutation instead of writing a lossy row (the WithAuditTx rule).
func auditJSON(v interface{}) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Audit record could not be serialized")
	}
	return b, nil
}

// taskCodeByID resolves a task id to its code (report rows carry only
// the id); the FK guarantees the task exists.
func taskCodeByID(ctx context.Context, pool *pg.Pool, taskID int64) (string, error) {
	t, err := repository.FindTaskByID(ctx, pool, taskID)
	if err != nil {
		return "", errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t == nil {
		return "", errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	return t.Code, nil
}
