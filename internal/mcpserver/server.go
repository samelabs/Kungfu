// Package mcpserver is the MCP protocol adapter for Kungfu (v1.3 M1).
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

// ProtocolVersion is the ONLY supported MCP protocol revision (M1).
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

	// MemoryWork carries the M2 memory/work service seams (nil in M1
	// tests that predate M2; when nil, only the M1 account tools are
	// registered).
	MemoryWork *MemoryWorkDeps
}

// publicMethods is the anonymous-call allowlist: MCP protocol
// discovery plus the two explicitly public surfaces. Everything else
// requires a valid Agent API key.
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

// verifyToken is the official-sdk TokenVerifier: it runs the SAME
// raw Agent-key authority as REST (format → SHA-256 → active-bot
// lookup by digest) and returns a TokenInfo whose Extra carries the
// verified bot. The raw key never reaches repository code or logs.
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
// allowlist via the standardized Mcp-Method/Mcp-Name headers (never by
// parsing the JSON body) — the SDK itself verifies header/body
// consistency, so a Mcp-Name lying about a protected tool is rejected
// by the SDK before the tool runs.
func Handler(deps Deps) http.Handler {
	server := newServer(deps)

	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			PropagateRequestCancellation: true,
			MaxRequestBodyBytes:          MaxRequestBodyBytes,
			EventStore:                   nil, // stateless: no resumption
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
// key authority as REST and stamps the verified bot identity into the
// request context — the SDK plumbs that context into tool handlers.
// The public/private decision uses ONLY the Mcp-Method/Mcp-Name
// headers (2026-07-28 standard), never body parsing; the SDK's own
// header/body consistency check closes the lying-header bypass.
func authGate(deps Deps, next http.Handler) http.Handler {
	bearer := mcpsdkauth.RequireBearerToken(deps.verifyToken, &mcpsdkauth.RequireBearerTokenOptions{
		// Kungfu Agent keys are non-expiring static credentials; the
		// authority is the key hash lookup, not an exp claim.
		AllowMissingExpiration: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stamp the originating request into the context the SDK
		// propagates into tool handlers (trusted client-IP only).
		r = r.WithContext(WithHTTPRequest(r.Context(), r))
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r) // SDK answers GET with 405 (stateless)
			return
		}
		if isPublicCall(r.Header.Get("Mcp-Method"), r.Header.Get("Mcp-Name")) {
			next.ServeHTTP(w, r)
			return
		}
		bearer(next).ServeHTTP(w, r)
	})
}

// newServer constructs the MCP server with M1 identity + tools.
func newServer(deps Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "kungfu.md",
		Version: version.Get(),
	}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{ProtocolVersion},
		Logger:                    slog.Default(),
		// Explicit capabilities: M1 exposes TOOLS ONLY with a static
		// catalog (no listChanged notifications are ever produced).
		// Do not inherit the SDK's historical default logging
		// capability, and do not advertise prompts/resources/roots/
		// sampling/Tasks.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
	})
	addAccountTools(s, deps)
	if deps.MemoryWork != nil {
		addMemoryTools(s, deps, *deps.MemoryWork)
		addWorkTools(s, deps, *deps.MemoryWork)
	}
	return s
}

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

// addAccountTools registers the two M1 tools. account_register is
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
	BotID   int64   `json:"bot_id"`
	BotName string  `json:"bot_name"`
	Balance float64 `json:"balance"`
	Status  string  `json:"status"`
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
