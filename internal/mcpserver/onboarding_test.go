package mcpserver

// Onboarding behavior locks: a fresh agent that only knows the /mcp URL
// must be able to bootstrap without any out-of-band knowledge:
//
//  1. GET  /mcp                → 405 + Allow: POST + bootstrap guidance body
//  2. POST /mcp w/o Mcp-Method → 400 + bootstrap guidance (never body-parsed)
//  3. server/discover          → instructions carry the bootstrap path
//  4. official SDK Connect (no bearer) → discover + tools/list succeed
//  5. anonymous account_register → returns the one-shot Agent key
//  6. Bearer <key> account_status → 200
//  7. protected call w/o key   → 401 + WWW-Authenticate: Bearer realm="kungfu.md"
//
// Anonymous allowlist itself (discover/tools/list/account_register
// public; everything else 401) is unchanged by this hotfix.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/pg"
)

func m1SetupServer(t *testing.T) (*httptest.Server, *pg.Pool) {
	t.Helper()
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, pool
}

func m1Get(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func m1ReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String()
}

// 1. GET /mcp: 405 + Allow: POST + guidance body pointing at the docs.
// m1CleanupBot schedules deletion of a bot created via the anonymous
// registration path (plus its direct transaction/log rows), so reruns
// of this file leave no onb_* residue in the shared test database.
func m1CleanupBot(t *testing.T, botName string) {
	t.Helper()
	pool := m1TestPool(t)
	t.Cleanup(func() {
		ctx := context.Background()
		var id int64
		if err := pool.QueryRow(ctx, `SELECT id FROM tb_bots WHERE bot_name=$1`, botName).Scan(&id); err != nil {
			return // already gone
		}
		_, _ = pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE bot_id=$1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, id)
	})
}

func TestM1GetMCP405CarriesBootstrapGuidance(t *testing.T) {
	srv, _ := m1SetupServer(t)
	resp := m1Get(t, srv)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	body := m1ReadBody(t, resp)
	for _, want := range []string{ProtocolVersion, "account_register", "https://kungfu.md/llms.txt", "kungfu_skill.md"} {
		if !strings.Contains(body, want) {
			t.Fatalf("405 body missing %q", want)
		}
	}
}

// 2. Anonymous POST without Mcp-Method: 400 + guidance; the decision
// must not depend on the body (empty AND JSON-RPC bodies both 400).
func TestM1AnonymousPostMissingMethodHeaderGetsGuidance(t *testing.T) {
	srv, _ := m1SetupServer(t)
	// Only requests the edge cannot classify still get the guidance: a
	// well-formed JSON-RPC call without headers is served (edge_test.go).
	for _, body := range []string{"", "{}", "not json", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := resp.StatusCode
		text := m1ReadBody(t, resp)
		if got != http.StatusBadRequest {
			t.Fatalf("POST missing Mcp-Method (body %q) = %d, want 400", body, got)
		}
		if !strings.Contains(text, "account_register") || !strings.Contains(text, "Bearer") {
			t.Fatalf("400 body lacks bootstrap guidance: %q", text)
		}
	}
}

// 3. server/discover instructions carry the bootstrap path.
func TestM1DiscoverInstructionsContainBootstrap(t *testing.T) {
	srv, _ := m1SetupServer(t)
	code, out := m1RawRequest(t, srv, map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "server/discover",
		"params": map[string]interface{}{
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/protocolVersion":    ProtocolVersion,
				"io.modelcontextprotocol/clientInfo":         map[string]interface{}{"name": "onboard-probe", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
			},
		},
	}, map[string]string{
		"Mcp-Method":           "server/discover",
		"Mcp-Protocol-Version": ProtocolVersion,
	})
	if code != http.StatusOK {
		t.Fatalf("discover = %d: %s", code, out)
	}
	if !strings.Contains(out, "instructions") {
		t.Fatalf("discover result missing instructions: %s", out)
	}
	for _, want := range []string{"account_register", "Bearer", "Agent key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("discover instructions missing %q", want)
		}
	}
}

// 4+5. Official SDK flow, no bearer anywhere: Connect (discover-based
// handshake) → tools/list → account_register returns the one-shot key.
func TestM1SDKAnonymousBootstrapFlow(t *testing.T) {
	srv, _ := m1SetupServer(t)

	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client := mcp.NewClient(&mcp.Implementation{Name: "onboard-test", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("anonymous SDK Connect: %v", err)
	}
	defer ss.Close()

	// Instructions surface through the discovery result.
	if ir := ss.InitializeResult(); ir == nil || !strings.Contains(ir.Instructions, "account_register") {
		t.Fatal("SDK session missing bootstrap instructions")
	}

	// tools/list anonymously.
	tools, err := ss.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("anonymous tools/list: %v", err)
	}
	found := false
	for _, tl := range tools.Tools {
		if tl.Name == "account_register" {
			found = true
		}
	}
	if !found {
		t.Fatal("tools/list did not expose account_register")
	}

	// account_register anonymously → one-shot Agent key. The timestamp
	// lives in the FIRST half of the name so the 32-char cap can never
	// strip it — reruns stay collision-free.
	name := "onb_" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36)) + "_sdk"
	reg, err := ss.CallTool(ctx, &mcp.CallToolParams{
		Name: "account_register",
		Arguments: map[string]any{
			"name":     name,
			"password": "password123",
		},
	})
	if err != nil {
		t.Fatalf("anonymous account_register: %v", err)
	}
	// Remove the bot (and its direct rows) as soon as the subtest ends.
	m1CleanupBot(t, name)
	if reg.IsError {
		rawErr, _ := json.Marshal(reg.Content)
		t.Fatalf("account_register errored: %s", rawErr)
	}
	raw, _ := json.Marshal(reg.Content)
	var key string
	for _, c := range reg.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "key") {
			var out struct {
				BotName string `json:"bot_name"`
				APIKey  string `json:"api_key"`
			}
			if json.Unmarshal([]byte(tc.Text), &out) == nil && out.APIKey != "" {
				key = out.APIKey
			}
		}
	}
	if key == "" {
		t.Fatalf("no api_key in register output: %s", raw)
	}

	// 6. Bearer <key> account_status → 200 success.
	resp6, out := m1CallStatusRaw(t, srv, "Bearer "+key)
	if resp6.StatusCode != http.StatusOK || strings.Contains(out, "isError\":true") {
		t.Fatalf("account_status with fresh key: %d %s", resp6.StatusCode, out)
	}

	// 7. Protected call WITHOUT key: 401 + Bearer challenge.
	resp7, _ := m1CallStatusRaw(t, srv, "")
	if resp7.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected without key = %d, want 401", resp7.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"account_status","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "account_status")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if chal := resp.Header.Get("WWW-Authenticate"); !strings.Contains(chal, `Bearer`) || !strings.Contains(chal, `realm="kungfu.md"`) {
		t.Fatalf("401 WWW-Authenticate = %q", chal)
	}
}
