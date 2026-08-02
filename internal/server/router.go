package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// Server holds all dependencies.
type Server struct {
	Config      *config.Config
	Pool        *pg.Pool
	RateLimiter *ratelimit.Limiter
	Router      http.Handler
}

// New creates a new server with all routes configured.
func New(cfg *config.Config, pool *pg.Pool) *Server {
	// Build rate limiter from config
	rlConfigs := make(map[string]ratelimit.Config)
	for action, rlc := range cfg.RateLimits {
		enabled := true
		if rlc.Enabled != nil {
			enabled = *rlc.Enabled
		}
		rlConfigs[action] = ratelimit.Config{
			Window:  rlc.Window,
			Limit:   rlc.Limit,
			Enabled: enabled,
		}
	}
	rl := ratelimit.NewLimiter(rlConfigs)

	s := &Server{
		Config:      cfg,
		Pool:        pool,
		RateLimiter: rl,
	}

	s.Router = s.buildRouter()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

// buildRouter creates the chi router with all routes.
func (s *Server) buildRouter() http.Handler {
	r := chi.NewRouter()

	// Recovery middleware (catches panics)
	r.Use(s.recoverMiddleware)

	// -- Static file routes --
	r.Get("/robots.txt", serveStaticFile("robots.txt", "text/plain; charset=utf-8", ""))
	r.Get("/sitemap.xml", serveStaticFile("sitemap.xml", "application/xml; charset=utf-8", ""))
	r.Get("/llms.txt", serveStaticFile("llms.txt", "text/plain; charset=utf-8", ""))
	r.Get("/openai.json", serveStaticFile("openai.json", "application/json; charset=utf-8", "public, max-age=300"))
	r.Get("/.well-known/openai.json", serveStaticFile(".well-known/openai.json", "application/json; charset=utf-8", "public, max-age=300"))
	r.Get("/kungfu_skill.md", serveStaticFile("kungfu_skill.md", "text/markdown; charset=utf-8", ""))
	r.Get("/owner_task_guide.md", serveStaticFile("owner_task_guide.md", "text/markdown; charset=utf-8", ""))
	r.Get("/manifest.webmanifest", serveStaticFile("manifest.webmanifest", "application/manifest+json; charset=utf-8", "public, max-age=300"))
	r.Get("/sw.js", serveStaticFile("sw.js", "application/javascript; charset=utf-8", "no-cache, no-store, must-revalidate"))

	// Assets (/assets/*)
	r.Get("/assets/*", serveAssets())

	// -- API routes: Agent (X-Bot-Key auth) --
	r.Post("/api/register", s.handleRegister)
	r.Get("/api/ping", s.handlePing)

	// Kungfu CRUD
	r.Get("/api/kungfus", s.handleKungfuList)
	r.Post("/api/kungfus", s.handleKungfuPush)
	r.Get("/api/kungfus/{code}", s.handleKungfuGet)
	r.Delete("/api/kungfus/{code}", s.handleKungfuDelete)
	r.Post("/api/kungfus/{code}/share", s.handleKungfuShare)
	r.Post("/api/kungfus/{code}/unshare", s.handleKungfuUnshare)

	// Tasks (agent)
	r.Get("/api/tasks", s.handleTaskList)
	r.Get("/api/tasks/{code}", s.handleTaskGet)
	r.Post("/api/tasks/{code}/submissions", s.handleTaskSubmit)

	// -- API routes: Owner (session auth) --
	r.Get("/api/owner/session", s.handleOwnerSessionGet)
	r.Post("/api/owner/session", s.handleOwnerSessionLogin)
	r.Delete("/api/owner/session", s.handleOwnerSessionLogout)

	r.Get("/api/account", s.handleAccount)
	r.Get("/api/key", s.handleKey)
	r.Post("/api/change-password", s.handleChangePassword)
	r.Post("/api/reset-key", s.handleResetKey)

	r.Get("/api/owner/tasks", s.handleOwnerTasksList)
	r.Get("/api/owner/tasks/{code}", s.handleOwnerTaskGet)
	r.Post("/api/owner/tasks", s.handleOwnerTaskCreate)
	r.Post("/api/owner/tasks/{code}/open", s.handleOwnerTaskOpen)
	r.Post("/api/owner/tasks/{code}/close", s.handleOwnerTaskClose)
	r.Post("/api/owner/tasks/{code}/add-budget", s.handleOwnerTaskAddBudget)
	r.Post("/api/owner/tasks/{code}/refund", s.handleOwnerTaskRefund)
	r.Post("/api/owner/tasks/{code}/edit", s.handleOwnerTaskEdit)

	r.Post("/api/testtask/{code}", s.handleTestTask)

	r.Get("/api/owner/logs", s.handleOwnerLogs)

	// -- Web routes (HTML) --
	r.Get("/", s.agentHomeHandler())
	r.Get("/credits", s.handleCredits)
	r.Get("/owner", s.handleOwnerPage("overview"))
	r.Get("/owner/login", s.handleOwnerPage("login"))
	r.Get("/owner/register", s.handleOwnerPage("register"))
	r.Get("/owner/account", s.handleOwnerPage("account"))
	r.Get("/owner/key", s.handleOwnerPage("key"))
	r.Get("/owner/tasks", s.handleOwnerPage("tasks"))
	r.Get("/owner/tasks/new", s.handleOwnerPage("task_new"))
	r.Get("/owner/logs", s.handleOwnerPage("logs"))
	r.Get("/owner/task-guide", s.handleTaskGuide)

	// 404 for everything else
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		NotFound(w)
	})

	return r
}

// recoverMiddleware catches panics and returns 500.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				ErrorResponse(w, 500, "INTERNAL_ERROR", "An internal error occurred", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// -- Helper functions --

// parseJSONBodyRequired reads and parses JSON, returning error if invalid.
// requireObject: if true, body must be a JSON object (not just any valid JSON).
// emptyMessage: message for empty body case ("Request body must be valid JSON" vs "...JSON object")
func parseJSONBodyRequired(r *http.Request, requireObject bool, emptyMessage string) (map[string]interface{}, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &parseError{msg: emptyMessage}
	}

	if len(body) == 0 {
		if requireObject {
			return nil, &parseError{msg: emptyMessage}
		}
		// Empty body is allowed for reset-key: treat as empty input, no error.
		return map[string]interface{}{}, nil
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, &parseError{msg: emptyMessage}
	}

	if requireObject && data == nil {
		return nil, &parseError{msg: emptyMessage}
	}

	return data, nil
}

type parseError struct {
	msg string
}

func (e *parseError) Error() string { return e.msg }

// getIntParam gets an integer URL parameter with default.
func getIntParam(r *http.Request, key string, def int) int {
	v := chi.URLParam(r, key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// getQueryInt gets an integer query parameter with default.
func getQueryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// clampInt clamps a value between min and max.
func clampInt(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

// Placeholder stubs - will be implemented in handler files
// These reference functions defined in handler_*.go files

// Context keys for storing bot in request context
