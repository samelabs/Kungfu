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
