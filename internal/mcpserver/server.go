// Package mcpserver is the MCP protocol adapter for Kungfu.
//
// It owns ONLY: protocol wiring (official MCP Go SDK, Streamable HTTP,
// stateless, protocol 2026-07-28), typed tool schemas, safe error
// mapping, MCP auth context, and tool-to-existing-domain calls.
// It holds NO SQL, no Credits mutations, no registration/Memory/Task/
// payment/rewards/admin business rules — those stay in their owning
// domains. Dependency direction: server → mcpserver → auth/service/
// credits read APIs → repository through existing domains.
package mcpserver

import (
	"context"
	"encoding/json"
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

	// AgentRefKey derives the per-task anonymous agent_ref (§7.1).
	// Injected from the same source as the recovery worker's key
	// (cfg.SessionSecret in production wiring).
	AgentRefKey []byte
}

// limiter returns the business rate-limit authority (the single
// limiter keyed by Agent identity and action: list/push/get/task_submit
// et al.). Never nil in production wiring.
func (d *Deps) limiter() *ratelimit.Limiter { return d.RateLimiter }

// rateLimited is the shared 429 tool error.
func rateLimited() error {
	return &ToolError{Code: "RATE_LIMIT", Message: "Rate limit exceeded"}
}

// isPublicCall is the anonymous-call allowlist: MCP protocol
// discovery, plus any registry tool whose ToolDef.Public is set (the
// single source — no duplicated name list). Everything else requires
// a valid Agent key.
func isPublicCall(method, toolName string) bool {
	switch {
	case method == "server/discover":
		return true
	case method == "tools/list":
		return true
	case method == "tools/call":
		def, ok := Tool(toolName)
		return ok && def.Public
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
	addRegistryTools(s, deps)
	return s
}

// addRegistryTools mounts the Task 1.0 registry (WO-7a) on the MCP
// server: one low-level AddTool per ToolDef with the explicit JSON
// schema; the handler builds the §8.2 envelope as structuredContent
// and mirrors not-accepted calls with isError = true.
func addRegistryTools(s *mcp.Server, deps Deps) {
	for _, def := range tools {
		s.AddTool(&mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: json.RawMessage(def.InputSchema),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			agent, err := deps.resolveVerified(ctx)
			if err != nil && !def.Public {
				// unreachable behind the bearer middleware; fail closed
				env := notAcceptedEnvelope("UNAUTHORIZED", "Agent key is invalid or missing", nil)
				return envelopeResult(env), nil
			}
			env, _ := CallTool(ctx, &deps, def.Name, agent, json.RawMessage(req.Params.Arguments))
			return envelopeResult(env), nil
		})
	}
}

// envelopeResult renders the §8.2 envelope as both the structured
// content and the JSON text content; not-accepted calls carry
// isError = true (§8.2: the SAME structure on both channels).
func envelopeResult(env map[string]any) *mcp.CallToolResult {
	raw, err := json.Marshal(env)
	if err != nil {
		raw = []byte(`{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"An internal error occurred"}}`)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(raw)}},
		StructuredContent: env,
		IsError:           env["ok"] == false,
	}
}

// mcpBootstrapInstructions is the server instructions payload of the
// discovery result: auth, the §8.2 result contract, and where the
// full documentation lives (WO-9a; content only from the spec).
const mcpBootstrapInstructions = `Kungfu is a harness for agents: publishers define tasks (contract, execution material, acceptance rules); executors do the work and submit results; credits settle on acceptance.

Authentication: one Agent key. Register anonymously with the account_register tool (choose name + password; the key is returned exactly once — store it, it cannot be recovered). Send "Authorization: Bearer <your Agent key>" on every other call. The same key works on MCP /mcp and on plain HTTP POST /api/v1/<tool>.

Every tool returns ONE JSON object. Four keys decide your next step: ok (accepted or not), next_action (submit, poll, done, revise, retry, wait, stop or null), retry_after (seconds, when applicable) and error (code + message, only when not accepted). Act strictly by next_action; do not resubmit while it says poll.

Docs: https://kungfu.md/llms.txt (interfaces, tools, error catalogue) - https://kungfu.md/kungfu_skill.md (agent procedure) - https://kungfu.md/task-guide.md (publisher guide)`

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
		return &ToolError{Code: "RATE_LIMIT", Message: "Too many registrations from this IP; retry later"}
	}
	ip := d.ClientIP(r)
	if ip == "" {
		return nil
	}
	if rl := d.RateLimiter.CheckRegister(ip); !rl.Allowed {
		return &ToolError{Code: "RATE_LIMIT", Message: "Too many registrations from this IP; retry later"}
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

// WithHTTPRequest is the Deps-flavored alias for transport wrappers.
func (d *Deps) WithHTTPRequest(ctx context.Context, r *http.Request) context.Context {
	return WithHTTPRequest(ctx, r)
}
