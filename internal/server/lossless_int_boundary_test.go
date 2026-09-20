package server

// Lossless integer decoding at Credit-bearing public boundaries.
// Regression proof for the 2^53 class: json.Unmarshal into
// map[string]interface{} yields float64, which silently turns
// 9007199254740993 into 9007199254740992 BEFORE validation. The
// boundaries parse with json.Number (UseNumber) so the exact source
// text reaches parseCredits/jsonCredits.
//
// Also covers MaxInt64 handling and rejects-with-zero-mutation for
// out-of-range / fractional inputs.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
)

func (e *econEnv) countTasks(t *testing.T) int {
	var n int
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT COUNT(*) FROM tb_tasks WHERE bot_id=$1`, e.botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *econEnv) createPendingTask(t *testing.T, budget, price int64) string {
	rec, out := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"P","requirements":"r","postapi":"https://example.com/h","budget":`+
			strconv.FormatInt(budget, 10)+`,"price":`+strconv.FormatInt(price, 10)+`,"open_now":false}`)
	if rec.Code != 200 {
		t.Fatalf("seed task: %d %s", rec.Code, rec.Body.String())
	}
	data, _ := out["data"].(map[string]interface{})
	task, _ := data["task"].(map[string]interface{})
	c, _ := task["code"].(string)
	if c == "" {
		t.Fatalf("no code: %v", out)
	}
	return c
}

func TestOwnerTaskCreateAccepts2p53Plus1Exactly(t *testing.T) {
	e := newEconEnv(t, 9223372036854775807) // MaxInt64 balance

	const exact = int64(9007199254740993) // 2^53+1 — float64 corrupts to ...992
	body := `{"title":"E","requirements":"r","postapi":"https://example.com/h","budget":` +
		strconv.FormatInt(exact, 10) + `,"price":1,"open_now":false}`
	rec, out := e.ownerPOST(t, "/api/owner/tasks", body)
	if rec.Code != 200 {
		t.Fatalf("exact 2^53+1 budget rejected: %d %s", rec.Code, rec.Body.String())
	}
	_ = out
	// Response wire text is the authority: decode it losslessly.
	var resp struct {
		Data struct {
			Task struct {
				Budget json.Number `json:"budget"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got, err := resp.Data.Task.Budget.Int64()
	if err != nil || got != exact {
		t.Fatalf("wire budget = %s, want exactly %d", resp.Data.Task.Budget.String(), exact)
	}
	// Database value exact.
	var dbBudget int64
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT budget FROM tb_tasks WHERE bot_id=$1 ORDER BY id DESC LIMIT 1`, e.botID).
		Scan(&dbBudget); err != nil || dbBudget != exact {
		t.Fatalf("db budget = %d (err %v), want exactly %d", dbBudget, err, exact)
	}
}

func TestOwnerTaskCreateMaxInt64Rejected(t *testing.T) {
	// MaxInt64 is a syntactically valid int64, but the budget cap
	// (1e12) and balance rules reject it. Zero mutation.
	e := newEconEnv(t, 1000000)
	before := e.countTasks(t)
	rec, _ := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"X","requirements":"r","postapi":"https://example.com/h","budget":9223372036854775807,"price":1,"open_now":false}`)
	if rec.Code == 200 {
		t.Fatal("MaxInt64 budget accepted")
	}
	if after := e.countTasks(t); after != before {
		t.Fatalf("mutation on rejected input: %d -> %d", before, after)
	}
	// Beyond int64: 2^63 — parse must reject, not wrap negative.
	rec, _ = e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"X","requirements":"r","postapi":"https://example.com/h","budget":9223372036854775808,"price":1,"open_now":false}`)
	if rec.Code == 200 {
		t.Fatal("2^63 budget accepted (would wrap negative via float64)")
	}
	if after := e.countTasks(t); after != before {
		t.Fatalf("mutation on rejected 2^63: %d -> %d", before, after)
	}
}

func TestOwnerAddBudgetMaxInt64RangeLossless(t *testing.T) {
	// add-budget on a MaxInt64-scale bot balance: the value must reach
	// validation as the exact integer, then pass or fail on the RULES —
	// never on float64 corruption. Use a legal value just over 2^53.
	e := newEconEnv(t, 9223372036854775807) // MaxInt64 balance
	code := e.createPendingTask(t, 1000, 1)
	rec, out := e.ownerPOST(t, "/api/owner/tasks/"+code+"/add-budget",
		`{"amount":9007199254740994}`) // 2^53+2: exact
	if rec.Code != 200 {
		t.Fatalf("exact 2^53+2 add-budget rejected: %d %s", rec.Code, rec.Body.String())
	}
	_ = out
	var dbBudget int64
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT budget FROM tb_tasks WHERE code=$1`, code).Scan(&dbBudget); err != nil || dbBudget != 1000+9007199254740994 {
		t.Fatalf("db budget = %d (err %v), want %d exactly", dbBudget, err, int64(1000+9007199254740994))
	}
}

func TestAdminStorePrice2p53Plus1Lossless(t *testing.T) {
	// Decode-path proof for the admin boundary: with UseNumber the exact
	// source text reaches jsonCredits; without it (float64 path) the
	// corrupted value MUST be refused rather than silently stored.
	var mNum map[string]interface{}
	dec := json.NewDecoder(bytes.NewBufferString(`{"credits_price":9007199254740993}`))
	dec.UseNumber()
	if err := dec.Decode(&mNum); err != nil {
		t.Fatal(err)
	}
	v, ok := jsonCredits(mNum["credits_price"])
	if !ok || v != 9007199254740993 {
		t.Fatalf("jsonCredits(UseNumber) = %d,%v — want exactly 9007199254740993", v, ok)
	}
	// The float64 decode of the same text is the corrupted value; the
	// compatibility case in jsonCredits must still reject fractions and
	// never accept the corrupted integer silently as a DIFFERENT number.
	var mF64 map[string]interface{}
	if err := json.Unmarshal([]byte(`{"credits_price":12.5}`), &mF64); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonCredits(mF64["credits_price"]); ok {
		t.Fatal("fractional credits_price accepted via float64 path")
	}
	// 2^53+0.5 literally rounds to 2^53 in float64 — with UseNumber the
	// exact text still reaches validation and is refused.
	var mF2 map[string]interface{}
	dec2 := json.NewDecoder(bytes.NewBufferString(`{"credits_price":9007199254740992.5}`))
	dec2.UseNumber()
	if err := dec2.Decode(&mF2); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonCredits(mF2["credits_price"]); ok {
		t.Fatal("2^53+0.5 accepted via json.Number path — float64 would have hidden it")
	}
}
