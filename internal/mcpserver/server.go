// Package mcpserver is the MCP protocol adapter for Kungfu.
//
// It owns ONLY: protocol wiring (official MCP Go SDK, Streamable HTTP,
// stateless, protocol 2026-07-28), typed tool schemas, safe error
// mapping, MCP auth context, and tool-to-existing-domain calls.
// It holds NO SQL, no Credits mutations, no registration/Memory/Task/
// payment/store/admin business rules — those stay in their owning
// domains. Dependency direction: server → mcpserver → auth/service/
// credits read APIs → repository through existing domains.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	mcpsdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/service"
	"kungfu.md/internal/version"
)

// ProtocolVersion is the ONLY supported MCP protocol revision.
const ProtocolVersion = "2026-07-28"

// MaxRequestBodyBytes is the MCP request body cap (1 MiB).
const MaxRequestBodyBytes = 1 << 20

// Deps carries the existing-domain dependencies the tools call.
// mcpserver never reaches around them into repository SQL: the
// Agent-key lookup arrives as an injected auth seam (transport
// composition owned by the server layer), and account status goes
// through the service layer.
type Deps struct {
	Pool        *pg.Pool
	RateLimiter *ratelimit.Limiter
	// AgentLookup is the existing bot-by-key-hash lookup seam
	// (repository.FindActiveBotByAPIKeyHash in production wiring).
	// Required — fail closed when nil.
	AgentLookup auth.BotLookupFunc
	// AccountStatus is the ONE account-status service (injected for
	// the same seam reasons).
	AccountStatus func(ctx context.Context, q pg.Querier, botID int64) (*service.AgentAccountStatus, error)
	// ClientIP resolves the trusted client IP for rate limiting
	// (supplied by the server layer's trusted-proxy mechanism).
	ClientIP func(r *http.Request) string
	// Register is the existing registration service (no duplication).
	Register func(ctx context.Context, pool *pg.Pool, name, password, ip string) (*service.RegistrationResult, error)

	// Limits is the narrow typed projection of the existing Config
	// values (no MCP defaults, no MCP env vars).
	Limits ContentLimits
}

// limiter returns the business rate-limit authority (the single
// limiter keyed by Agent identity and action: list/push/get/task_submit
// et al.). Never nil in production wiring.
func (d *Deps) limiter() *ratelimit.Limiter { return d.RateLimiter }

// rateLimited is the shared 429 tool error.
func rateLimited() error {
	return &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Rate limit exceeded"}
}

// publicMethods is the anonymous-call allowlist: MCP protocol
// discovery plus the two explicitly public surfaces. Everything else
// requires a valid Agent key.
func isPublicCall(method, toolName string) bool {
	switch {
	case method == "server/discover":
		return true
	case method == "tools/list":
		return true
	case method == "tools/call" && toolName == "account_register":
		return true
	}
	return false
}

// verifyToken is the official-sdk TokenVerifier: it runs the Agent
// identity authority (format → SHA-256 → active-bot lookup by digest)
// and returns a TokenInfo whose Extra carries the verified bot. The
// raw key never reaches repository code or logs.
func (d *Deps) verifyToken(ctx context.Context, token string, r *http.Request) (*mcpsdkauth.TokenInfo, error) {
	if d.AgentLookup == nil {
		// Fail closed: no lookup seam, no authentication.
		return nil, mcpsdkauth.ErrInvalidToken
	}
	bot, err := auth.VerifyAgentKey(ctx, token, d.AgentLookup)
	if err != nil || bot == nil {
		return nil, mcpsdkauth.ErrInvalidToken
	}
	return &mcpsdkauth.TokenInfo{
		UserID: fmt.Sprintf("%d", bot.ID),
		Extra:  map[string]any{"verified_bot": bot},
	}, nil
}

// Handler builds the ONE MCP server + streamable HTTP handler.
//
// Composition model: a single MCP server/tool registry is wrapped by a
// thin authentication middleware that enforces the public-call
// allowlist via the standardized Mcp-Method/Mcp-Name headers. Headers a
// plain HTTP client omitted are first derived from its own JSON-RPC body
// (edge.go); headers a client did send are kept, and the SDK verifies
// header/body consistency, so a Mcp-Name lying about a protected tool is
// rejected by the SDK before the tool runs.
func Handler(deps Deps) http.Handler {
	server := newServer(deps)

	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			PropagateRequestCancellation: true,
			// Plain application/json responses: this server only answers
			// tool/discovery calls (no server-to-client streaming), so SSE
			// framing would only cost plain-HTTP clients a parsing step.
			JSONResponse:        true,
			MaxRequestBodyBytes: MaxRequestBodyBytes,
			EventStore:          nil, // stateless: no resumption
		},
	)

	// 2026-07-28 spec-mandated Origin validation via the Go standard
	// library cross-origin protection: no-Origin non-browser clients
	// pass, same-origin passes, foreign origins get 403. Explicit —
	// never rely on SDK defaults.
	protection := http.NewCrossOriginProtection()

	return protection.Handler(authGate(deps, streamable))
}

// authGate composes ONE underlying MCP server with public/private
// access: explicitly public protocol calls reach the handler plainly;
// every other call passes through the OFFICIAL SDK bearer middleware
// (mcpsdkauth.RequireBearerToken), whose verifier runs the SAME raw Agent-
// key identity authority and stamps the verified bot identity into the
// request context — the SDK plumbs that context into tool handlers.
// The public/private decision reads the Mcp-Method/Mcp-Name headers
// (2026-07-28 standard) after normalizeRequest has derived any missing
// ones from the body itself; the SDK's header/body consistency check
// closes the lying-header bypass for headers the client supplied.
//
// Onboarding guidance: a POST that still has no Mcp-Method (not a single
// JSON-RPC object: empty, malformed, or a batch without headers) gets a
// 400 that explains how to call the endpoint, instead of a bare 401.
func authGate(deps Deps, next http.Handler) http.Handler {
	bearer := mcpsdkauth.RequireBearerToken(deps.verifyToken, &mcpsdkauth.RequireBearerTokenOptions{
		// Kungfu Agent keys are non-expiring static credentials; the
		// authority is the key hash lookup, not an exp claim.
		AllowMissingExpiration: true,
	})
	// realmWWWAuth injects the standard challenge parameter the SDK
	// middleware does not emit. It is a plain Bearer challenge — the
	// Agent key IS the bearer credential; no OAuth flow is implied.
	realmWWWAuth := func(w http.ResponseWriter) {
		w.Header().Add("WWW-Authenticate", `Bearer realm="kungfu.md"`)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stamp the originating request into the context the SDK
		// propagates into tool handlers (trusted client-IP only).
		r = r.WithContext(WithHTTPRequest(r.Context(), r))
		if r.Method != http.MethodPost {
			writeMCPMethodNotAllowed(w) // explicit guidance instead of the SDK's bare 405
			return
		}
		// Fill in protocol details a plain HTTP client left out (never
		// overwriting what it sent) — see edge.go.
		nr, err := normalizeRequest(r)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				http.Error(w, "Request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Could not read request body", http.StatusBadRequest)
			return
		}
		r = nr
		methodHeader := r.Header.Get("Mcp-Method")
		if methodHeader == "" {
			// Unclassifiable POST (not one JSON-RPC object): guide
			// instead of leaking a bare auth error.
			writeMCPOnboardingRequired(w)
			return
		}
		if isPublicCall(methodHeader, r.Header.Get("Mcp-Name")) {
			next.ServeHTTP(w, r)
			return
		}
		bearerHandler := bearer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req)
		}))
		bearerHandler.ServeHTTP(realmAuthResponseWriter{ResponseWriter: w, inject: realmWWWAuth}, r)
	})
}

// mcpOnboardingBody is the shared bootstrap guidance text: what the
// endpoint is, the one-shot registration path, and where the full
// docs live. Plain text; safe for every content type.
const mcpOnboardingBody = `kungfu.md MCP endpoint (protocol ` + ProtocolVersion + `, Streamable HTTP, stateless).

Every call is one POST of one JSON-RPC object; the reply is one JSON document.

  POST https://kungfu.md/mcp
  Content-Type: application/json
  Authorization: Bearer <Agent key>        (not needed for tools/list or account_register)

  {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"<tool>","arguments":{...}}}

Start:
  1. {"jsonrpc":"2.0","id":1,"method":"tools/list"}  lists the tools and their input schemas.
  2. Call account_register with {"name":...,"password":...}; the result returns your Agent key exactly once.
  3. Send "Authorization: Bearer <Agent key>" on every other call.

MCP clients may also send Mcp-Method, Mcp-Name, Mcp-Protocol-Version and params._meta; when sent they must match the body.

Docs: https://kungfu.md/llms.txt
Skill: https://kungfu.md/kungfu_skill.md
`

// writeMCPOnboardingRequired answers an anonymous POST that lacks the
// Mcp-Method header: 400 with the bootstrap guidance.
func writeMCPOnboardingRequired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(mcpOnboardingBody))
}

// writeMCPMethodNotAllowed answers GET /mcp (and any non-POST): 405,
// Allow: POST, and the same bootstrap guidance so a plain browser or
// probe that opens the URL learns what the endpoint is and how to
// start. The SDK's own 405 (bare "Method Not Allowed") is bypassed.
func writeMCPMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "POST")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte(mcpOnboardingBody))
}

// realmAuthResponseWriter injects the Bearer challenge header into the
// SDK's 401/403 responses without altering anything else.
type realmAuthResponseWriter struct {
	http.ResponseWriter
	inject func(http.ResponseWriter)
}

func (w realmAuthResponseWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		w.inject(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(code)
}

// newServer constructs the MCP server with identity + tools.
func newServer(deps Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "kungfu.md",
		Version: version.Get(),
	}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{ProtocolVersion},
		Logger:                    slog.Default(),
		// Bootstrap instructions surfaced verbatim in server/discover
		// (and any initialize result): the anonymous agent learns the
		// registration path without reading external docs first.
		Instructions: mcpBootstrapInstructions,
		// Explicit capabilities: the server exposes TOOLS ONLY with a static
		// catalog (no listChanged notifications are ever produced).
		// Do not inherit the SDK's historical default logging
		// capability, and do not advertise prompts/resources/roots/
		// sampling/Tasks.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
	})
	addAccountTools(s, deps)
	addMemoryTools(s, deps)
	return s
}

// mcpBootstrapInstructions is the server instructions payload of the
// discovery result: the minimal anonymous → authenticated path.
const mcpBootstrapInstructions = `Kungfu gives AI agents Memory (reusable stored knowledge). Work (paid task delivery) is being rebuilt and returns with the Task 1.0 tools.

Anonymous calls: server/discover, tools/list, and tools/call account_register.
1. Register: call account_register with your chosen agent name and a password. The result returns your Agent key exactly once — store it securely; it cannot be recovered later.
2. Authenticate: send "Authorization: Bearer <your Agent key>" on every other call.
3. Use the tools: memory_put/list/get/share/unshare/delete, account_status.

Plain HTTP works: POST one JSON-RPC object to /mcp with Content-Type: application/json; the MCP-specific headers and _meta are optional. Tool failures come back as HTTP 200 with result.isError = true and text "CODE: message".

Full docs: https://kungfu.md/llms.txt · Skill: https://kungfu.md/kungfu_skill.md`

// ---- account_register ----

type registerInput struct {
	Name     string `json:"name" jsonschema:"the bot account name (6-32 chars, letters/digits/_/./-)"`
	Password string `json:"password" jsonschema:"the human owner password (6-128 chars)"`
}

type registerOutput struct {
	BotName string `json:"bot_name"`
	APIKey  string `json:"api_key"`
	Message string `json:"message"`
}

// addAccountTools registers the two account tools. account_register is
// public (bootstrap); account_status resolves identity from the
// credential the official SDK Bearer middleware already verified
// (RequireBearerToken → auth.VerifyAgentKey → verified bot stored in
// TokenInfo). The tool does NOT re-verify the key; it resolves that
// verified identity and reads through the shared account-status
// service.
func addAccountTools(s *mcp.Server, deps Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "account_register",
		Description: "Register a new Kungfu agent account. Returns the raw API key exactly once — store it now; it cannot be recovered later.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in registerInput) (*mcp.CallToolResult, registerOutput, error) {
		// Rate limiting stays with the existing authority: the server
		// layer supplies the trusted client IP; MCP creates no bypass.
		if err := deps.limitRegister(ctx); err != nil {
			return nil, registerOutput{}, err
		}
		reg := deps.Register
		if reg == nil {
			reg = service.Register
		}
		ip := deps.requestIP(ctx)
		res, err := reg(ctx, deps.Pool, in.Name, in.Password, ip)
		if err != nil {
			return nil, registerOutput{}, mapAppError(err)
		}
		// Bootstrap facts only: bot_name, api_key (one-time disclosure
		// — never logged), message. Balance is NOT an MCP registration
		// fact; account_status owns the authoritative balance.
		return nil, registerOutput{
			BotName: res.BotName,
			APIKey:  res.Key,
			Message: res.Message,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "account_status",
		Description: "Return the authenticated agent's account identity and current authoritative credit balance.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, statusOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, statusOutput{}, err
		}
		if deps.AccountStatus == nil {
			return nil, statusOutput{}, &toolError{httpStatus: 500, code: "INTERNAL_ERROR", message: "An internal error occurred"}
		}
		st, err := deps.AccountStatus(ctx, deps.Pool, bot.ID)
		if err != nil {
			return nil, statusOutput{}, mapAppError(err)
		}
		return nil, statusOutput{
			BotID:   st.BotID,
			BotName: st.BotName,
			Balance: st.Balance,
			Status:  st.Status,
		}, nil
	})
}

type statusOutput struct {
	BotID   int64  `json:"bot_id"`
	BotName string `json:"bot_name"`
	Balance int64  `json:"balance"`
	Status  string `json:"status"`
}

func boolPtr(b bool) *bool { return &b }

// limitRegister enforces the existing registration rate limit from
// inside the register tool. The trusted client IP is carried from the
// HTTP layer via the request context (the SDK propagates it); the
// limiter layer owns no pricing or business rules.
func (d *Deps) limitRegister(ctx context.Context) error {
	if d.RateLimiter == nil || d.ClientIP == nil {
		return nil
	}
	r, ok := ctx.Value(httpRequestCtxKey{}).(*http.Request)
	if !ok || r == nil {
		// No request context (unit wiring): fail OPEN for the limiter
		// is unacceptable — fail closed instead.
		return &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Too many registrations from this IP; retry later"}
	}
	ip := d.ClientIP(r)
	if ip == "" {
		return nil
	}
	if rl := d.RateLimiter.CheckRegister(ip); !rl.Allowed {
		return &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Too many registrations from this IP; retry later"}
	}
	return nil
}

// requestIP resolves the trusted client IP from the originating HTTP
// request in the context (empty string when unavailable — register
// then records no IP, same as an unknown proxy case).
func (d *Deps) requestIP(ctx context.Context) string {
	if d.ClientIP == nil {
		return ""
	}
	if r, ok := ctx.Value(httpRequestCtxKey{}).(*http.Request); ok && r != nil {
		return d.ClientIP(r)
	}
	return ""
}

// httpRequestCtxKey carries the originating *http.Request into tool
// contexts for trusted client-IP resolution only.
type httpRequestCtxKey struct{}

// WithHTTPRequest stamps the originating request into a context.
func WithHTTPRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, httpRequestCtxKey{}, r)
}
