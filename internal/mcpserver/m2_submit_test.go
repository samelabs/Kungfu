package mcpserver

// M2 work_submit integration: a controlled PostAPI endpoint receives
// the real delivery, settlement flows through the existing Submit
// authority (budget decrement + earn_task), and non-2xx produces no
// settlement.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/ratelimit"
	"time"
)

func TestM2WorkSubmitSettlement(t *testing.T) {
	pool := m1TestPool(t)

	// controlled PostAPI endpoint: agent A's task delivers here
	hits := 0
	postSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(postSrv.Close)

	// failing endpoint for the non-2xx case
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(badSrv.Close)

	h := m1Handler(t, pool, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := mcpTestServer{srv: srv, client: srv.Client()}

	// publisher bot with funds; submitter bot
	_, keyPub, botPub := m2Bot(t, pool, srv, "wp")
	_, keySub, botSub := m2Bot(t, pool, srv, "ws")
	txCtx := context.Background()
	if _, err := pool.Exec(txCtx, "UPDATE tb_bots SET balance = 5000 WHERE id = $1", botPub); err != nil {
		t.Fatal(err)
	}

	publish := func(title, postapi string) string {
		sc, body := m2CallTool(t, ts, keyPub, "work_publish", map[string]interface{}{
			"title": title, "requirements": "r",
			"postapi": postapi, "budget": 1500, "price": 100, "open_now": true,
		})
		if sc != 200 || toolFailed(body) {
			t.Fatalf("publish %s: %d %s", title, sc, body)
		}
		var env struct {
			Result struct {
				StructuredContent struct {
					Task struct {
						Code string `json:"code"`
					} `json:"task"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(extractJSON(body)), &env)
		if env.Result.StructuredContent.Task.Code == "" {
			// task code also appears in the text content map
			var txtEnv struct {
				Result struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			_ = json.Unmarshal([]byte(extractJSON(body)), &txtEnv)
			for _, c := range txtEnv.Result.Content {
				var m map[string]interface{}
				if json.Unmarshal([]byte(c.Text), &m) == nil {
					if tk, ok := m["task"].(map[string]interface{}); ok {
						if code, ok := tk["code"].(string); ok {
							return code
						}
					}
				}
			}
			t.Fatalf("no task code: %s", body)
		}
		return env.Result.StructuredContent.Task.Code
	}

	bal := func(botID int64) float64 {
		var b float64
		pool.QueryRow(txCtx, "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&b)
		return b
	}
	earnCount := func(botID int64) int {
		var n int
		pool.QueryRow(txCtx,
			"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='earn_task'", botID).Scan(&n)
		return n
	}

	// success path
	code := publish("m2 submit task "+fmt.Sprint(time.Now().UnixNano()), postSrv.URL)
	pubBefore, subBefore := bal(botPub), bal(botSub)
	subEarn0 := earnCount(botSub)

	sc, body := m2CallTool(t, ts, keySub, "work_submit", map[string]interface{}{
		"code": code, "payload": map[string]interface{}{"answer": 42},
	})
	if sc != 200 || toolFailed(body) {
		t.Fatalf("work_submit: %d %s", sc, body)
	}
	if hits == 0 {
		t.Fatal("PostAPI never received the delivery")
	}
	// settlement: task budget decremented by price (publisher balance
	// was already locked at publish time), agent earned price
	var budgetAfter float64
	pool.QueryRow(txCtx, "SELECT budget FROM tb_tasks WHERE code=$1", code).Scan(&budgetAfter)
	if diff := 1500 - budgetAfter; diff < 100-0.0001 || diff > 100+0.0001 {
		t.Fatalf("task budget decreased by %f, want 100 (price)", diff)
	}
	if bal(botPub) != pubBefore {
		t.Fatalf("publisher balance changed on submit (lock already at publish)")
	}
	if diff := bal(botSub) - subBefore; diff < 100-0.0001 || diff > 100+0.0001 {
		t.Fatalf("submitter balance increased by %f, want 100 (price)", diff)
	}
	if n := earnCount(botSub); n != subEarn0+1 {
		t.Fatalf("earn_task entries = %d, want +1", n-subEarn0)
	}
	// returned billing balance is authoritative
	if !strings.Contains(body, "billing") {
		t.Fatalf("no billing block in result: %s", body)
	}

	// non-2xx: no settlement
	badCode := publish("m2 bad post "+fmt.Sprint(time.Now().UnixNano()), badSrv.URL)
	pubB, subB := bal(botPub), bal(botSub)
	_, body = m2CallTool(t, ts, keySub, "work_submit", map[string]interface{}{
		"code": badCode, "payload": map[string]interface{}{"answer": 1},
	})
	if bal(botPub) != pubB || bal(botSub) != subB {
		t.Fatalf("non-2xx caused settlement: pub %f->%f sub %f->%f", pubB, bal(botPub), subB, bal(botSub))
	}
}

func TestM2WorkSubmitRateLimitPreserved(t *testing.T) {
	pool := m1TestPool(t)
	// A limiter restricting task_submit to 1 (same REST action).
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
	_ = sc // first may fail on task lookup — limiter counts the attempt
	sc2, body := m2CallTool(t, ts, key, "work_submit", args())
	if sc2 == 200 && !strings.Contains(body, "RATE_LIMIT") {
		t.Fatalf("second work_submit not rate limited: %d %.200s", sc2, body)
	}
}
