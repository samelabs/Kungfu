package server

// POST /api/v1/<tool> — the HTTP twin of the MCP registry (WO-7a):
// Bearer Agent-key authentication through the same
// auth.VerifyAgentKey + AgentLookup seam as /mcp, dispatch into the
// same mcpserver registry, and the same §8.2 envelope with the
// protocol-layer code→status table.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/mcpserver"
	"kungfu.md/internal/model"
)

func (s *Server) handleAPIV1Tool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "tool")
	def, ok := mcpserver.Tool(name)
	if !ok {
		writeAPIV1JSON(w, http.StatusNotFound,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "UNKNOWN_TOOL", "message": "Unknown tool " + name},
			})
		return
	}

	// Public tools (the registry's single allowlist) skip the Bearer
	// gate; account_register keeps its per-IP register rate limit.
	var bot *model.Bot
	if !def.Public {
		key := bearerToken(r)
		verified, err := auth.VerifyAgentKey(r.Context(), key, s.mcpAgentLookup)
		if err != nil || verified == nil {
			writeAPIV1JSON(w, http.StatusUnauthorized,
				map[string]any{"ok": false,
					"error": map[string]any{"code": "UNAUTHORIZED", "message": "Agent key is invalid or missing"},
				})
			return
		}
		bot = verified
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpserver.MaxRequestBodyBytes+1))
	if err != nil {
		writeAPIV1JSON(w, http.StatusUnprocessableEntity,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Unreadable request body"},
			})
		return
	}
	if len(body) > mcpserver.MaxRequestBodyBytes {
		// over the cap BEFORE any decoding: the same §8.2 structure,
		// the protocol status table's 413
		env := map[string]any{"ok": false, "error": map[string]any{
			"code":    "PAYLOAD_TOO_LARGE",
			"message": fmt.Sprintf("Request body exceeds %d bytes", mcpserver.MaxRequestBodyBytes),
		}, "next_action": "revise", "retry_after": nil}
		writeAPIV1JSON(w, http.StatusRequestEntityTooLarge, env)
		return
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	var args json.RawMessage = body
	if !json.Valid(args) {
		writeAPIV1JSON(w, http.StatusUnprocessableEntity,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Body must be a JSON object"},
			})
		return
	}

	deps := s.mcpDeps()
	// public /api/v1 calls still carry the HTTP request context so the
	// register limiter sees the trusted client IP
	ctx := deps.WithHTTPRequest(r.Context(), r)
	env, status := mcpserver.CallTool(ctx, deps, name, bot, args)
	writeAPIV1JSON(w, status, env)
}

func writeAPIV1JSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(`{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"An internal error occurred"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// mcpDeps returns the MCP wiring for the HTTP dispatcher.
func (s *Server) mcpDeps() *mcpserver.Deps {
	deps := s.mcpDepsValue
	return &deps
}

// bearerToken extracts the Authorization: Bearer value.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
