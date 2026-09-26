package server

// POST /api/owner/tool/{tool} — the Owner console's single bridge into
// the unified tool registry (WO-8a). No business logic lives here:
// ownerMutation enforces the JSON/CSRF gate, requireOwnerAuth resolves
// the session bot, and mcpserver.CallTool runs the tool with the §8.2
// envelope. Only the eleven publisher task_* tools are exposed; every
// other name — including all executor, account and memory tools — is
// 404 UNKNOWN_TOOL.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/mcpserver"
)

// ownerToolBodyLimit bounds one console tool call (512 KB payload cap
// plus envelope headroom — the same budget as /api/v1).
const ownerToolBodyLimit = 512*1024 + 4096

// publisherTools is the console allowlist: the §8.1 publisher column.
var publisherTools = map[string]bool{
	"task_create": true, "task_update": true, "task_open": true,
	"task_pause": true, "task_close": true, "task_fund": true,
	"task_refund": true, "task_get": true, "task_list": true,
	"task_submissions": true, "task_verdict": true,
}

func (s *Server) handleOwnerTool(w http.ResponseWriter, r *http.Request) {
	tool := chi.URLParam(r, "tool")
	if !publisherTools[tool] {
		mcpserver.WriteOwnerToolJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "UNKNOWN_TOOL", "message": "Unknown tool " + tool},
		})
		return
	}

	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		mcpserver.WriteOwnerToolJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "UNAUTHORIZED", "message": "Owner sign-in required"},
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, ownerToolBodyLimit+1))
	if err != nil || len(body) > ownerToolBodyLimit {
		mcpserver.WriteOwnerToolJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Unreadable request body"},
		})
		return
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	var args json.RawMessage = body
	if !json.Valid(args) {
		mcpserver.WriteOwnerToolJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Body must be a JSON object"},
		})
		return
	}

	// The session bot IS the caller; the registry's publisher handlers
	// treat it exactly like the MCP/HTTP publisher.
	env, status := mcpserver.CallTool(r.Context(), s.mcpDeps(), tool, bot, args)
	mcpserver.WriteOwnerToolJSON(w, status, env)
}
