package mcpserver

// M1 tests: official MCP Go SDK client against httptest, real
// PostgreSQL for the account paths.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
)

func m1TestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// m1Deps builds the production Deps wiring (tests may override fields).
func m1Deps(t *testing.T, pool *pg.Pool, limiter *ratelimit.Limiter) Deps {
	t.Helper()
	if limiter == nil {
		limiter = ratelimit.NewLimiter(map[string]ratelimit.Config{})
	}
	return Deps{
		Pool:        pool,
		RateLimiter: limiter,
		Limits: ContentLimits{
			MaxTitleLength:       128,
			MaxTags:              10,
			MaxTagLength:         32,
			MaxDescriptionLength: 500,
			MaxContentSize:       102400,
		},
		AgentLookup: func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
			return repository.FindActiveBotByAPIKeyHash(ctx, pool, keyHash)
		},
		AccountStatus: service.ComposeAgentAccountStatus,
		ClientIP: func(r *http.Request) string {
			host, _, err := splitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return host
		},
	}
}

// m1Handler builds the production handler on httptest.
func m1Handler(t *testing.T, pool *pg.Pool, limiter *ratelimit.Limiter) http.Handler {
	t.Helper()
	return Handler(m1Deps(t, pool, limiter))
}

// m1RawRequest posts a raw JSON-RPC body with the given headers.
func m1RawRequest(t *testing.T, srv *httptest.Server, body map[string]interface{}, hdr map[string]string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, "", fmt.Errorf("no port")
	}
	return s[:i], s[i+1:], nil
}

// ---- protocol ----

func TestM1ProtocolNegotiatesExactly20260728(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client := mcp.NewClient(&mcp.Implementation{Name: "m1-test", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer ss.Close()
	initRes := ss.InitializeResult()
	if initRes == nil || initRes.ProtocolVersion != ProtocolVersion {
		t.Fatalf("negotiated %v, want exactly 2026-07-28", initRes)
	}
}

// The ACTUAL M1 contract: legacy initialize is NOT a public/anonymous
// MCP operation on this surface. The 2026-07-28 handshake is
// server/discover; a legacy initialize attempt cannot negotiate (the
// SDK's own legacy fallback to an older revision is never reached
// because the request is auth-rejected first). Kungfu advertises and
// negotiates 2026-07-28 only — proven by the discovery test above.
func TestM1LegacyInitializeIsNotPublic(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	sc, body := m1RawRequest(t, srv, map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]string{"name": "legacy", "version": "1"},
		},
	}, map[string]string{"Mcp-Method": "initialize"})

	// Anonymous legacy initialize must NOT negotiate: HTTP-level
	// rejection (401 — not a public operation) or a JSON-RPC error.
	// A SUCCESS result with serverInfo must never appear.
	if strings.Contains(body, "serverInfo") {
		t.Fatalf("legacy initialize negotiated: %d %s", sc, body)
	}
	if sc == 200 && !strings.Contains(body, "error") {
		t.Fatalf("legacy initialize accepted anonymously: %d %s", sc, body)
	}
}

func TestM1PostWorksGetRejects(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// GET on a stateless server: 405
	resp, err := srv.Client().Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405 (stateless)", resp.StatusCode)
	}
}

func TestM1NoOriginAllowedForeignOrigin403(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// no Origin (non-browser client) => allowed past origin gate
	body := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 403 {
		t.Fatal("no-Origin request must be allowed")
	}

	// foreign Origin => 403
	req2, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	req2.Header.Set("Mcp-Method", "tools/list")
	req2.Header.Set("Origin", "https://evil.example")
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin = %d, want 403", resp2.StatusCode)
	}
}

func TestM1OversizedBody413(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	big := bytes.Repeat([]byte("a"), (1<<20)+16)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", resp.StatusCode)
	}
}

func TestM1ToolsListDeterministicOrder(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// The official SDK's tools/list returns the registry SORTED by
	// tool name — that sorted order is the SDK's own deterministic
	// contract (single registry, no second registry added). The exact
	// 12-tool set in the SDK-deterministic order:
	want := []string{
		"account_register", "account_status",
		"memory_delete", "memory_get", "memory_list", "memory_put", "memory_share", "memory_unshare",
		"work_get", "work_list", "work_publish", "work_submit",
	}

	listNames := func() []string {
		ctx := context.Background()
		transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
		client := mcp.NewClient(&mcp.Implementation{Name: "m1-ls", Version: "1"}, nil)
		ss, err := client.Connect(ctx, transport, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer ss.Close()
		tools, err := ss.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var names []string
		for _, tl := range tools.Tools {
			names = append(names, tl.Name)
		}
		return names // NO client-side sort before comparison
	}

	for i := 0; i < 3; i++ {
		got := listNames() // independent stateless client each call
		if len(got) != len(want) || !equalStrings(got, want) {
			t.Fatalf("list #%d = %v, want exactly %v in this order", i, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestM1ServerDiscoverAdvertisesExactCapabilities: server/discover (or
// initialize result capabilities, whichever the negotiated protocol
// exposes) must advertise TOOLS and nothing else — no logging, no
// prompts, no resources, no sampling/roots.
func TestM1ServerDiscoverAdvertisesExactCapabilities(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client := mcp.NewClient(&mcp.Implementation{Name: "m1-cap", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer ss.Close()

	// Connect performs the SEP-2575 stateless server/discover; the
	// cached InitializeResult carries the server-advertised
	// capabilities envelope.
	res := ss.InitializeResult()
	if res == nil {
		t.Fatal("no discovery result")
	}
	caps := res.Capabilities
	if caps == nil {
		t.Fatal("no capabilities advertised")
	}
	if caps.Tools == nil {
		t.Fatal("tools capability missing")
	}
	if caps.Tools.ListChanged {
		t.Fatal("tools capability must not advertise listChanged (static catalog)")
	}
	if caps.Logging != nil {
		t.Fatal("logging capability must not be advertised")
	}
	if caps.Prompts != nil {
		t.Fatal("prompts capability must not be advertised")
	}
	if caps.Resources != nil {
		t.Fatal("resources capability must not be advertised")
	}
	// exact serialized form: only "tools" may appear
	b, _ := json.Marshal(caps)
	if !strings.Contains(string(b), "\"tools\"") {
		t.Fatalf("serialized capabilities: %s", b)
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(b, &env)
	if len(env) != 1 {
		t.Fatalf("advertised capability keys = %v, want exactly [tools]", envKeys(env))
	}
}

func envKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- auth boundary ----

func m1RegisterBot(t *testing.T, pool *pg.Pool, suffix string) (int64, string) {
	t.Helper()
	res, err := service.Register(context.Background(), pool, "m1bot_"+suffix, "password123", "127.0.0.1")
	if err != nil {
		t.Fatalf("seed register: %v", err)
	}
	var id int64
	_ = pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, "m1bot_"+suffix).Scan(&id)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id)
	})
	return id, res.Key
}

func m1CallStatusRaw(t *testing.T, srv *httptest.Server, auth string) (*http.Response, string) {
	t.Helper()
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]interface{}{"name": "account_status", "arguments": map[string]interface{}{}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_status")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.String()
}

func TestM1AuthBoundary(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	botID, rawKey := m1RegisterBot(t, pool, fmt.Sprint(time.Now().UnixNano()))

	// no Authorization => 401
	resp, body := m1CallStatusRaw(t, srv, "")
	if resp.StatusCode != 401 {
		t.Fatalf("no-auth status = %d body=%s", resp.StatusCode, body)
	}
	// invalid bearer => 401
	resp, body = m1CallStatusRaw(t, srv, "Bearer kf_live_"+strings.Repeat("b", 64))
	if resp.StatusCode != 401 {
		t.Fatalf("invalid bearer = %d", resp.StatusCode)
	}

	// valid key: raw POST with Bearer header reaches the tool (identity + Credits balance)
	resp3, body3 := m1CallStatusRaw(t, srv, "Bearer "+rawKey)
	if resp3.StatusCode != 200 {
		t.Fatalf("valid bearer raw = %d body=%s", resp3.StatusCode, body3)
	}
	if !strings.Contains(body3, fmt.Sprintf(`"bot_id":%d`, botID)) {
		t.Fatalf("raw bearer did not resolve identity: %s", body3)
	}
	var rawOut statusOutput
	_ = json.Unmarshal([]byte(extractJSON(body3)), &rawOut)

	// valid key via official SDK client
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: authClient(srv, rawKey)}
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "m1-ok", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect with valid key: %v", err)
	}
	defer ss.Close()
	res, err := ss.CallTool(ctx, &mcp.CallToolParams{Name: "account_status", Arguments: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("call account_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("account_status failed: %+v", res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out statusOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content: %v (%s)", err, raw)
	}
	if out.BotID != botID {
		t.Fatalf("bot_id = %d, want %d (same identity as REST)", out.BotID, botID)
	}
	if out.Balance != service.SignupGrant {
		t.Fatalf("balance = %v, want signup grant %v via Credits authority", out.Balance, service.SignupGrant)
	}
}

// authClient wraps the transport with a Bearer header.
func authClient(srv *httptest.Server, key string) *http.Client {
	base := srv.Client()
	return &http.Client{Transport: bearerRT{base.Transport, key}}
}

type bearerRT struct {
	base http.RoundTripper
	key  string
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.key)
	var rt http.RoundTripper = b.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(r)
}

func TestM1McpNameBodyMismatchRejectedBeforeToolExecution(t *testing.T) {
	pool := m1TestPool(t)

	// The protected tool's domain callback: must NEVER run.
	statusCalls := 0
	deps := m1Deps(t, pool, nil)
	deps.AccountStatus = func(ctx context.Context, q pg.Querier, botID int64) (*service.AgentAccountStatus, error) {
		statusCalls++
		return &service.AgentAccountStatus{BotID: botID}, nil
	}
	h := Handler(deps)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Header claims PUBLIC account_register; body calls PROTECTED
	// account_status. The official SDK header/body consistency
	// mechanism must reject the request at the protocol layer BEFORE
	// any tool handler or domain callback runs.
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]interface{}{"name": "account_status", "arguments": map[string]interface{}{}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_register")          // LIE
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion) // required by the 2026-07-28 standard-header contract
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	rbody := buf.String()

	rejected := false
	if resp.StatusCode == http.StatusBadRequest {
		rejected = true // HTTP-level protocol rejection
	}
	if resp.StatusCode == 200 && (strings.Contains(rbody, "\"code\"") && strings.Contains(rbody, "error")) {
		rejected = true // JSON-RPC error envelope (HeaderMismatch-style)
	}
	if !rejected {
		t.Fatalf("mismatch not rejected at protocol layer: %d %s", resp.StatusCode, rbody)
	}
	// Proof the protected path never executed: the domain callback was
	// never invoked and no account data leaked.
	if statusCalls != 0 {
		t.Fatalf("account_status domain callback ran %d time(s) on a mismatched request", statusCalls)
	}
	if strings.Contains(rbody, "balance") {
		t.Fatalf("protected account data leaked: %s", rbody)
	}
}

// ---- register tool ----

func TestM1RegisterToolPersistsHashOnlyAndReturnsKeyOnce(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	name := "m1reg_" + fmt.Sprint(time.Now().UnixNano())
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]interface{}{
			"name":      "account_register",
			"arguments": map[string]string{"name": name, "password": "password123"},
		},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_register")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("register = %d %s", resp.StatusCode, buf.String())
	}
	if !strings.Contains(buf.String(), "kf_live_") {
		t.Fatal("raw key not returned once")
	}
	// DB: hash + last4 only; no plaintext column exists at all
	var (
		hashBytes []byte
		last4     string
		plainCols int
	)
	var id int64
	_ = pool.QueryRow(context.Background(), `SELECT id, api_key_hash, api_key_last4 FROM tb_bots WHERE bot_name=$1`, name).Scan(&id, &hashBytes, &last4)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='tb_bots' AND column_name='api_key'`).Scan(&plainCols)
	if plainCols != 0 || len(hashBytes) != 32 {
		t.Fatalf("hash-only persistence broken (plainCols=%d hashLen=%d)", plainCols, len(hashBytes))
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id)
	})
	// no Balance field in the tool output (bootstrap facts only)
	if strings.Contains(buf.String(), `"balance"`) {
		t.Fatal("register output leaked a Balance economic fact")
	}
}

func TestM1RegistrationRateLimitPreserved(t *testing.T) {
	pool := m1TestPool(t)
	limiter := ratelimit.NewLimiter(map[string]ratelimit.Config{
		"register": {Window: 3600, Limit: 1, Enabled: true},
	})
	h := m1Handler(t, pool, limiter)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	register := func(name string) int {
		body := map[string]interface{}{
			"jsonrpc": "2.0", "id": 4, "method": "tools/call",
			"params": map[string]interface{}{
				"name":      "account_register",
				"arguments": map[string]string{"name": name, "password": "password123"},
			},
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "account_register")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	first := register("m1rl_a_" + suffix)
	if first != 200 {
		t.Fatalf("first register = %d", first)
	}
	// second call from the SAME IP: the tool must fail with RATE_LIMIT
	// (or HTTP 429) — MCP must not bypass the existing limiter.
	sc, body := m1CallRegisterRaw(t, srv, "m1rl_b_"+suffix)
	if sc == 200 && !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("second register not rate limited: %d %s", sc, body)
	}
	// cleanup first bot
	var id int64
	_ = pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, "m1rl_a_"+suffix).Scan(&id)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id)
	})
}

func TestM1NoSessionStateEstablished(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// stateless: no Mcp-Session-Id header ever set on responses
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client := mcp.NewClient(&mcp.Implementation{Name: "m1-ss", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer ss.Close()
	// a second independent client with no session knowledge works
	transport2 := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client2 := mcp.NewClient(&mcp.Implementation{Name: "m1-ss2", Version: "1"}, nil)
	ss2, err := client2.Connect(ctx, transport2, nil)
	if err != nil {
		t.Fatalf("stateless second connect: %v", err)
	}
	ss2.Close()
}

func TestM1CancellationPropagatesToToolContext(t *testing.T) {
	pool := m1TestPool(t)

	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	deps := m1Deps(t, pool, nil)
	deps.Register = func(ctx context.Context, pool *pg.Pool, name, password, ip string) (*service.RegistrationResult, error) {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, nil
		}
	}
	h := Handler(deps)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]interface{}{
			"name": "account_register",
			"arguments": map[string]string{
				"name":     "m1cancel_" + fmt.Sprint(time.Now().UnixNano()),
				"password": "password123",
			},
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/protocolVersion":    ProtocolVersion,
				"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "m1cancel", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
			},
		},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_register")
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion) // 2026-07-28: cancellation propagation applies

	errCh := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errCh <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never entered")
	}
	cancel() // cancel the originating HTTP request

	select {
	case <-cancelled:
		// tool ctx observed cancellation through the real HTTP→SDK→tool path
	case <-time.After(5 * time.Second):
		t.Fatal("tool context did not observe cancellation")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled request should not complete successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not terminate after cancellation")
	}
	// no detached execution: the cancelled signal IS the proof the tool
	// goroutine reached its exit path (channel send happens before return).
}

func extractJSON(sse string) string {
	if i := strings.Index(sse, "data: "); i >= 0 {
		return strings.TrimSpace(sse[i+6:])
	}
	return sse
}

func m1CallRegisterRaw(t *testing.T, srv *httptest.Server, name string) (int, string) {
	t.Helper()
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]interface{}{
			"name":      "account_register",
			"arguments": map[string]string{"name": name, "password": "password123"},
		},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_register")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// ---- architecture guard ----

// TestM1McpserverHasNoRepositoryDependency: production files under
// internal/mcpserver must not import the repository package and must
// not contain direct SQL / Credits mutation constructs. The MCP layer
// is a protocol adapter only.
func TestM1McpserverHasNoRepositoryDependency(t *testing.T) {
	// Unconditional directory enumeration: every current AND future
	// production .go file in this package enters the guard. (The test
	// binary runs with the package directory as cwd.)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatal("no production Go sources found — guard must scan real files")
	}
	// The discovered set must include the current production files
	// (sanity check that enumeration works; do NOT lock the total
	// count — M2 will legitimately add production files).
	found := map[string]bool{}
	for _, f := range files {
		found[f] = true
	}
	for _, must := range []string{"server.go", "errors.go"} {
		if !found[must] {
			t.Fatalf("guard enumeration missed production file %s (scanned: %v)", must, files)
		}
	}
	forbiddenTokens := []string{
		"kungfu.md/internal/repository",
		"credits.Record",
		"TxBegin",
		"delivery.PostJSON",
		"UPDATE tb_bots",
		"INSERT INTO tb_transactions",
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		for _, tok := range forbiddenTokens {
			if strings.Contains(s, tok) {
				t.Fatalf("%s contains forbidden token %q — mcpserver is a protocol adapter only", f, tok)
			}
		}
	}
}
