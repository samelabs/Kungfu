package mcpserver

// M2 work_submit / discovery integration against controlled PostAPI
// endpoints and real PostgreSQL. Locks: payload shape delivered by the
// existing Submit→BuildPayload mechanism, authoritative billing values,
// zero settlement on delivery failure, and open+fundable-only discovery.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/ratelimit"
)

// submitOutputs is the typed work_submit structured result.
type submitOutputs struct {
	Result struct {
		StructuredContent WorkSubmitOutput `json:"structuredContent"`
	} `json:"result"`
}

// publishOut captures the typed work_publish result code.
func m2Publish(t *testing.T, ts mcpTestServer, key, title, postapi string, budget, price float64, openNow bool) WorkPublishOutput {
	t.Helper()
	sc, body := m2CallTool(t, ts, key, "work_publish", map[string]interface{}{
		"title": title, "requirements": "r", "postapi": postapi,
		"budget": budget, "price": price, "open_now": openNow,
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("publish %s: %d %s", title, sc, extractJSON(body)[:min(400, len(extractJSON(body)))])
	}
	var env struct {
		Result struct {
			StructuredContent WorkPublishOutput `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &env); err != nil {
		t.Fatalf("publish parse: %v %s", err, body)
	}
	if env.Result.StructuredContent.Code == "" {
		t.Fatalf("publish code missing: %s", body)
	}
	return env.Result.StructuredContent
}

func TestM2WorkSubmitPayloadAndBilling(t *testing.T) {
	pool := m1TestPool(t)

	// R7: the controlled PostAPI decodes the actual JSON request body.
	var mu sync.Mutex
	var bodies []map[string]interface{}
	postSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(postSrv.Close)

	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}

	_, keyPub, botPub := m2Bot(t, pool, srv, "wp")
	_, keySub, botSub := m2Bot(t, pool, srv, "ws")
	txCtx := context.Background()
	if _, err := pool.Exec(txCtx, "UPDATE tb_bots SET balance = 5000 WHERE id = $1", botPub); err != nil {
		t.Fatal(err)
	}

	out := m2Publish(t, ts, keyPub, "m2 payload task "+fmt.Sprint(time.Now().UnixNano()), postSrv.URL, 1500, 100, true)

	bal := func(botID int64) float64 {
		var b float64
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&b)
		return b
	}
	subBefore := bal(botSub)

	sc, body := m2CallTool(t, ts, keySub, "work_submit", map[string]interface{}{
		"code": out.Code, "payload": map[string]interface{}{"answer": 42},
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("work_submit: %d %s", sc, body)
	}

	// R7: delivered top-level shape: answer=42 AND task_code=<code> at
	// top level; NOT nested under "payload".
	mu.Lock()
	delivered := bodies[len(bodies)-1]
	mu.Unlock()
	if v, ok := delivered["answer"].(float64); !ok || v != 42 {
		t.Fatalf("PostAPI body missing top-level answer=42: %v", delivered)
	}
	if tc, _ := delivered["task_code"].(string); tc != out.Code {
		t.Fatalf("PostAPI body missing top-level task_code=%s: %v", out.Code, delivered)
	}
	if _, nested := delivered["payload"]; nested {
		t.Fatalf("payload was double-wrapped: %v", delivered)
	}

	// R8: typed billing/post values, authoritative against PostgreSQL.
	var res submitOutputs
	if err := json.Unmarshal([]byte(extractJSON(body)), &res); err != nil {
		t.Fatalf("submit parse: %v", err)
	}
	so := res.Result.StructuredContent
	if so.TaskCode != out.Code {
		t.Fatalf("task_code = %s, want %s", so.TaskCode, out.Code)
	}
	if !so.Post.Delivered {
		t.Fatalf("post.delivered = false")
	}
	if so.Post.ResponseCode != 200 {
		t.Fatalf("post.response_code = %d, want 200", so.Post.ResponseCode)
	}
	if so.Billing.Reward != 100 {
		t.Fatalf("billing.reward = %f, want 100 (task price)", so.Billing.Reward)
	}
	dbBal := bal(botSub)
	if so.Billing.Balance != dbBal {
		t.Fatalf("billing.balance = %f, want DB balance %f", so.Billing.Balance, dbBal)
	}
	if diff := dbBal - subBefore; diff < 100-0.0001 || diff > 100+0.0001 {
		t.Fatalf("submitter balance delta = %f, want 100", diff)
	}
}

// R9: true network failure -> zero settlement, no retry.
func TestM2WorkSubmitNetworkFailureZeroSettlement(t *testing.T) {
	pool := m1TestPool(t)

	// Create an endpoint, capture its URL, then CLOSE it — the task
	// now points at an unreachable address.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}

	_, keyPub, botPub := m2Bot(t, pool, srv, "np")
	_, keySub, botSub := m2Bot(t, pool, srv, "ns")
	txCtx := context.Background()
	if _, err := pool.Exec(txCtx, "UPDATE tb_bots SET balance = 5000 WHERE id = $1", botPub); err != nil {
		t.Fatal(err)
	}

	out := m2Publish(t, ts, keyPub, "m2 dead post "+fmt.Sprint(time.Now().UnixNano()), deadURL, 1500, 100, true)

	snap := func() (float64, float64, float64, int) {
		var budget, pubB, subB float64
		var earn int
		pool.QueryRow(txCtx, "SELECT budget FROM tb_tasks WHERE code=$1", out.Code).Scan(&budget)
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botPub).Scan(&pubB)
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botSub).Scan(&subB)
		pool.QueryRow(txCtx,
			"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='earn_task'", botSub).Scan(&earn)
		return budget, pubB, subB, earn
	}
	b0, p0, s0, e0 := snap()

	_, body := m2CallTool(t, ts, keySub, "work_submit", map[string]interface{}{
		"code": out.Code, "payload": map[string]interface{}{"x": 1},
	})
	if !toolFailed(body) {
		t.Fatalf("network failure must surface an error: %s", body)
	}

	b1, p1, s1, e1 := snap()
	if b1 != b0 {
		t.Fatalf("budget changed on network failure: %f -> %f", b0, b1)
	}
	if p1 != p0 {
		t.Fatalf("publisher balance changed after its publish lock: %f -> %f", p0, p1)
	}
	if s1 != s0 {
		t.Fatalf("submitter balance changed on network failure: %f -> %f", s0, s1)
	}
	if e1 != e0 {
		t.Fatalf("earn_task row appeared on network failure: %d -> %d", e0, e1)
	}
}

// R10: discovery = open + fundable only.
func TestM2WorkDiscoveryStatusContract(t *testing.T) {
	pool := m1TestPool(t)
	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}
	txCtx := context.Background()

	_, keyA, botA := m2Bot(t, pool, srv, "da")
	_, keyB, _ := m2Bot(t, pool, srv, "db")
	if _, err := pool.Exec(txCtx, "UPDATE tb_bots SET balance = 10000 WHERE id = $1", botA); err != nil {
		t.Fatal(err)
	}

	// pending (open_now=false): not discoverable
	pend := m2Publish(t, ts, keyA, "m2 pending "+fmt.Sprint(time.Now().UnixNano()),
		"https://example.test/hook", 1500, 100, false)
	if pend.Status != "pending" {
		t.Fatalf("open_now=false status = %s, want pending", pend.Status)
	}
	sc, body := m2CallTool(t, ts, keyB, "work_list", map[string]interface{}{})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("work_list: %d %s", sc, body)
	}
	if strings.Contains(body, pend.Code) {
		t.Fatal("pending task exposed by work_list")
	}
	sc, body = m2CallTool(t, ts, keyB, "work_get", map[string]interface{}{"code": pend.Code})
	if !toolFailed(body) {
		t.Fatalf("pending task exposed by work_get: %s", body)
	}

	// closed: not discoverable (fixture mutation of an open task)
	openT := m2Publish(t, ts, keyA, "m2 toclose "+fmt.Sprint(time.Now().UnixNano()),
		"https://example.test/hook", 1500, 100, true)
	if _, err := pool.Exec(txCtx,
		"UPDATE tb_tasks SET status='closed', closed_at=NOW() WHERE code=$1", openT.Code); err != nil {
		t.Fatal(err)
	}
	_, body = m2CallTool(t, ts, keyB, "work_list", map[string]interface{}{})
	if strings.Contains(body, openT.Code) {
		t.Fatal("closed task exposed by work_list")
	}

	// unfundable open (budget below min): not discoverable
	unf := m2Publish(t, ts, keyA, "m2 unfund "+fmt.Sprint(time.Now().UnixNano()),
		"https://example.test/hook", 1500, 100, true)
	if _, err := pool.Exec(txCtx,
		"UPDATE tb_tasks SET budget=1 WHERE code=$1", unf.Code); err != nil {
		t.Fatal(err)
	}
	_, body = m2CallTool(t, ts, keyB, "work_list", map[string]interface{}{})
	if strings.Contains(body, unf.Code) {
		t.Fatal("unfundable open task exposed by work_list")
	}
	// and a healthy open task IS discoverable
	okT := m2Publish(t, ts, keyA, "m2 healthy "+fmt.Sprint(time.Now().UnixNano()),
		"https://example.test/hook", 1500, 100, true)
	_, body = m2CallTool(t, ts, keyB, "work_list", map[string]interface{}{})
	if !strings.Contains(body, okT.Code) {
		t.Fatal("healthy open task missing from work_list")
	}
	// work_get does not mutate it
	sc, body = m2CallTool(t, ts, keyB, "work_get", map[string]interface{}{"code": okT.Code})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("work_get healthy: %d %s", sc, body)
	}
	var status string
	pool.QueryRow(txCtx, "SELECT status FROM tb_tasks WHERE code=$1", okT.Code).Scan(&status)
	if status != "open" {
		t.Fatalf("work_get mutated status: %q", status)
	}
}

func TestM2WorkSubmitRateLimitPreserved(t *testing.T) {
	pool := m1TestPool(t)
	lm := ratelimit.NewLimiter(map[string]ratelimit.Config{
		"task_submit": {Window: 3600, Limit: 1, Enabled: true},
	})
	h := Handler(m1Deps(t, pool, lm))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}
	_, key, _ := m2Bot(t, pool, srv, "sr")

	args := func() map[string]interface{} {
		return map[string]interface{}{"code": "nonexistent00", "payload": map[string]interface{}{"x": 1}}
	}
	sc, _ := m2CallTool(t, ts, key, "work_submit", args())
	_ = sc // first attempt consumes the quota (task lookup comes after the gate)
	sc2, body := m2CallTool(t, ts, key, "work_submit", args())
	if sc2 == 200 && !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("second work_submit not rate limited: %d %.200s", sc2, body)
	}
}
