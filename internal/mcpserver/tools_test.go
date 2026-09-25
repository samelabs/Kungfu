package mcpserver

// Integration tests: memory + work tools over the official MCP Go
// client against httptest, with real PostgreSQL for business
// invariants. Tools delegate to existing service authorities; these
// tests prove the adapter wiring, the economic invariants, and the
// security invariants.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// m2Bot registers a fresh bot through the MCP register tool and
// returns (botName, rawKey, botID).
func m2Bot(t *testing.T, pool *pg.Pool, srv *httptest.Server, tag string) (string, string, int64) {
	t.Helper()
	name := "m2" + tag + "_" + fmt.Sprint(time.Now().UnixNano())
	sc, body := m1CallRegisterRaw(t, srv, name)
	if sc != 200 || !strings.Contains(body, "kf_live_") {
		t.Fatalf("register %s: %d %s", name, sc, body)
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent struct {
				APIKey  string `json:"api_key"`
				BotName string `json:"bot_name"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &env); err != nil {
		t.Fatalf("register parse: %v %s", err, body)
	}
	key := env.Result.StructuredContent.APIKey
	if key == "" {
		// fall back to text content
		for _, c := range env.Result.Content {
			if strings.Contains(c.Text, "kf_live_") {
				for _, f := range strings.Fields(c.Text) {
					if strings.HasPrefix(f, "kf_live_") {
						key = f
					}
				}
			}
		}
	}
	if key == "" {
		t.Fatalf("no raw key in register output: %s", body)
	}
	var botID int64
	row := pool.QueryRow(context.Background(),
		"SELECT id FROM tb_bots WHERE bot_name=$1", name)
	if err := row.Scan(&botID); err != nil {
		t.Fatalf("bot lookup: %v", err)
	}
	return name, key, botID
}

// m2CallTool drives tools/call over a RAW request with the standard
// 2026-07-28 headers (bearer optional) and returns (status, body).
func m2CallTool(t *testing.T, srv mcpTestServer, key, tool string, args map[string]interface{}) (int, string) {
	t.Helper()
	bodyMap := map[string]interface{}{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]interface{}{
			"name":      tool,
			"arguments": args,
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/protocolVersion":    ProtocolVersion,
				"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "m2-it", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
			},
		},
	}
	b, _ := json.Marshal(bodyMap)
	req, _ := http.NewRequest("POST", srv.srv.URL+"/mcp", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", tool)
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := srv.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

type mcpTestServer struct {
	srv    *httptest.Server
	client *http.Client
}

// toolsSetup builds a full MCP handler server.
func toolsSetup(t *testing.T) (*pg.Pool, mcpTestServer) {
	t.Helper()
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return pool, mcpTestServer{srv: srv, client: srv.Client()}
}

func TestMCPToolsEveryNewToolRequiresAuth(t *testing.T) {
	pool, ts := toolsSetup(t)
	_ = pool
	tools := []string{"memory_list", "memory_get", "memory_put", "memory_share",
		"memory_unshare", "memory_delete"}
	for _, tool := range tools {
		sc, _ := m2CallTool(t, ts, "", tool, map[string]interface{}{})
		if sc != 401 {
			t.Fatalf("%s without Authorization = %d, want 401", tool, sc)
		}
	}
}

func TestMCPToolsMemoryLifecycleAndFreeEconomics(t *testing.T) {
	pool, ts := toolsSetup(t)
	_, keyA, botA := m2Bot(t, pool, ts.srv, "a")
	_, keyB, botB := m2Bot(t, pool, ts.srv, "b")

	txCtx := context.Background()
	countTx := func(botID int64) int {
		var n int
		pool.QueryRow(txCtx,
			"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1", botID).Scan(&n)
		return n
	}
	balanceOf := func(botID int64) int64 {
		var b int64
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&b)
		return b
	}
	balA0 := countTx(botA)

	// put create
	sc, body := m2CallTool(t, ts, keyA, "memory_put", map[string]interface{}{
		"title": "m2 memory", "tags": []string{"m2"}, "content": "This is deliberately long memory content for the MCP integration test, exceeding fifty characters by a comfortable margin.",
	})
	if sc != 200 || strings.Contains(body, "\"error\"") {
		t.Fatalf("memory_put: %d %s", sc, body)
	}
	var putEnv struct {
		Result struct {
			StructuredContent struct {
				Code   string `json:"code"`
				Action string `json:"action"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &putEnv); err != nil {
		t.Fatalf("put parse: %v %s", err, body)
	}
	code := putEnv.Result.StructuredContent.Code
	action := putEnv.Result.StructuredContent.Action
	if code == "" || action != "created" {
		t.Fatalf("put result code=%q action=%q body=%q", code, action, extractJSON(body)[:min(400, len(extractJSON(body)))])
	}
	// create is FREE: no new Credits transaction
	if n := countTx(botA); n != balA0 {
		t.Fatalf("memory create produced %d new ledger rows (want 0)", n-balA0)
	}

	// list own
	sc, body = m2CallTool(t, ts, keyA, "memory_list", map[string]interface{}{})
	if sc != 200 || toolFailed(body) || !strings.Contains(body, code) {
		t.Fatalf("memory_list: %d %s", sc, extractJSON(body)[:min(400, len(extractJSON(body)))])
	}

	// get own (private)
	sc, body = m2CallTool(t, ts, keyA, "memory_get", map[string]interface{}{"code": code})
	if sc != 200 || strings.Contains(body, "\"error\"") {
		t.Fatalf("memory_get own: %d %.200s", sc, body)
	}

	// private non-owner get fails
	sc, body = m2CallTool(t, ts, keyB, "memory_get", map[string]interface{}{"code": code})
	if !(sc >= 400 || strings.Contains(body, "error") || strings.Contains(body, "isError\":true")) {
		t.Fatalf("private non-owner get should fail: %d %.200s", sc, body)
	}

	// share -> public get by other agent is FREE for the READER:
	// B's transaction count and balance must be unchanged.
	sc, _ = m2CallTool(t, ts, keyA, "memory_share", map[string]interface{}{"code": code})
	if sc != 200 {
		t.Fatalf("memory_share: %d", sc)
	}
	txB0 := countTx(botB)
	balB0 := balanceOf(botB)
	sc, body = m2CallTool(t, ts, keyB, "memory_get", map[string]interface{}{"code": code})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("public get by other: %d %.200s", sc, body)
	}
	if n := countTx(botB); n != txB0 {
		t.Fatalf("READER B: public get produced %d new ledger rows (want 0)", n-txB0)
	}
	if b := balanceOf(botB); b != balB0 {
		t.Fatalf("READER B: balance changed on free public get: %d -> %d", balB0, b)
	}
	// owner A also unchanged
	if n := countTx(botA); n != countTx(botA) {
		t.Fatal("unreachable")
	}

	// unshare -> non-owner get fails again
	_, _ = m2CallTool(t, ts, keyA, "memory_unshare", map[string]interface{}{"code": code})
	sc, body = m2CallTool(t, ts, keyB, "memory_get", map[string]interface{}{"code": code})
	if !(sc >= 400 || strings.Contains(body, "error") || strings.Contains(body, "isError\":true")) {
		t.Fatalf("unshared non-owner get should fail: %d %.200s", sc, body)
	}

	// put update (same code)
	sc, body = m2CallTool(t, ts, keyA, "memory_put", map[string]interface{}{
		"code": code, "title": "m2 memory v2", "tags": []string{"m2"}, "content": "This is deliberately long memory content for the MCP integration test, exceeding fifty characters by a comfortable margin. v2",
	})
	if sc != 200 || !strings.Contains(body, "updated") {
		if !strings.Contains(body, code) {
			t.Fatalf("memory_put update: %d %.200s", sc, body)
		}
	}

	// delete soft: no longer listed active
	sc, _ = m2CallTool(t, ts, keyA, "memory_delete", map[string]interface{}{"code": code})
	if sc != 200 {
		t.Fatalf("memory_delete: %d", sc)
	}
	sc, body = m2CallTool(t, ts, keyA, "memory_list", map[string]interface{}{})
	if strings.Contains(body, "\""+code+"\"") {
		t.Fatalf("deleted memory still listed: %.200s", body)
	}
}

// R6: MCP preserves the REST rate-limit actions list / push / get —
// first call passes, second call from the SAME authenticated bot is
// limited. No new action names.
func TestMCPToolsMemoryRateLimitsPreserved(t *testing.T) {
	pool := m1TestPool(t)
	limiter := ratelimit.NewLimiter(map[string]ratelimit.Config{
		"list": {Window: 3600, Limit: 1, Enabled: true},
		"push": {Window: 3600, Limit: 1, Enabled: true},
		"get":  {Window: 3600, Limit: 1, Enabled: true},
	})
	h := Handler(m1Deps(t, pool, limiter))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}
	_, key, _ := m2Bot(t, pool, srv, "rl")

	long := "This is deliberately long memory content for the rate limit test, well above fifty characters."

	// list: 1st OK, 2nd limited
	sc, body := m2CallTool(t, ts, key, "memory_list", map[string]interface{}{})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("list#1: %d %.200s", sc, body)
	}
	sc, body = m2CallTool(t, ts, key, "memory_list", map[string]interface{}{})
	if !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("list#2 not limited: %d %.200s", sc, body)
	}

	// push: 1st OK, 2nd limited
	sc, body = m2CallTool(t, ts, key, "memory_put", map[string]interface{}{
		"title": "rl put", "tags": []string{"t"}, "content": long})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("push#1: %d %.200s", sc, body)
	}
	var pe struct {
		Result struct {
			StructuredContent struct {
				Code string `json:"code"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(extractJSON(body)), &pe)
	if pe.Result.StructuredContent.Code == "" {
		t.Fatalf("push#1 code missing: %s", body)
	}
	sc, body = m2CallTool(t, ts, key, "memory_put", map[string]interface{}{
		"title": "rl put 2", "tags": []string{"t"}, "content": long})
	if !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("push#2 not limited: %d %.200s", sc, body)
	}

	// get: 1st OK, 2nd limited (own memory; the list/put pools are
	// exhausted but "get" is an independent action)
	sc, body = m2CallTool(t, ts, key, "memory_get", map[string]interface{}{
		"code": pe.Result.StructuredContent.Code})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("get#1: %d %.200s", sc, body)
	}
	sc, body = m2CallTool(t, ts, key, "memory_get", map[string]interface{}{
		"code": pe.Result.StructuredContent.Code})
	if !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("get#2 not limited: %d %.200s", sc, body)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toolFailed(body string) bool {
	j := extractJSON(body)
	return strings.Contains(j, chr34+"error"+chr34) || strings.Contains(j, "isError"+chr34+":true")
}

const chr34 = string(rune(34))

// R3: schema evidence — memory_put tags are required; Tool outputs
// are schema-backed (not generic objects).
func TestMCPToolsToolSchemaContract(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	client := mcp.NewClient(&mcp.Implementation{Name: "m2-schema", Version: "1"}, nil)
	ss, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer ss.Close()
	tools, err := ss.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// The transport-visible JSON is the contract: marshal each tool
	// and assert on the wire schema shape.
	type wireSchema struct {
		Type                 string                 `json:"type"`
		Properties           map[string]interface{} `json:"properties"`
		Required             []string               `json:"required"`
		AdditionalProperties interface{}            `json:"additionalProperties"`
	}
	type wireTool struct {
		Name         string      `json:"name"`
		InputSchema  *wireSchema `json:"inputSchema"`
		OutputSchema *wireSchema `json:"outputSchema,omitempty"`
		Annotations  *struct {
			OpenWorldHint  *bool `json:"openWorldHint,omitempty"`
			IdempotentHint *bool `json:"idempotentHint,omitempty"`
		} `json:"annotations,omitempty"`
	}
	raw, err := json.Marshal(tools.Tools)
	if err != nil {
		t.Fatal(err)
	}
	var wire []wireTool
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*wireTool{}
	for i := range wire {
		byName[wire[i].Name] = &wire[i]
	}

	// memory_put: tags required
	mp := byName["memory_put"]
	if mp == nil || mp.InputSchema == nil {
		t.Fatal("memory_put schema missing")
	}
	tagsRequired := false
	for _, r := range mp.InputSchema.Required {
		if r == "tags" {
			tagsRequired = true
		}
	}
	if !tagsRequired {
		t.Fatal("memory_put tags must be advertised as required (existing service rejects missing tags)")
	}

	// Outputs expose outputSchema (typed DTOs, not generic objects)
	for _, name := range []string{"memory_list", "memory_get", "memory_put", "memory_share",
		"memory_unshare", "memory_delete"} {
		tl := byName[name]
		if tl == nil {
			t.Fatalf("tool %s missing", name)
		}
		if tl.OutputSchema == nil || tl.OutputSchema.Type != "object" {
			t.Fatalf("tool %s has no typed outputSchema", name)
		}
	}

	// annotation truthfulness
	for _, name := range []string{"memory_share", "memory_unshare"} {
		tl := byName[name]
		if tl == nil || tl.Annotations == nil || tl.Annotations.IdempotentHint == nil || !*tl.Annotations.IdempotentHint {
			t.Fatalf("%s idempotentHint must be true (existing op is idempotent)", name)
		}
	}
}

// ---- exact service-contract projections ----

// m2Typed calls a tool and decodes structuredContent into out.
func m2Typed(t *testing.T, ts mcpTestServer, key, tool string, args map[string]interface{}, out interface{}) {
	t.Helper()
	sc, body := m2CallTool(t, ts, key, tool, args)
	if sc != 200 || toolFailed(body) {
		t.Fatalf("%s: %d %s", tool, sc, extractJSON(body)[:min(400, len(extractJSON(body)))])
	}
	var env struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &env); err != nil {
		t.Fatalf("%s parse: %v", tool, err)
	}
	if len(env.Result.StructuredContent) == 0 {
		t.Fatalf("%s: no structuredContent: %s", tool, body)
	}
	if err := json.Unmarshal(env.Result.StructuredContent, out); err != nil {
		t.Fatalf("%s typed decode: %v (raw %s)", tool, err, env.Result.StructuredContent)
	}
}

func TestMCPToolsMemoryProjectionExactServiceFacts(t *testing.T) {
	pool, ts := toolsSetup(t)
	_, keyA, _ := m2Bot(t, pool, ts.srv, "cj")
	long := "Closure test memory content deliberately longer than fifty characters for projection."

	// create WITH description
	var put MemoryPutOutput
	m2Typed(t, ts, keyA, "memory_put", map[string]interface{}{
		"title": "closure mem", "tags": []string{"alpha", "beta"},
		"description": "described item", "content": long,
	}, &put)
	if put.Code == "" || put.Action != "created" {
		t.Fatalf("put: %+v", put)
	}

	// list: tags exact, description preserved, NO checksum
	var list MemoryListOutput
	m2Typed(t, ts, keyA, "memory_list", map[string]interface{}{}, &list)
	var found *MemoryItem
	for i := range list.Items {
		if list.Items[i].Code == put.Code {
			found = &list.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("created memory missing from list: %+v", list)
	}
	if len(found.Tags) != 2 || found.Tags[0] != "alpha" || found.Tags[1] != "beta" {
		t.Fatalf("list tags not exactly preserved: %v", found.Tags)
	}
	if found.Description == nil || *found.Description != "described item" {
		t.Fatalf("list description lost: %v", found.Description)
	}
	listRaw, _ := json.Marshal(list)
	if strings.Contains(string(listRaw), "checksum") {
		t.Fatalf("list output contains checksum (service list does not provide it): %s", listRaw)
	}

	// get: tags exact, description preserved, checksum present (detail has it)
	var get MemoryGetOutput
	m2Typed(t, ts, keyA, "memory_get", map[string]interface{}{"code": put.Code}, &get)
	if len(get.Tags) != 2 || get.Tags[0] != "alpha" || get.Tags[1] != "beta" {
		t.Fatalf("get tags not exactly preserved: %v", get.Tags)
	}
	if get.Description == nil || *get.Description != "described item" {
		t.Fatalf("get description lost: %v", get.Description)
	}
	if get.Checksum == "" {
		t.Fatal("get detail missing checksum (service detail provides it)")
	}

	// nil description remains valid (no description on update-create path)
	var put2 MemoryPutOutput
	m2Typed(t, ts, keyA, "memory_put", map[string]interface{}{
		"title": "closure nodesc", "tags": []string{"solo"}, "content": long + " v2",
	}, &put2)
	var get2 MemoryGetOutput
	m2Typed(t, ts, keyA, "memory_get", map[string]interface{}{"code": put2.Code}, &get2)
	if get2.Description != nil && *get2.Description != "" {
		t.Fatalf("empty description should stay nil/empty: %v", get2.Description)
	}

	// delete: real service facts only — code/title/message, NO deleted flag
	var del MemoryDeleteOutput
	m2Typed(t, ts, keyA, "memory_delete", map[string]interface{}{"code": put2.Code}, &del)
	if del.Code != put2.Code || del.Title != "closure nodesc" || del.Message == "" {
		t.Fatalf("delete projection: %+v", del)
	}
	delRaw, _ := json.Marshal(del)
	if strings.Contains(string(delRaw), "deleted") {
		t.Fatalf("delete output contains invented 'deleted' flag: %s", delRaw)
	}
}

// Malformed required facts fail safely: the projection can never
// manufacture a successful zero-value DTO.
func TestMCPToolsMalformedProjectionFailsSafely(t *testing.T) {
	// memory list missing meta
	if _, err := projectMemoryList(map[string]interface{}{"kungfus": []interface{}{}}); err == nil {
		t.Fatal("missing meta accepted")
	}
	// list item missing required title
	bad := map[string]interface{}{
		"kungfus": []interface{}{map[string]interface{}{"code": "x"}},
		"meta":    map[string]interface{}{"total": 1, "returned": 1, "offset": 0, "has_more": false},
	}
	if _, err := projectMemoryList(bad); err == nil {
		t.Fatal("item missing title accepted")
	}
	// tags wrong type
	badTags := map[string]interface{}{
		"kungfus": []interface{}{map[string]interface{}{"code": "x", "title": "t", "tags": "not-a-slice"}},
		"meta":    map[string]interface{}{"total": 1, "returned": 1, "offset": 0, "has_more": false},
	}
	if _, err := projectMemoryList(badTags); err == nil {
		t.Fatal("wrong-type tags accepted")
	}
	// get missing content
	if _, err := projectMemoryGet(map[string]interface{}{"code": "x", "title": "t", "tags": []string{}, "checksum": "c", "visibility": "public", "created_at": "1", "updated_at": "1"}); err == nil {
		t.Fatal("get missing content accepted")
	}
	// delete missing message
	if _, err := projectDelete(map[string]interface{}{"code": "x", "title": "t"}); err == nil {
		t.Fatal("delete missing message accepted")
	}
	// error shape is the safe INTERNAL_ERROR mapping
	err := mapProjErr(nil)
	te, ok := err.(*toolError)
	if !ok || te.httpStatus != 500 || te.code != "INTERNAL_ERROR" {
		t.Fatalf("projection error mapping: %+v", err)
	}
}
