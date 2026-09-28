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

	"kungfu.md/internal/auth"
	"kungfu.md/internal/config"
	"kungfu.md/internal/mcpserver"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/payment"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
	"kungfu.md/internal/service"

	"bytes"
)

// Server holds all dependencies.
type Server struct {
	Config         *config.Config
	Pool           *pg.Pool
	RateLimiter    *ratelimit.Limiter
	mcpDepsValue   mcpserver.Deps
	mcpAgentLookup auth.BotLookupFunc
	Router         http.Handler
	TrustedProxies []*net.IPNet

	// secretBox seals/opens operator secrets stored in the database
	// (SETTINGS_ENC_KEY). nil when the key is not configured.
	secretBox *security.SecretBox

	// creemCache holds the last loaded payment settings snapshot.
	creemCache creemSettingsCache

	// creemBaseOverride redirects the Creem client at a test fake, and
	// creemSettingsOverride replaces the database settings. Both are
	// test-only: nil/empty in production, never settable from requests
	// or env.
	creemBaseOverride     string
	creemSettingsOverride *payment.CreemSettings
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
	if len(cfg.SettingsEncKey) > 0 {
		box, err := security.NewSecretBox(cfg.SettingsEncKey)
		if err != nil {
			log.Fatalf("SETTINGS_ENC_KEY: %v", err) // Load already validated the length
		}
		s.secretBox = box
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
	r.Get("/task-guide.md", serveStaticFile("task-guide.md", "text/markdown; charset=utf-8", ""))
	r.Get("/manifest.webmanifest", serveStaticFile("manifest.webmanifest", "application/manifest+json; charset=utf-8", "public, max-age=300"))
	r.Get("/sw.js", serveStaticFile("sw.js", "application/javascript; charset=utf-8", "no-cache, no-store, must-revalidate"))

	// Assets (/assets/*)
	r.Get("/assets/*", serveAssets())

	// Infrastructure probes (public, no auth). Liveness never
	// touches PG; readiness Pings PG via r.Context() under the
	// existing request deadline.
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	// -- MCP surface: official SDK streamable HTTP handler,
	// stateless, protocol 2026-07-28 only. Routed under the existing
	// security-header / panic / deadline middleware — no second
	// timeout or lifecycle owner. Trusted client IP flows from the
	// existing proxy mechanism into MCP rate limiting.
	s.mcpAgentLookup = func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		return repository.FindActiveBotByAPIKeyHash(ctx, s.Pool, keyHash)
	}
	mcpDeps := mcpserver.Deps{
		Pool:          s.Pool,
		RateLimiter:   s.RateLimiter,
		AgentLookup:   s.mcpAgentLookup,
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
		// Same agent_ref key source as the recovery worker (§7.1).
		AgentRefKey: []byte(s.Config.SessionSecret),
	}
	s.mcpDepsValue = mcpDeps
	r.Handle("/mcp", mcpserver.Handler(mcpDeps))

	// -- Task 1.0 tools over plain HTTP JSON (§8): POST /api/v1/<tool>,
	// Bearer Agent key, the same registry and §8.2 envelope as /mcp.
	r.Post("/api/v1/{tool}", s.handleAPIV1Tool)

	// -- API routes: Owner (session auth) --
	r.Get("/api/owner/session", s.handleOwnerSessionGet)
	r.Post("/api/owner/session", ownerMutation(s.handleOwnerSessionLogin))
	r.Post("/api/owner/register", ownerMutation(s.handleOwnerRegister))
	r.Delete("/api/owner/session", ownerMutation(s.handleOwnerSessionLogout))

	r.Get("/api/account", s.handleAccount)
	r.Get("/api/key", s.handleKey)
	r.Post("/api/change-password", ownerMutation(s.handleChangePassword))
	r.Post("/api/reset-key", ownerMutation(s.handleResetKey))

	r.Get("/api/owner/logs", s.handleOwnerLogs)

	// Owner rewards entry points (session -> bot_id; the only subject)
	r.Get("/api/owner/payments/packages", s.handleOwnerPaymentPackages)
	r.Post("/api/owner/payments/checkout", ownerMutation(s.handleOwnerPaymentCheckout))
	r.Post("/api/owner/tool/{tool}", ownerMutation(s.handleOwnerTool))
	r.Get("/api/owner/payments/{code}", s.handleOwnerPaymentGet)
	r.Post("/api/webhooks/creem", s.handleCreemWebhook)
	r.Get("/api/owner/rewards/products", s.handleOwnerRewardsProducts)
	r.Post("/api/owner/rewards/redemptions", ownerMutation(s.handleOwnerRewardsRedeem))
	r.Get("/api/owner/rewards/redemptions/{code}", s.handleOwnerRewardsRedemptionGet)

	// -- API routes: Admin (kf_admin server-side session) --
	// Identity foundation + management surface. Every
	// authenticated mutation requires X-CSRF-Token (enforced in
	// requireAdminMutation); the only exception remains the login
	// POST, which is rate-limited instead.
	r.Post("/api/samelabs/session", s.handleAdminSessionCreate)
	r.Get("/api/samelabs/session", s.handleAdminSessionGet)
	r.Delete("/api/samelabs/session", s.handleAdminSessionDelete)

	// Admin accounts
	r.Get("/api/samelabs/users", s.handleAdminUsersList)
	r.Post("/api/samelabs/users", s.handleAdminUsersCreate)
	r.Get("/api/samelabs/users/{id}", s.handleAdminUserGet)
	r.Patch("/api/samelabs/users/{id}", s.handleAdminUserPatch)
	r.Post("/api/samelabs/users/{id}/enable", s.handleAdminUserEnable)
	r.Post("/api/samelabs/users/{id}/disable", s.handleAdminUserDisable)
	r.Put("/api/samelabs/users/{id}/roles", s.handleAdminUserRoles)
	r.Put("/api/samelabs/users/{id}/password", s.handleAdminUserPassword)
	r.Post("/api/samelabs/users/{id}/force-logout", s.handleAdminUserForceLogout)
	r.Post("/api/samelabs/me/password", s.handleAdminMePassword)

	// Roles + permissions
	r.Get("/api/samelabs/roles", s.handleAdminRolesList)
	r.Post("/api/samelabs/roles", s.handleAdminRolesCreate)
	r.Get("/api/samelabs/roles/{id}", s.handleAdminRoleGet)
	r.Patch("/api/samelabs/roles/{id}", s.handleAdminRolePatch)
	r.Put("/api/samelabs/roles/{id}/permissions", s.handleAdminRolePermissions)
	r.Get("/api/samelabs/permissions", s.handleAdminPermissionsList)

	// Sessions + audit explorer
	r.Get("/api/samelabs/sessions", s.handleAdminSessionsList)
	r.Delete("/api/samelabs/sessions/{id}", s.handleAdminSessionRevoke)
	r.Get("/api/samelabs/audit", s.handleAdminAuditList)

	// Rewards Administration (Admin control plane → rewards domain)
	r.Get("/api/samelabs/rewards/products", s.handleAdminRewardsProductsList)
	r.Post("/api/samelabs/rewards/products", s.handleAdminRewardsProductsCreate)
	r.Get("/api/samelabs/rewards/products/{code}", s.handleAdminRewardsProductGet)
	r.Patch("/api/samelabs/rewards/products/{code}", s.handleAdminRewardsProductPatch)
	r.Post("/api/samelabs/rewards/products/{code}/activate", s.handleAdminRewardsProductActivate)
	r.Post("/api/samelabs/rewards/products/{code}/deactivate", s.handleAdminRewardsProductDeactivate)
	r.Get("/api/samelabs/rewards/redemptions", s.handleAdminRewardsRedemptionsList)
	r.Get("/api/samelabs/rewards/redemptions/{code}", s.handleAdminRewardsRedemptionGet)
	r.Post("/api/samelabs/rewards/redemptions/{code}/approve", s.handleAdminRewardsRedemptionApprove)
	r.Post("/api/samelabs/rewards/redemptions/{code}/reject", s.handleAdminRewardsRedemptionReject)
	r.Post("/api/samelabs/rewards/redemptions/{code}/fulfill", s.handleAdminRewardsRedemptionFulfill)
	r.Post("/api/samelabs/rewards/redemptions/{code}/cancel", s.handleAdminRewardsRedemptionCancel)

	// 013: payment provider settings (Creem), secrets sealed at rest
	r.Get("/api/samelabs/settings/payment", s.handleAdminPaymentSettingsGet)
	r.Put("/api/samelabs/settings/payment", s.handleAdminPaymentSettingsPut)

	// 011: Platform Account Administration (Admin control plane →
	// tb_bots accounts; NOT admin accounts, NOT finance authority)
	r.Get("/api/samelabs/accounts", s.handleAdminAccountsList)
	r.Get("/api/samelabs/accounts/{id}", s.handleAdminAccountGet)
	r.Post("/api/samelabs/accounts/{id}/disable", s.handleAdminAccountDisable)
	r.Post("/api/samelabs/accounts/{id}/enable", s.handleAdminAccountEnable)

	// 012: Finance Admin — READ-ONLY control plane (no POST/PATCH/
	// DELETE finance routes exist anywhere).
	r.Get("/api/samelabs/finance/summary", s.handleAdminFinanceSummary)
	r.Get("/api/samelabs/finance/payments", s.handleAdminFinancePayments)
	r.Get("/api/samelabs/finance/payments/{code}", s.handleAdminFinancePaymentDetail)
	r.Get("/api/samelabs/finance/adjustments", s.handleAdminFinanceAdjustments)
	r.Get("/api/samelabs/finance/ledger", s.handleAdminFinanceLedger)

	// WO-8b: task governance + report queue (reads tasks.read;
	// mutations tasks.manage / reports.manage via requireAdminMutation)
	r.Get("/api/samelabs/tasks", s.handleAdminTasksList)
	r.Get("/api/samelabs/tasks/{code}", s.handleAdminTaskGet)
	r.Post("/api/samelabs/tasks/{code}/close", s.handleAdminTaskClose)
	r.Get("/api/samelabs/reports", s.handleAdminReportsList)
	r.Post("/api/samelabs/reports/{id}/dismiss", func(w http.ResponseWriter, req *http.Request) {
		s.handleAdminReportResolve(w, req, "dismiss")
	})
	r.Post("/api/samelabs/reports/{id}/close", func(w http.ResponseWriter, req *http.Request) {
		s.handleAdminReportResolve(w, req, "close")
	})

	// Admin Workspace HTML routes
	// Platform admin pages (server-rendered)
	s.registerSamelabs(r)

	// -- Web routes (HTML) --
	r.Get("/", s.agentHomeHandler())
	r.Get("/credits", s.handleCredits)
	r.Get("/owner", s.handleOwnerPage("overview"))
	r.Get("/owner/login", s.handleOwnerPage("login"))
	r.Get("/owner/register", s.handleOwnerPage("register"))
	r.Get("/owner/account", s.handleOwnerPage("account"))
	r.Get("/owner/key", s.handleOwnerPage("key"))
	r.Get("/owner/credits", s.handleOwnerPage("owner_credits"))
	r.Get("/owner/tasks", s.handleOwnerPage("tasks"))
	r.Get("/owner/tasks/new", s.handleOwnerPage("task_new"))
	r.Get("/owner/tasks/{code}", s.handleOwnerPage("task_detail"))
	r.Get("/owner/logs", s.handleOwnerPage("logs"))
	r.Get("/owner/rewards", s.handleOwnerPage("rewards"))
	r.Get("/terms", s.handleLegalPage("terms"))
	r.Get("/privacy", s.handleLegalPage("privacy"))

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

// parseJSONBodyRequiredNumbers is parseJSONBodyRequired with
// json.Decoder.UseNumber(): every JSON number decodes into json.Number
// (the exact source text) instead of float64. Credit-bearing public
// boundaries MUST use this variant — float64 silently corrupts integer
// values above 2^53 (9007199254740993 would become 9007199254740992
// before validation could see it). All other field semantics identical.
func parseJSONBodyRequiredNumbers(r *http.Request, requireObject bool, emptyMessage string) (map[string]interface{}, error) {
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
		return map[string]interface{}{}, nil
	}

	var data map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, &parseError{msg: emptyMessage}
	}

	// Strict single-document semantics: after the object, only trailing
	// whitespace may remain. A second JSON value or any trailing
	// non-whitespace content is rejected (Decode skips whitespace and
	// returns io.EOF exactly when nothing but whitespace remains).
	if err := dec.Decode(&struct{}{}); err != io.EOF {
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
// The SINGLE application request execution budget.
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
