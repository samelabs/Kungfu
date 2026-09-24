package server

// 011 Platform Account Administration HTTP surface. Handlers do:
// HTTP parse → admin auth/CSRF/permission wiring → admin
// control-plane call → DTO serialization. No repository/credits
// imports; no SQL.
//
// Reads require accounts.read (via admin.ListAccounts /
// admin.GetAccount). Mutations go through requireAdminMutation
// (auth + CSRF + accounts.manage). The balance travels as a
// canonical decimal STRING (econString) — the admin browser must
// never route Credits through JS Number.

import (
	"net/http"
	"strconv"

	"kungfu.md/internal/admin"
)

// -- DTOs (explicit allowlists) --

func adminAccountDTO(a admin.AccountView) map[string]interface{} {
	return map[string]interface{}{
		"id":       a.ID,
		"bot_name": a.BotName,
		"status":   a.Status,
		// balance: authoritative Credits balance column on the wire
		// as a canonical decimal STRING (int64 corruption boundary).
		"balance":        econString(a.Balance),
		"api_key_last4":  a.APIKeyLast4,
		"key_issued_at":  nullableTimeJSON(a.KeyIssuedAt),
		"last_active_at": nullableTimeJSON(a.LastActive),
		"created_at":     a.CreatedAt,
		"updated_at":     a.UpdatedAt,
	}
}

func adminAccountDetailDTO(d admin.AccountDetailView) map[string]interface{} {
	out := adminAccountDTO(d.AdminAccount)
	// Two DISTINCT business facts — never merged into one count.
	out["published_task_count"] = d.PublishedTaskCount
	out["submission_count"] = d.SubmissionCount
	out["kungfu_count"] = d.KungfuCount
	return out
}

// -- reads --

func (s *Server) handleAdminAccountsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	items, total, err := admin.ListAccounts(r.Context(), s.Pool, principal, admin.AccountListFilter{
		Status:   q.Get("status"),
		Q:        q.Get("q"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, adminAccountDTO(items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"accounts": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminAccountGet(w http.ResponseWriter, r *http.Request) {
	botID, ok := adminAccountIDParam(w, r)
	if !ok {
		return
	}
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	d, err := admin.GetAccount(r.Context(), s.Pool, principal, botID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	if d == nil {
		ErrorResponse(w, 404, "ACCOUNT_NOT_FOUND", "Platform account not found", nil)
		return
	}
	SuccessResponse(w, map[string]interface{}{"account": adminAccountDetailDTO(*d)}, "")
}

// -- mutations (accounts.manage + admin CSRF authority) --

func (s *Server) handleAdminAccountDisable(w http.ResponseWriter, r *http.Request) {
	botID, ok := adminAccountIDParam(w, r)
	if !ok {
		return
	}
	principal, err := s.requireAdminMutation(r, "accounts.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	if err := admin.DisablePlatformAccount(r.Context(), s.Pool, principal, botID); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": botID, "status": "disabled"}, "Platform account disabled")
}

func (s *Server) handleAdminAccountEnable(w http.ResponseWriter, r *http.Request) {
	botID, ok := adminAccountIDParam(w, r)
	if !ok {
		return
	}
	principal, err := s.requireAdminMutation(r, "accounts.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	if err := admin.EnablePlatformAccount(r.Context(), s.Pool, principal, botID); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": botID, "status": "active"}, "Platform account enabled")
}

// adminAccountIDParam parses the {id} path value as a bot id.
func adminAccountIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		ErrorResponse(w, 400, "INVALID_ACCOUNT_ID", "Invalid platform account id", nil)
		return 0, false
	}
	return id, true
}
