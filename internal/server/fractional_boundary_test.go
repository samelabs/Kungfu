package server

// J-contract boundary tests: fractional Credits are rejected fail-closed
// at the public HTTP boundaries (owner task create/edit/add-budget,
// admin store create/edit). MCP work_publish rejection is covered in
// internal/mcpserver; CREEM_PACKAGES_JSON in internal/config. Integers
// stay valid, and no silent rounding ever happens. Real PostgreSQL.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ctxBg() context.Context { return context.Background() }

type econEnv struct {
	s     *Server
	botID int64
}

func newEconEnv(t *testing.T, balance int64) *econEnv {
	t.Helper()
	e := &econEnv{s: newAdminEnv(t).s}
	suffix := fmt.Sprint(time.Now().UnixNano())
	sum := sha256.Sum256([]byte("econ_" + suffix))
	if err := e.s.Pool.QueryRow(ctxBg(), `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance, status)
		VALUES ($1, 'x', $2, 'ab12', $3, 'active') RETURNING id`,
		"econ_"+suffix, sum[:], balance).Scan(&e.botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(ctxBg(), `DELETE FROM tb_tasks WHERE bot_id=$1`, e.botID)
		_, _ = e.s.Pool.Exec(ctxBg(), `DELETE FROM tb_transactions WHERE bot_id=$1`, e.botID)
		_, _ = e.s.Pool.Exec(ctxBg(), `DELETE FROM tb_logs WHERE bot_id=$1`, e.botID)
		_, _ = e.s.Pool.Exec(ctxBg(), `DELETE FROM tb_bots WHERE id=$1`, e.botID)
	})
	return e
}

// ownerPOST drives an owner-session endpoint with a raw JSON body.
func (e *econEnv) ownerPOST(t *testing.T, path, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	w := httptest.NewRecorder()
	setOwnerCookie(w, e.botID, e.s.Config.SessionSecret, false)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "kf_owner="+cookieValue(w))
	rec := httptest.NewRecorder()
	e.s.buildRouter().ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func cookieValue(w *httptest.ResponseRecorder) string {
	h := w.Header().Get("Set-Cookie")
	parts := strings.SplitN(h, ";", 2)
	kv := strings.SplitN(parts[0], "=", 2)
	return kv[1]
}

func errCode(t *testing.T, out map[string]interface{}) string {
	t.Helper()
	c, _ := out["error"].(map[string]interface{})
	if c == nil {
		return ""
	}
	s, _ := c["code"].(string)
	return s
}

func (e *econEnv) lastTaskCode(t *testing.T) string {
	t.Helper()
	var code string
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT code FROM tb_tasks WHERE bot_id=$1 ORDER BY id DESC LIMIT 1`, e.botID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	return code
}

func (e *econEnv) assertNoLeak(t *testing.T) {
	t.Helper()
	var n int
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT COUNT(*) FROM tb_tasks WHERE bot_id=$1`, e.botID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("task rows leaked: %d (err %v)", n, err)
	}
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, e.botID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ledger rows leaked: %d (err %v)", n, err)
	}
}

// Fractional budget/price in owner task create → 400, zero task, zero ledger.
func TestOwnerTaskCreateRejectsFractionalCredits(t *testing.T) {
	e := newEconEnv(t, 5000)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"fractional budget", `{"title":"F","requirements":"r","postapi":"https://example.com/h","budget":1000.1,"price":10}`, "INVALID_BUDGET"},
		{"fractional price", `{"title":"F","requirements":"r","postapi":"https://example.com/h","budget":1000,"price":1.5}`, "INVALID_PRICE"},
		{"tiny fraction", `{"title":"F","requirements":"r","postapi":"https://example.com/h","budget":1000,"price":0.0001}`, "INVALID_PRICE"},
		{"fractional budget string", `{"title":"F","requirements":"r","postapi":"https://example.com/h","budget":"1000.5","price":10}`, "INVALID_BUDGET"},
	}
	for _, tc := range cases {
		rec, out := e.ownerPOST(t, "/api/owner/tasks", tc.body)
		if rec.Code != 400 {
			t.Fatalf("%s: status = %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
		if got := errCode(t, out); got != tc.code {
			t.Fatalf("%s: code = %q, want %q", tc.name, got, tc.code)
		}
	}
	e.assertNoLeak(t)
}

// Integral inputs stay valid end-to-end.
func TestOwnerTaskCreateAcceptsIntegerCredits(t *testing.T) {
	e := newEconEnv(t, 5000)

	rec, out := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"OK","requirements":"r","postapi":"https://example.com/h","budget":1500,"price":100,"open_now":false}`)
	if rec.Code != 200 {
		t.Fatalf("integer create rejected: %d %s", rec.Code, rec.Body.String())
	}
	data, _ := out["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("no data in response: %v", out)
	}
	task, _ := data["task"].(map[string]interface{})
	if task == nil {
		t.Fatalf("no task in response: %v", out)
	}
	if task["budget"] != "1500" || task["price"] != "100" {
		t.Fatalf("integer task economics: %v", task)
	}
}

// Fractional add-budget amount → 400 INVALID_AMOUNT.
func TestOwnerAddBudgetRejectsFractional(t *testing.T) {
	e := newEconEnv(t, 5000)
	rec, _ := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"AB","requirements":"r","postapi":"https://example.com/h","budget":1200,"price":10}`)
	if rec.Code != 200 {
		t.Fatalf("seed create failed: %d %s", rec.Code, rec.Body.String())
	}
	code := e.lastTaskCode(t)

	rec, out := e.ownerPOST(t, "/api/owner/tasks/"+code+"/add-budget", `{"amount":1.5}`)
	if rec.Code != 400 {
		t.Fatalf("fractional add-budget status = %d, want 400", rec.Code)
	}
	if got := errCode(t, out); got != "INVALID_AMOUNT" {
		t.Fatalf("fractional add-budget code = %q, want INVALID_AMOUNT", got)
	}
	// integer amount still works
	rec, _ = e.ownerPOST(t, "/api/owner/tasks/"+code+"/add-budget", `{"amount":100}`)
	if rec.Code != 200 {
		t.Fatalf("integer add-budget rejected: %d", rec.Code)
	}
}

// Fractional price in task edit → 400.
func TestOwnerTaskEditRejectsFractionalPrice(t *testing.T) {
	e := newEconEnv(t, 5000)
	rec, _ := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"ED","requirements":"r","postapi":"https://example.com/h","budget":1200,"price":10}`)
	if rec.Code != 200 {
		t.Fatalf("seed create failed: %d", rec.Code)
	}
	code := e.lastTaskCode(t)
	// edit requires closed
	if _, err := e.s.Pool.Exec(ctxBg(),
		`UPDATE tb_tasks SET status='closed', closed_at=NOW() WHERE code=$1`, code); err != nil {
		t.Fatal(err)
	}
	rec, out := e.ownerPOST(t, "/api/owner/tasks/"+code+"/edit", `{"price":2.5}`)
	if rec.Code != 400 {
		t.Fatalf("fractional edit price status = %d, want 400", rec.Code)
	}
	if got := errCode(t, out); got != "INVALID_PRICE" {
		t.Fatalf("fractional edit code = %q, want INVALID_PRICE", got)
	}
}

// Fractional admin store product price → 400 (create and PATCH); integer OK.
func TestAdminStoreRejectsFractionalPrice(t *testing.T) {
	env := newB2HTTPEnv(t)
	suffix := fmt.Sprint(time.Now().UnixNano())

	rec := env.mutateJSON(t, "POST", "/api/samelabs/store/products",
		`{"title":"FRAC `+suffix+`","credits_price":12.5}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_PRICE") {
		t.Fatalf("fractional create = %d %s", rec.Code, rec.Body.String())
	}

	rec = env.mutateJSON(t, "POST", "/api/samelabs/store/products",
		`{"title":"FRAC2 `+suffix+`","credits_price":12}`)
	if rec.Code != 200 {
		t.Fatalf("integer create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	t.Cleanup(func() {
		_, _ = env.s.Pool.Exec(ctxBg(), `DELETE FROM tb_store_products WHERE code=$1`, created.Data.Code)
	})

	rec = env.mutateJSON(t, "PATCH", "/api/samelabs/store/products/"+created.Data.Code,
		`{"credits_price":13.75}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_PRICE") {
		t.Fatalf("fractional patch = %d %s", rec.Code, rec.Body.String())
	}

	var n int
	_ = env.s.Pool.QueryRow(ctxBg(),
		`SELECT COUNT(*) FROM tb_store_products WHERE title LIKE $1`, "FRAC%"+suffix).Scan(&n)
	if n != 1 {
		t.Fatalf("product rows leaked: %d", n)
	}
}
