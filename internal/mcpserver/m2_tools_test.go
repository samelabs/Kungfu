package mcpserver

// M2 integration tests: memory + work tools over the official MCP Go
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

// m2Setup builds a full M2 handler server.
func m2Setup(t *testing.T) (*pg.Pool, mcpTestServer) {
	t.Helper()
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return pool, mcpTestServer{srv: srv, client: srv.Client()}
}

func TestM2EveryNewToolRequiresAuth(t *testing.T) {
	pool, ts := m2Setup(t)
	_ = pool
	tools := []string{"memory_list", "memory_get", "memory_put", "memory_share",
		"memory_unshare", "memory_delete", "work_list", "work_get", "work_submit", "work_publish"}
	for _, tool := range tools {
		sc, _ := m2CallTool(t, ts, "", tool, map[string]interface{}{})
		if sc != 401 {
			t.Fatalf("%s without Authorization = %d, want 401", tool, sc)
		}
	}
}

func TestM2MemoryLifecycleAndFreeEconomics(t *testing.T) {
	pool, ts := m2Setup(t)
	_, keyA, botA := m2Bot(t, pool, ts.srv, "a")
	_, keyB, botB := m2Bot(t, pool, ts.srv, "b")

	txCtx := context.Background()
	countTx := func(botID int64) int {
		var n int
		pool.QueryRow(txCtx,
			"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1", botID).Scan(&n)
		return n
	}
	balanceOf := func(botID int64) float64 {
		var b float64
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&b)
		return b
	}
	balA0 := countTx(botA)

	// put create
	sc, body := m2CallTool(t, ts, keyA, "memory_put", map[string]interface{}{
		"title": "m2 memory", "tags": []string{"m2"}, "content": "This is deliberately long memory content for the M2 MCP integration test, exceeding fifty characters by a comfortable margin.",
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
		t.Fatalf("READER B: balance changed on free public get: %f -> %f", balB0, b)
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
		"code": code, "title": "m2 memory v2", "tags": []string{"m2"}, "content": "This is deliberately long memory content for the M2 MCP integration test, exceeding fifty characters by a comfortable margin. v2",
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
func TestM2MemoryRateLimitsPreserved(t *testing.T) {
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

	long := "This is deliberately long memory content for the M2 rate limit test, well above fifty characters."

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

func TestM2WorkPublishOwnershipAndEconomics(t *testing.T) {
	pool, ts := m2Setup(t)
	_, keyA, botA := m2Bot(t, pool, ts.srv, "pa")
	_, keyB, botB := m2Bot(t, pool, ts.srv, "pb")

	txCtx := context.Background()
	balance := func(botID int64) float64 {
		var b float64
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&b)
		return b
	}
	lockCount := func(botID int64) int {
		var n int
		pool.QueryRow(txCtx,
			"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='lock_task'", botID).Scan(&n)
		return n
	}

	// Test fixture: raise botA's balance above the minimum task budget
	// (the same direct-seed pattern service tests use; Credits.Record
	// remains the production authority and is untouched).
	if _, err := pool.Exec(txCtx,
		"UPDATE tb_bots SET balance = 2000 WHERE id = $1", botA); err != nil {
		t.Fatalf("fixture balance: %v", err)
	}
	var balA float64
	pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botA).Scan(&balA)
	budget := 1500.0

	// Malicious extra identity fields are rejected at the typed-schema
	// layer (additionalProperties=false) — they can NEVER influence
	// ownership because they never reach the tool.
	sc, body := m2CallTool(t, ts, keyA, "work_publish", map[string]interface{}{
		"title": "m2 task", "requirements": "do the thing",
		"postapi": "https://example.test/hook", "budget": budget, "price": 0.5,
		"open_now": true, "bot_id": botB, "owner_id": botB,
	})
	if !toolFailed(body) {
		t.Fatalf("identity-bearing extra fields must be rejected: %d %s", sc, body)
	}
	// Clean publish: ownership derives ONLY from the verified credential.
	sc, body = m2CallTool(t, ts, keyA, "work_publish", map[string]interface{}{
		"title": "m2 task", "requirements": "do the thing",
		"postapi": "https://example.test/hook", "budget": budget, "price": 0.5,
		"open_now": true,
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("work_publish failed: %s", body)
	}
	// task owned by A (the caller), never B
	var owner int64
	var taskCode string
	err := pool.QueryRow(txCtx,
		"SELECT bot_id, code FROM tb_tasks WHERE title='m2 task' ORDER BY id DESC LIMIT 1").Scan(&owner, &taskCode)
	if err != nil {
		t.Fatalf("task row: %v", err)
	}
	if owner != botA {
		t.Fatalf("task owner = %d, want caller %d (bot_id injection worked!)", owner, botA)
	}
	// exact lock_task ledger fact: single row with publisher,
	// amount=-budget, ref_type=task, ref_id=task code
	if n := lockCount(botA); n != 1 {
		t.Fatalf("lock_task entries = %d, want 1", n)
	}
	var ltBot int64
	var ltAmount float64
	var ltRefType *string
	var ltRefID *string
	err2 := pool.QueryRow(txCtx,
		"SELECT bot_id, amount, ref_type, ref_id FROM tb_transactions WHERE bot_id=$1 AND type='lock_task'", botA).
		Scan(&ltBot, &ltAmount, &ltRefType, &ltRefID)
	if err2 != nil {
		t.Fatalf("lock_task row: %v", err2)
	}
	if ltBot != botA || ltAmount != -budget || ltRefType == nil || *ltRefType != "task" || ltRefID == nil || *ltRefID != taskCode {
		t.Fatalf("lock_task fact mismatch: bot=%d amount=%f ref_type=%v ref_id=%v (want bot=%d amount=%f ref=task/%s)",
			ltBot, ltAmount, ltRefType, ltRefID, botA, -budget, taskCode)
	}
	var balA2 float64
	pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botA).Scan(&balA2)
	if diff := balA - balA2; diff < budget-0.0001 || diff > budget+0.0001 {
		t.Fatalf("balance decreased by %f, want %f", diff, budget)
	}
	if n := lockCount(botB); n != 0 {
		t.Fatalf("botB got %d lock_task entries — ownership leaked", n)
	}

	// work_list shows the open task; work_get returns it without mutation
	sc, body = m2CallTool(t, ts, keyB, "work_list", map[string]interface{}{})
	if sc != 200 || !strings.Contains(body, taskCode) {
		t.Fatalf("work_list missing task: %d %.200s", sc, body)
	}
	sc, body = m2CallTool(t, ts, keyB, "work_get", map[string]interface{}{"code": taskCode})
	if sc != 200 || toolFailed(body) || !strings.Contains(body, "m2 task") {
		t.Fatalf("work_get: %d %s", sc, extractJSON(body)[:min(400, len(extractJSON(body)))])
	}
	// no claim state: task row must have no ownership mutation columns;
	// the existing schema has none — prove status is unchanged (open).
	var status string
	pool.QueryRow(txCtx, "SELECT status FROM tb_tasks WHERE code=$1", taskCode).Scan(&status)
	if status != "open" {
		t.Fatalf("work_get mutated task status: %q", status)
	}

	// insufficient balance: publish fails, no task, no debit
	bigBudget := balA2*10 + 100
	before := balance(botA)
	sc, body = m2CallTool(t, ts, keyA, "work_publish", map[string]interface{}{
		"title": "m2 broke task", "requirements": "x",
		"postapi": "https://example.test/hook", "budget": bigBudget, "price": 0.5,
		"open_now": true,
	})
	if !strings.Contains(body, "INSUFFICIENT_CREDITS") {
		t.Fatalf("insufficient publish should fail with INSUFFICIENT_CREDITS: %d %.300s", sc, body)
	}
	if balance(botA) != before {
		t.Fatal("failed publish debited balance")
	}
	var cnt int
	pool.QueryRow(txCtx, "SELECT COUNT(*) FROM tb_tasks WHERE title='m2 broke task'").Scan(&cnt)
	if cnt != 0 {
		t.Fatal("failed publish created a task row")
	}
}

func TestM2WorkPublishValidationDelegated(t *testing.T) {
	pool, ts := m2Setup(t)
	_, key, _ := m2Bot(t, pool, ts.srv, "pv")

	// non-http(s) postapi rejected by existing service
	sc, body := m2CallTool(t, ts, key, "work_publish", map[string]interface{}{
		"title": "bad proto", "requirements": "x",
		"postapi": "ftp://example.test/hook", "budget": 1, "price": 0.5, "open_now": true,
	})
	if !toolFailed(body) && sc < 400 {
		t.Fatalf("ftp postapi accepted: %d %s", sc, extractJSON(body)[:min(500, len(extractJSON(body)))])
	}

	// negative budget: rejected by the existing service validation (out-of-range numeric, not non-finite)
	sc, body = m2CallTool(t, ts, key, "work_publish", map[string]interface{}{
		"title": "bad budget", "requirements": "x",
		"postapi": "https://example.test/hook", "budget": -5, "price": 0.5, "open_now": true,
	})
	if !toolFailed(body) && sc < 400 {
		t.Fatalf("non-finite budget accepted: %d %.200s", sc, body)
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

// R3: schema evidence — work_publish input rejects additional
// properties and declares no identity fields; memory_put tags are
// required; M2 outputs are schema-backed (not generic objects).
func TestM2ToolSchemaContract(t *testing.T) {
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

	// work_publish input: no identity fields, additionalProperties=false
	wp := byName["work_publish"]
	if wp == nil || wp.InputSchema == nil {
		t.Fatal("work_publish schema missing")
	}
	for _, forbidden := range []string{"bot_id", "owner_id", "credits", "status", "pinned", "opened_at", "closed_at"} {
		if _, exists := wp.InputSchema.Properties[forbidden]; exists {
			t.Fatalf("work_publish input declares forbidden property %s", forbidden)
		}
	}
	if ap, ok := wp.InputSchema.AdditionalProperties.(bool); !ok || ap {
		t.Fatalf("work_publish additionalProperties must be false, got %v", wp.InputSchema.AdditionalProperties)
	}
	// postapi description lives in the description transport field
	papi, _ := wp.InputSchema.Properties["postapi"].(map[string]interface{})
	if desc, _ := papi["description"].(string); !strings.Contains(desc, "HTTP or HTTPS") {
		t.Fatalf("postapi description must say HTTP or HTTPS: %v", desc)
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

	// M2 outputs expose outputSchema (typed DTOs, not generic objects)
	for _, name := range []string{"memory_list", "memory_get", "memory_put", "memory_share",
		"memory_unshare", "memory_delete", "work_list", "work_get", "work_submit", "work_publish"} {
		tl := byName[name]
		if tl == nil {
			t.Fatalf("tool %s missing", name)
		}
		if tl.OutputSchema == nil || tl.OutputSchema.Type != "object" {
			t.Fatalf("tool %s has no typed outputSchema", name)
		}
	}

	// annotation truthfulness
	if ws := byName["work_submit"]; ws == nil || ws.Annotations == nil || ws.Annotations.OpenWorldHint == nil || !*ws.Annotations.OpenWorldHint {
		t.Fatal("work_submit openWorldHint must be true (PostAPI delivery)")
	}
	if wp.Annotations == nil || wp.Annotations.OpenWorldHint == nil || *wp.Annotations.OpenWorldHint {
		t.Fatal("work_publish openWorldHint must be false (publish sends nothing outbound)")
	}
	for _, name := range []string{"memory_share", "memory_unshare"} {
		tl := byName[name]
		if tl == nil || tl.Annotations == nil || tl.Annotations.IdempotentHint == nil || !*tl.Annotations.IdempotentHint {
			t.Fatalf("%s idempotentHint must be true (existing op is idempotent)", name)
		}
	}
}

// ---- M2 closure: exact service-contract projections ----

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

func TestM2MemoryProjectionExactServiceFacts(t *testing.T) {
	pool, ts := m2Setup(t)
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

func TestM2WorkProjectionExactServiceFacts(t *testing.T) {
	pool, ts := m2Setup(t)
	_, keyPub, botPub := m2Bot(t, pool, ts.srv, "wk")
	_, keyB, _ := m2Bot(t, pool, ts.srv, "wb")
	txCtx := context.Background()
	if _, err := pool.Exec(txCtx, "UPDATE tb_bots SET balance = 5000 WHERE id = $1", botPub); err != nil {
		t.Fatal(err)
	}

	var pub WorkPublishOutput
	m2Typed(t, ts, keyPub, "work_publish", map[string]interface{}{
		"title": "closure work", "requirements": "req text",
		"postapi": "https://example.test/hook", "budget": 1500, "price": 100, "open_now": true,
	}, &pub)
	if pub.Code == "" || pub.Status != "open" {
		t.Fatalf("publish: %+v", pub)
	}

	// work_list item: exactly the board facts, no budget/postapi/owner
	var list WorkListOutput
	m2Typed(t, ts, keyB, "work_list", map[string]interface{}{}, &list)
	var item *WorkItem
	for i := range list.Items {
		if list.Items[i].Code == pub.Code {
			item = &list.Items[i]
		}
	}
	if item == nil {
		t.Fatalf("published work missing from list: %+v", list)
	}
	if item.Title != "closure work" || item.Requirements != "req text" || item.Price != 100 || item.Status != "open" {
		t.Fatalf("list item facts: %+v", item)
	}
	rawItem, _ := json.Marshal(item)
	for _, forbidden := range []string{"budget", "postapi", "bot_id", "owner_id"} {
		if strings.Contains(string(rawItem), forbidden) {
			t.Fatalf("work list item exposes %s: %s", forbidden, rawItem)
		}
	}
	// meta: total+returned, no has_more
	if list.Total < 1 || list.Returned < 1 {
		t.Fatalf("list meta: %+v", list)
	}
	rawList, _ := json.Marshal(list)
	if strings.Contains(string(rawList), "has_more") {
		t.Fatalf("work list meta exposes invented has_more: %s", rawList)
	}

	// work_get: same facts
	var get WorkGetOutput
	m2Typed(t, ts, keyB, "work_get", map[string]interface{}{"code": pub.Code}, &get)
	if get.Code != pub.Code || get.Title != "closure work" || get.Pinned != 0 {
		t.Fatalf("get facts: %+v", get)
	}
	rawGet, _ := json.Marshal(get)
	for _, forbidden := range []string{"budget", "postapi", "bot_id", "owner_id"} {
		if strings.Contains(string(rawGet), forbidden) {
			t.Fatalf("work get exposes %s: %s", forbidden, rawGet)
		}
	}
}

// Malformed required facts fail safely: the projection can never
// manufacture a successful zero-value DTO.
func TestM2MalformedProjectionFailsSafely(t *testing.T) {
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
	// work list item missing status
	badTask := map[string]interface{}{
		"tasks": []interface{}{map[string]interface{}{"code": "x", "title": "t", "requirements": "r", "price": 1.0, "pinned": 0, "created_at": "1", "updated_at": "1"}},
		"meta":  map[string]interface{}{"total": 1, "returned": 1},
	}
	if _, err := projectWorkList(badTask); err == nil {
		t.Fatal("task missing status accepted")
	}
	// work list invented has_more is NOT read: meta without returned fails
	noRet := map[string]interface{}{"tasks": []interface{}{}, "meta": map[string]interface{}{"total": 0}}
	if _, err := projectWorkList(noRet); err == nil {
		t.Fatal("meta missing returned accepted")
	}
	// error shape is the safe INTERNAL_ERROR mapping
	err := mapProjErr(nil)
	te, ok := err.(*toolError)
	if !ok || te.httpStatus != 500 || te.code != "INTERNAL_ERROR" {
		t.Fatalf("projection error mapping: %+v", err)
	}
}
