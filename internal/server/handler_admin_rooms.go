package server

// Admin room disposal (migration 035): the platform's one-click door.
// Same core routine the governor tool and the retention sweep use —
// settle, then hard-delete; the audit row keeps metadata only.

import (
	"encoding/json"
	"net/http"

	"kungfu.md/internal/service"
)

func (s *Server) handleAdminRoomDispose(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminMutation(r, "rooms.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := r.PathValue("code")
	if code == "" {
		http.Error(w, "room code required", http.StatusBadRequest)
		return
	}
	res, err := service.DisposeThreadByCode(r.Context(), s.Pool, code, "admin", "admin:"+principal.Admin.Username)
	if err != nil {
		handleAppError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(res)
}
