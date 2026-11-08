package server

// POST /api/owner/tool/{tool} — the Owner console's single bridge into
// the unified tool registry (WO-8a). No business logic lives here:
// ownerMutation enforces the JSON/CSRF gate, requireOwnerAuth resolves
// the session bot, and mcpserver.CallTool runs the tool with the §8.2
// envelope. The allowlist is the §8.1 publisher task_* column, the one
// thread write the console's room detail page offers (thread_update:
// the governor-only profile edit, 3.2.0) and the read-only projections
// the console renders (WO-30 §3): memory_list / memory_get for the
// Memory view, todo_list for the Turn view, thread_list / thread_get
// for the Threads view. Every other name — including all executor,
// account and the remaining thread mutation tools — is 404
// UNKNOWN_TOOL: the console's thread write path is the profile edit
// and nothing else.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/mcpserver"
)

// ownerTools is the console allowlist: the §8.1 publisher column, the
// thread profile edit (thread_update, governor-checked by the tool
// itself), and the read-only tools the workbench views render.
var ownerTools = map[string]bool{
	"bounty_create": true, "bounty_update": true, "bounty_open": true,
	"bounty_pause": true, "bounty_close": true, "bounty_fund": true,
	"bounty_refund": true, "bounty_get": true, "bounty_list": true,
	"bounty_submissions": true, "memory_list": true, "memory_get": true,
	"todo_list": true, "thread_list": true, "thread_get": true,
	"thread_update": true,
}

func (s *Server) handleOwnerTool(w http.ResponseWriter, r *http.Request) {
	tool := chi.URLParam(r, "tool")
	if !ownerTools[tool] {
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

	// Body cap: mcpserver.MaxRequestBodyBytes, shared with /mcp and /api/v1.
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpserver.MaxRequestBodyBytes+1))
	if err != nil {
		mcpserver.WriteOwnerToolJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Unreadable request body"},
		})
		return
	}
	if len(body) > mcpserver.MaxRequestBodyBytes {
		// over the cap BEFORE any decoding: the same envelope and 413
		// as /api/v1 (WO-8b parity fix)
		mcpserver.WriteOwnerToolJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"ok":          false,
			"error":       map[string]any{"code": "PAYLOAD_TOO_LARGE", "message": fmt.Sprintf("Request body exceeds %d bytes", mcpserver.MaxRequestBodyBytes)},
			"next_action": "revise",
			"retry_after": nil,
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
