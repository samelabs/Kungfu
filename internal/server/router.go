package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"kungfu.md/internal/config"
	"kungfu.md/internal/mcpserver"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
)

// Server holds all dependencies.
type Server struct {
	Config         *config.Config
	Pool           *pg.Pool
	RateLimiter    *ratelimit.Limiter
	Router         http.Handler
	TrustedProxies []*net.IPNet

	// creemBaseOverride redirects the Creem client at a test fake.
	// Empty in production; never settable from requests or env.
	creemBaseOverride string
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
		Config:         cfg,
		Pool:           pool,
		RateLimiter:    rl,
		TrustedProxies: cfg.TrustedProxyCIDRs,
	}

	s.Router = s.buildRouter()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

// buildRouter creates the chi router with all routes and the FIXED
// production request deadline (25s — the single budget authority).
func (s *Server) buildRouter() http.Handler {
	return s.buildRouterWithDeadline(requestDeadlineBudgetDefault)
}

// buildRouterWithDeadline is the single router builder: production
// enters via buildRouter with the fixed budget; tests inject an
// explicit short duration. Same routes, same mechanism, no mutable
// package state, no second deadline owner.
func (s *Server) buildRouterWithDeadline(deadline time.Duration) http.Handler {
	r := chi.NewRouter()

	// Global baseline security headers — outermost so every response
	// (HTML, JSON, static, errors, recovered panics) inherits them.
	r.Use(securityHeadersMiddleware)
	// Recovery middleware (catches panics)
	r.Use(s.recoverMiddleware)
	r.Use(requestDeadlineMiddleware(deadline))

	// -- Static file routes --
	r.Get("/robots.txt", serveStaticFile("robots.txt", "text/plain; charset=utf-8", ""))
	r.Get("/sitemap.xml", serveStaticFile("sitemap.xml", "application/xml; charset=utf-8", ""))
	r.Get("/llms.txt", serveStaticFile("llms.txt", "text/plain; charset=utf-8", ""))
	r.Get("/openai.json", serveStaticFile("openai.json", "application/json; charset=utf-8", "public, max-age=300"))
	r.Get("/.well-known/openai.json", serveStaticFile("openai.json", "application/json; charset=utf-8", "public, max-age=300"))
	r.Get("/kungfu_skill.md", serveStaticFile("kungfu_skill.md", "text/markdown; charset=utf-8", ""))
	r.Get("/owner_task_guide.md", serveStaticFile("owner_task_guide.md", "text/markdown; charset=utf-8", ""))
	r.Get("/manifest.webmanifest", serveStaticFile("manifest.webmanifest", "application/manifest+json; charset=utf-8", "public, max-age=300"))
	r.Get("/sw.js", serveStaticFile("sw.js", "application/javascript; charset=utf-8", "no-cache, no-store, must-revalidate"))

	// Assets (/assets/*)
	r.Get("/assets/*", serveAssets())

	// Infrastructure probes (public, no auth). Liveness never
	// touches PG; readiness Pings PG via r.Context() under the
	// existing request deadline. /api/ping remains the Agent
	// identity/balance API and is not a probe alias.
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	// -- MCP surface (v1.3 M1): official SDK streamable HTTP handler,
	// stateless, protocol 2026-07-28 only. Routed under the existing
	// security-header / panic / deadline middleware — no second
	// timeout or lifecycle owner. Trusted client IP flows from the
	// existing proxy mechanism into MCP rate limiting.
	mcpDeps := mcpserver.Deps{
		Pool:        s.Pool,
		RateLimiter: s.RateLimiter,
		AgentLookup: func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
			return repository.FindActiveBotByAPIKeyHash(ctx, s.Pool, keyHash)
		},
		AccountStatus: service.ComposeAgentAccountStatus,
		ClientIP: func(r *http.Request) string {
			return middleware.GetClientIP(r, s.TrustedProxies)
		},
		Limits: mcpserver.ContentLimits{
			MaxTitleLength:       s.Config.MaxTitleLength,
			MaxTags:              s.Config.MaxTags,
			MaxTagLength:         s.Config.MaxTagLength,
			MaxDescriptionLength: s.Config.MaxDescriptionLength,
			MaxContentSize:       s.Config.MaxContentSize,
		},
	}
	r.Handle("/mcp", mcpserver.Handler(mcpDeps))

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
	r.Post("/api/owner/session", ownerMutation(s.handleOwnerSessionLogin))
	r.Delete("/api/owner/session", ownerMutation(s.handleOwnerSessionLogout))

	r.Get("/api/account", s.handleAccount)
	r.Get("/api/key", s.handleKey)
	r.Post("/api/change-password", ownerMutation(s.handleChangePassword))
	r.Post("/api/reset-key", ownerMutation(s.handleResetKey))

	r.Get("/api/owner/tasks", s.handleOwnerTasksList)
	r.Get("/api/owner/tasks/{code}", s.handleOwnerTaskGet)
	r.Post("/api/owner/tasks", ownerMutation(s.handleOwnerTaskCreate))
	r.Post("/api/owner/tasks/{code}/open", ownerMutation(s.handleOwnerTaskOpen))
	r.Post("/api/owner/tasks/{code}/close", ownerMutation(s.handleOwnerTaskClose))
	r.Post("/api/owner/tasks/{code}/add-budget", ownerMutation(s.handleOwnerTaskAddBudget))
	r.Post("/api/owner/tasks/{code}/refund", ownerMutation(s.handleOwnerTaskRefund))
	r.Post("/api/owner/tasks/{code}/edit", ownerMutation(s.handleOwnerTaskEdit))

	r.Post("/api/testtask/{code}", ownerMutation(s.handleTestTask))

	r.Get("/api/owner/logs", s.handleOwnerLogs)

	// Owner store entry points (session -> bot_id; the only subject)
	r.Get("/api/owner/payments/packages", s.handleOwnerPaymentPackages)
	r.Post("/api/owner/payments/checkout", ownerMutation(s.handleOwnerPaymentCheckout))
	r.Get("/api/owner/payments/{code}", s.handleOwnerPaymentGet)
	r.Post("/api/webhooks/creem", s.handleCreemWebhook)
	r.Get("/api/owner/store/products", s.handleOwnerStoreProducts)
	r.Post("/api/owner/store/redemptions", ownerMutation(s.handleOwnerStoreRedeem))
	r.Get("/api/owner/store/redemptions/{code}", s.handleOwnerStoreRedemptionGet)

	// -- API routes: Admin (kf_admin server-side session) --
	// B1.1 identity foundation + B1.2 management surface. Every
	// authenticated mutation requires X-CSRF-Token (enforced in
	// requireAdminMutation); the only exception remains the login
	// POST, which is rate-limited instead.
	r.Post("/api/admin/session", s.handleAdminSessionCreate)
	r.Get("/api/admin/session", s.handleAdminSessionGet)
	r.Delete("/api/admin/session", s.handleAdminSessionDelete)

	// B1.2: admin accounts
	r.Get("/api/admin/users", s.handleAdminUsersList)
	r.Post("/api/admin/users", s.handleAdminUsersCreate)
	r.Get("/api/admin/users/{id}", s.handleAdminUserGet)
	r.Patch("/api/admin/users/{id}", s.handleAdminUserPatch)
	r.Post("/api/admin/users/{id}/enable", s.handleAdminUserEnable)
	r.Post("/api/admin/users/{id}/disable", s.handleAdminUserDisable)
	r.Put("/api/admin/users/{id}/roles", s.handleAdminUserRoles)
	r.Put("/api/admin/users/{id}/password", s.handleAdminUserPassword)
	r.Post("/api/admin/users/{id}/force-logout", s.handleAdminUserForceLogout)
	r.Post("/api/admin/me/password", s.handleAdminMePassword)

	// B1.2: roles + permissions
	r.Get("/api/admin/roles", s.handleAdminRolesList)
	r.Post("/api/admin/roles", s.handleAdminRolesCreate)
	r.Get("/api/admin/roles/{id}", s.handleAdminRoleGet)
	r.Patch("/api/admin/roles/{id}", s.handleAdminRolePatch)
	r.Put("/api/admin/roles/{id}/permissions", s.handleAdminRolePermissions)
	r.Get("/api/admin/permissions", s.handleAdminPermissionsList)

	// B1.2: sessions + audit explorer
	r.Get("/api/admin/sessions", s.handleAdminSessionsList)
	r.Delete("/api/admin/sessions/{id}", s.handleAdminSessionRevoke)
	r.Get("/api/admin/audit", s.handleAdminAuditList)

	// B2: Store Administration (Admin control plane → store domain)
	r.Get("/api/admin/store/products", s.handleAdminStoreProductsList)
	r.Post("/api/admin/store/products", s.handleAdminStoreProductsCreate)
	r.Get("/api/admin/store/products/{code}", s.handleAdminStoreProductGet)
	r.Patch("/api/admin/store/products/{code}", s.handleAdminStoreProductPatch)
	r.Post("/api/admin/store/products/{code}/activate", s.handleAdminStoreProductActivate)
	r.Post("/api/admin/store/products/{code}/deactivate", s.handleAdminStoreProductDeactivate)
	r.Get("/api/admin/store/redemptions", s.handleAdminStoreRedemptionsList)
	r.Get("/api/admin/store/redemptions/{code}", s.handleAdminStoreRedemptionGet)
	r.Post("/api/admin/store/redemptions/{code}/approve", s.handleAdminStoreRedemptionApprove)
	r.Post("/api/admin/store/redemptions/{code}/reject", s.handleAdminStoreRedemptionReject)
	r.Post("/api/admin/store/redemptions/{code}/fulfill", s.handleAdminStoreRedemptionFulfill)
	r.Post("/api/admin/store/redemptions/{code}/cancel", s.handleAdminStoreRedemptionCancel)

	// B1.2: Admin Workspace HTML routes
	r.Get("/admin", s.handleAdminPage("dashboard"))
	r.Get("/admin/login", s.handleAdminPage("login"))
	r.Get("/admin/account", s.handleAdminPage("account"))
	r.Get("/admin/users", s.handleAdminPage("users"))
	r.Get("/admin/roles", s.handleAdminPage("roles"))
	r.Get("/admin/sessions", s.handleAdminPage("sessions"))
	r.Get("/admin/audit", s.handleAdminPage("audit"))
	r.Get("/admin/store/products", s.handleAdminPage("store_products"))
	r.Get("/admin/store/redemptions", s.handleAdminPage("store_redemptions"))

	// -- Web routes (HTML) --
	r.Get("/", s.agentHomeHandler())
	r.Get("/credits", s.handleCredits)
	r.Get("/owner", s.handleOwnerPage("overview"))
	r.Get("/owner/login", s.handleOwnerPage("login"))
	r.Get("/owner/register", s.handleOwnerPage("register"))
	r.Get("/owner/account", s.handleOwnerPage("account"))
	r.Get("/owner/key", s.handleOwnerPage("key"))
	r.Get("/owner/tasks", s.handleOwnerPage("tasks"))
	r.Get("/owner/credits", s.handleOwnerPage("owner_credits"))
	r.Get("/owner/tasks/new", s.handleOwnerPage("task_new"))
	r.Get("/owner/logs", s.handleOwnerPage("logs"))
	r.Get("/owner/store", s.handleOwnerPage("store"))
	r.Get("/owner/task-guide", s.handleTaskGuide)

	// 404 for everything else
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		NotFound(w)
	})

	return r
}

// recoverMiddleware is the ONE HTTP request panic boundary.
//
// Contract:
//   - handler panics never escape to the server loop
//   - panic before the response is committed -> canonical 500
//     INTERNAL_ERROR (existing public contract, no panic details)
//   - panic after the response is committed (explicit WriteHeader or
//     an implicit-commit first Write) -> the committed bytes stand:
//     no second WriteHeader, no appended error JSON, no rewriting
//   - http.ErrAbortHandler keeps its native net/http sentinel
//     semantics and is re-panicked, not converted to a 500
//   - every recovered panic leaves a server-side diagnostic
//     (method, path only — never query, headers, cookies, or body)
//
// The wrapper does NOT buffer the response; normal writes pass
// straight through. Production handlers have no Flusher/Hijacker/
// Pusher/ResponseController dependencies, so the basic
// WrapResponseWriter is sufficient.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// net/http's abort sentinel is a control signal, not an
			// application panic: preserve its native semantics.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			// Diagnostic: method + path (never RawQuery), panic type,
			// stack. No credentials, cookies, bodies, or secrets.
			log.Printf("panic recovered: method=%s path=%s panic_type=%T\n%s",
				r.Method, r.URL.Path, rec, debug.Stack())
			if ww.Status() != 0 || ww.BytesWritten() > 0 {
				// Response already committed: leave the bytes as they
				// are. Swallow the panic; do not append a second
				// error document.
				return
			}
			ErrorResponse(ww, 500, "INTERNAL_ERROR", "An internal error occurred", nil)
		}()
		next.ServeHTTP(ww, r)
	})
}

// -- Helper functions --

// parseJSONBodyRequired reads and parses JSON, returning error if invalid.
// requireObject: if true, body must be a JSON object (not just any valid JSON).
// emptyMessage: message for empty body case ("Request body must be valid JSON" vs "...JSON object")
func parseJSONBodyRequired(r *http.Request, requireObject bool, emptyMessage string) (map[string]interface{}, error) {
	// Cap body at 256KB to prevent memory exhaustion
	r.Body = http.MaxBytesReader(nil, r.Body, 262144)
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

// ============================================================
// R2.1: the SINGLE application request execution budget.
// ============================================================

// requestDeadlineBudgetDefault is the ONLY production request-budget
// authority: a fixed 25s, strictly below the frozen 30s WriteTimeout.
// No env/config override, no package mutable state.
const requestDeadlineBudgetDefault = 25 * time.Second

// requestDeadlineMiddleware is the ONLY request-budget owner: every
// inbound request gets the given deadline on its context before the
// handler runs. Handlers, domains, repositories, and providers all
// observe the same cancellation because they already receive
// r.Context(); no handler adds a second WithTimeout. 25s is strictly
// below the frozen http.Server WriteTimeout (30s), leaving ~5s for
// error writeback; per-call client safety nets (PostAPI 10s, Creem
// 15s) remain the lower-layer bounds. Production wires the fixed 25s
// via buildRouter; tests inject an explicit short duration through
// buildRouterWithDeadline — same mechanism, same routes.
func requestDeadlineMiddleware(budget time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), budget)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
