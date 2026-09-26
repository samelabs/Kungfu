package server

// POST /api/v1/<tool> — the HTTP twin of the MCP registry (WO-7a):
// Bearer Agent-key authentication through the same
// auth.VerifyAgentKey + AgentLookup seam as /mcp, dispatch into the
// same mcpserver registry, and the same §8.2 envelope with the
// protocol-layer code→status table.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/mcpserver"
)

// apiV1BodyLimit bounds one tool-call body (§5.3 payload 512 KB plus
// envelope headroom).
const apiV1BodyLimit = 512*1024 + 4096

func (s *Server) handleAPIV1Tool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "tool")
	if _, ok := mcpserver.ExecutorTool(name); !ok {
		writeAPIV1JSON(w, http.StatusNotFound,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "UNKNOWN_TOOL", "message": "Unknown tool " + name},
			})
		return
	}

	// Bearer authentication — the same identity authority as /mcp.
	key := bearerToken(r)
	bot, err := auth.VerifyAgentKey(r.Context(), key, s.mcpAgentLookup)
	if err != nil || bot == nil {
		writeAPIV1JSON(w, http.StatusUnauthorized,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "UNAUTHORIZED", "message": "Agent key is invalid or missing"},
			})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, apiV1BodyLimit))
	if err != nil {
		writeAPIV1JSON(w, http.StatusUnprocessableEntity,
			map[string]any{"ok": false,
				"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Unreadable request body"},
			})
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
	env, status := mcpserver.CallExecutorTool(r.Context(), deps, name, bot, args)
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
