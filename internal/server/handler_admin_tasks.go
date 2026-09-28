package server

// Task governance API (WO-8b): the /api/samelabs surface of the
// platform task console. Same shape as the rewards handlers: HTTP parse
// → admin auth/CSRF/permission wiring → admin read or service
// governance call → DTO serialization. No SQL here.

import (
	"encoding/json"
	"net/http"

	"kungfu.md/internal/admin"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/service"
)

// handleServiceAppError maps a service-layer app error onto HTTP.
// The task service builds protocol-relative errors (HTTPCode 0)
// whose status is the single code→status table's (WO-7d); admin
// domain errors carry explicit codes and go through handleAppError.
func handleServiceAppError(w http.ResponseWriter, err error) {
	if ae, ok := apperrors.IsAppError(err); ok && ae.HTTPCode == 0 {
		if status, mapped := apperrors.StatusFor(ae.Code); mapped {
			ErrorResponse(w, status, ae.Code, ae.Message, ae.Details)
			return
		}
	}
	handleAppError(w, err)
}

// adminTaskDTO projects an admin task row for the browser. The
// economic integers (price, the four budget counters, available)
// travel as canonical decimal strings — the browser must never route
// them through JS Number.
func adminTaskDTO(t *admin.TaskRow) map[string]interface{} {
	dto := map[string]interface{}{
		"code":          t.Code,
		"title":         t.Title,
		"publisher_id":  t.PublisherID,
		"publisher":     t.PublisherName,
		"status":        t.Status,
		"version":       t.Version,
		"budget_locked": econString(t.BudgetLocked),
		"settled":       econString(t.Settled),
		"reserved":      econString(t.Reserved),
		"refunded":      econString(t.Refunded),
		"available":     econString(t.Available),
		"slots":         t.Slots,
		"price":         econString(t.Price),
		"paused_reason": nullableStringJSON(t.PausedReason),
		"closed_reason": nullableStringJSON(t.ClosedReason),
		"created_at":    t.CreatedAt,
		"updated_at":    t.UpdatedAt,
	}
	if len(t.Contract) > 0 {
		// The effective contract, passed through verbatim (read-only
		// JSON for the detail page).
		dto["contract"] = json.RawMessage(t.Contract)
	}
	return dto
}

func adminReportDTO(r *admin.ReportRow) map[string]interface{} {
	return map[string]interface{}{
		"id":          r.ID,
		"task_id":     r.TaskID,
		"task_code":   r.TaskCode,
		"task_status": r.TaskStatus,
		"reporter_id": r.ReporterID,
		"reporter":    r.ReporterName,
		"reason":      r.Reason,
		"status":      r.Status,
		"created_at":  r.CreatedAt,
		"updated_at":  r.UpdatedAt,
	}
}

func (s *Server) handleAdminTasksList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminPermission(r, "tasks.read")
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	items, total, err := admin.ListPlatformTasks(r.Context(), s.Pool, principal, admin.TaskFilter{
		Status: q.Get("status"), Page: page, Size: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, adminTaskDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"tasks": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminTaskGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminPermission(r, "tasks.read")
	if err != nil {
		handleAppError(w, err)
		return
	}
	d, err := admin.GetPlatformTask(r.Context(), s.Pool, principal, r.PathValue("code"))
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{
		"task":        adminTaskDTO(d.Task),
		"stats":       taskStatsDTO(d.Stats),
		"counts":      d.Counts,
		"submissions": recentSubmissionsDTO(d.Submissions),
	}, "")
}

// taskStatsDTO projects the §6.3 30-day statistics (nil rates stay
// null — no denominator yet).
func taskStatsDTO(s admin.TaskStats) map[string]interface{} {
	rate := func(num, den int64) interface{} {
		if den == 0 {
			return nil
		}
		return float64(num) / float64(den)
	}
	var median interface{}
	if s.MedianReplySeconds != nil {
		median = *s.MedianReplySeconds
	}
	return map[string]interface{}{
		"settled":              s.Settled,
		"rejected":             s.Rejected,
		"failed":               s.Failed,
		"terminal_total":       s.TerminalTotal,
		"accept_rate":          rate(s.Settled, s.Settled+s.Rejected),
		"median_reply_seconds": median,
		"failure_rate":         rate(s.Failed, s.TerminalTotal),
	}
}

// recentSubmissionsDTO lists the recent submissions without payload
// bodies (the queue shows states and facts, not delivery material).
func recentSubmissionsDTO(rows []admin.SubmissionFact) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, map[string]interface{}{
			"submission_id": r.SubmissionID,
			"version":       r.Version,
			"agent_id":      r.AgentID,
			"amount":        econString(r.Amount),
			"state":         r.State,
			"failure":       nullableStringJSON(r.Failure),
			"created_at":    r.CreatedAt,
		})
	}
	return out
}

// handleAdminTaskClose is the §4 平台关闭 via the service governance
// action (reason 1–500, open reports actioned, audit inside the
// mutation's transaction).
func (s *Server) handleAdminTaskClose(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBodyNumbers(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	principal, err := s.requireAdminMutation(r, "tasks.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	reason, ok := input["reason"].(string)
	if !ok {
		ErrorResponse(w, 400, "INVALID_REASON", "reason must be a string", nil)
		return
	}
	view, err := service.PlatformCloseTask(r.Context(), s.Pool, principal.Admin.ID, r.PathValue("code"), reason)
	if err != nil {
		handleServiceAppError(w, err)
		return
	}
	SuccessResponse(w, view, "Task closed")
}

func (s *Server) handleAdminReportsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminPermission(r, "reports.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	// the queue's default view is open — the same default as the page
	status := q.Get("status")
	if status == "" {
		status = "open"
	}
	items, total, err := admin.ListPlatformReports(r.Context(), s.Pool, principal, admin.ReportFilter{
		Status: status, Page: page, Size: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, adminReportDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"reports": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

// handleAdminReportResolve disposes of one report: dismiss, or close
// the reported task (which actions the task's open reports).
func (s *Server) handleAdminReportResolve(w http.ResponseWriter, r *http.Request, action string) {
	if _, err := parseAdminOptionalJSONObject(r); err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	principal, err := s.requireAdminMutation(r, "reports.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	out, err := service.ResolveReport(r.Context(), s.Pool, principal.Admin.ID, id, action)
	if err != nil {
		handleServiceAppError(w, err)
		return
	}
	message := "Report dismissed"
	if action == "close" {
		message = "Task closed"
	}
	SuccessResponse(w, out, message)
}
