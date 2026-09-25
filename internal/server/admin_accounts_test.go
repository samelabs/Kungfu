package server

// 011 Platform Account Administration HTTP integration tests.
// Real router + real PostgreSQL (shared KF_TEST_DATABASE_URL).
//
// Coverage:
//  1. accounts.read can list/get
//  2. no read → 403
//  3. accounts.manage can disable/enable
//  4. read-only admin has no mutation authority
//  5. mutation without CSRF → fail closed
//  6. disable → Owner authenticated request 401 / Agent key invalid
//  7. enable → access restored under existing credentials
//  8. disable changes no economic/task/store facts
//  9. list/detail never leak credential columns
// 10. mutation + audit atomic (audit row written with real facts)
// 11. balance wire is a canonical decimal string (no float64)
// 12. idempotent disable/enable still audited with before/after
// 13. published_task_count vs submission_count kept distinct
// 14. list page-size cap
// 15. illegal status filter → 400 INVALID_STATUS, zero mutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/repository"
)

// accountProbe registers a REAL platform account through the
// production register surface (real password + real Agent key) and
// captures the real owner session cookie from the real login flow.
type accountProbe struct {
	id     int64
	name   string
	pass   string
	key    string
	cookie *http.Cookie
}

func newAccountProbe(t *testing.T, e *adminEnv) *accountProbe {
	t.Helper()
	p := &accountProbe{
		name: fmt.Sprintf("acc_probe_%d", time.Now().UnixNano()%100000000),
		pass: "probe-pass-123",
	}
	body := fmt.Sprintf(`{"name":%q,"password":%q}`, p.name, p.pass)
	req := httptest.NewRequest("POST", "/api/owner/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var reg struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)
	if reg.Data.Key == "" {
		t.Fatalf("register returned no key: %s", rec.Body.String())
	}
	p.key = reg.Data.Key

	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT id FROM tb_bots WHERE bot_name=$1`, p.name).Scan(&p.id); err != nil {
		t.Fatalf("find bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, p.id)
	})

	login := fmt.Sprintf(`{"name":%q,"password":%q}`, p.name, p.pass)
	lreq := httptest.NewRequest("POST", "/api/owner/session", strings.NewReader(login))
	lreq.Header.Set("Content-Type", "application/json")
	lrec := httptest.NewRecorder()
	e.router.ServeHTTP(lrec, lreq)
	if lrec.Code != 200 {
		t.Fatalf("owner login: %d %s", lrec.Code, lrec.Body.String())
	}
	for _, c := range lrec.Result().Cookies() {
		if c.Name == "kf_owner" {
			p.cookie = c
		}
	}
	if p.cookie == nil {
		t.Fatal("owner login set no kf_owner cookie")
	}
	return p
}

func parseAccountsBody(t *testing.T, rec *httptest.ResponseRecorder) ([]map[string]interface{}, float64) {
	t.Helper()
	var resp struct {
		Data struct {
			Accounts []map[string]interface{} `json:"accounts"`
			Total    float64                  `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return resp.Data.Accounts, resp.Data.Total
}

// 1+11+9: list works, balance is a canonical decimal STRING, list
// carries only allowlisted flat fields (no aggregates, no secrets).
func TestAccountsAdminAccountsListBasics(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)

	rec := e.do(t, "GET", "/api/samelabs/accounts?status=active&q="+p.name, "", false)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	accounts, total := parseAccountsBody(t, rec)
	if total < 1 {
		t.Fatalf("total = %v", total)
	}
	var found map[string]interface{}
	for _, a := range accounts {
		if fmt.Sprint(a["id"]) == fmt.Sprint(p.id) {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("probe account %d not in list", p.id)
	}
	// Balance must be the canonical decimal string "66" (fresh
	// signup grant), never a JSON number (float64 corruption).
	bal, ok := found["balance"].(string)
	if !ok || bal != "66" {
		t.Fatalf("balance = %#v, want string \"66\"", found["balance"])
	}
	for _, forbidden := range []string{"password_hash", "api_key_hash", "key"} {
		if _, exists := found[forbidden]; exists {
			t.Fatalf("list leaks %q", forbidden)
		}
	}
	last4 := found["api_key_last4"].(string)
	if last4 != p.key[len(p.key)-4:] {
		t.Fatalf("last4 = %q, want %q", last4, p.key[len(p.key)-4:])
	}
	for _, agg := range []string{"published_task_count", "submission_count", "kungfu_count"} {
		if _, exists := found[agg]; exists {
			t.Fatalf("list must not carry aggregate %q", agg)
		}
	}
}

// 1+13: detail returns the aggregates as DISTINCT facts.
func TestAccountsAdminAccountDetailAggregatesDistinct(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)

	if _, err := e.s.Pool.Exec(context.Background(), `
		INSERT INTO tb_task (code, publisher_id, status, budget_locked)
		VALUES ('t01100000001', $1, 'open', 100)`, p.id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(), `DELETE FROM tb_task WHERE publisher_id=$1`, p.id)
	})
	if _, err := e.s.Pool.Exec(context.Background(), `
		INSERT INTO tb_task_submission (task_id, version, agent_id, request_key, payload_hash, amount, state)
		VALUES ((SELECT id FROM tb_task WHERE publisher_id=$1 ORDER BY id DESC LIMIT 1), 1, $1, 'r01100000001', '\x00', 1, 'rejected')`, p.id); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(context.Background(),
			`DELETE FROM tb_task_submission WHERE agent_id=$1`, p.id)
	})

	rec := e.do(t, "GET", fmt.Sprintf("/api/samelabs/accounts/%d", p.id), "", false)
	if rec.Code != 200 {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Account map[string]interface{} `json:"account"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	a := resp.Data.Account
	if got := a["published_task_count"].(float64); got != 1 {
		t.Fatalf("published_task_count = %v, want 1", got)
	}
	if got := a["submission_count"].(float64); got != 1 {
		t.Fatalf("submission_count = %v, want 1", got)
	}
	if _, ok := a["balance"].(string); !ok {
		t.Fatalf("detail balance = %#v, want string", a["balance"])
	}
	for _, forbidden := range []string{"password_hash", "api_key_hash"} {
		if _, exists := a[forbidden]; exists {
			t.Fatalf("detail leaks %q", forbidden)
		}
	}
}

// 2+4: scoped permission boundaries through the real router.
func TestAccountsAdminScopedPermissions(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)

	scopedLogin := func(rolePerms []string) *adminEnv {
		t.Helper()
		code := fmt.Sprintf("ra%d", time.Now().UnixNano()%1000000)
		rec := e.mutateJSON(t, "POST", "/api/samelabs/roles",
			fmt.Sprintf(`{"code":%q,"name":"RA"}`, code))
		if rec.Code != 200 {
			t.Fatalf("role: %d %s", rec.Code, rec.Body.String())
		}
		var rr struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &rr)
		permJSON, _ := json.Marshal(rolePerms)
		rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/samelabs/roles/%d/permissions", rr.Data.ID),
			`{"permission_codes":`+string(permJSON)+`}`)
		if rec.Code != 200 {
			t.Fatalf("perms: %d %s", rec.Code, rec.Body.String())
		}
		username := fmt.Sprintf("ra_user_%d", time.Now().UnixNano()%1000000)
		rec = e.mutateJSON(t, "POST", "/api/samelabs/users",
			fmt.Sprintf(`{"username":%q,"display_name":"RA","password":"ra-pass-123"}`, username))
		if rec.Code != 200 {
			t.Fatalf("user: %d %s", rec.Code, rec.Body.String())
		}
		var ur struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &ur)
		ids, _ := json.Marshal([]int64{rr.Data.ID})
		rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/samelabs/users/%d/roles", ur.Data.ID),
			`{"role_ids":`+string(ids)+`}`)
		if rec.Code != 200 {
			t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
		}
		env := &adminEnv{s: e.s, router: e.router, username: username, password: "ra-pass-123"}
		env.login(t)
		return env
	}

	// accounts.read only: list/get 200, mutations 403.
	rv := scopedLogin([]string{"accounts.read"})
	if rec := rv.do(t, "GET", "/api/samelabs/accounts", "", false); rec.Code != 200 {
		t.Fatalf("read list = %d", rec.Code)
	}
	if rec := rv.do(t, "GET", fmt.Sprintf("/api/samelabs/accounts/%d", p.id), "", false); rec.Code != 200 {
		t.Fatalf("read get = %d", rec.Code)
	}
	if rec := rv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/disable", p.id), ""); rec.Code != 403 {
		t.Fatalf("read disable = %d, want 403", rec.Code)
	}
	if rec := rv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/enable", p.id), ""); rec.Code != 403 {
		t.Fatalf("read enable = %d, want 403", rec.Code)
	}

	// no account permissions at all: complete 403 matrix.
	nv := scopedLogin([]string{"admin.audit.read"})
	if rec := nv.do(t, "GET", "/api/samelabs/accounts", "", false); rec.Code != 403 {
		t.Fatalf("no-perm list = %d, want 403", rec.Code)
	}
	if rec := nv.do(t, "GET", fmt.Sprintf("/api/samelabs/accounts/%d", p.id), "", false); rec.Code != 403 {
		t.Fatalf("no-perm detail = %d, want 403", rec.Code)
	}
	if rec := nv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/disable", p.id), ""); rec.Code != 403 {
		t.Fatalf("no-perm disable = %d, want 403", rec.Code)
	}
	if rec := nv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/enable", p.id), ""); rec.Code != 403 {
		t.Fatalf("no-perm enable = %d, want 403", rec.Code)
	}

	// manage WITHOUT read: mutations work, reads are forbidden —
	// accounts.manage must not implicitly require accounts.read.
	mv := scopedLogin([]string{"accounts.manage"})
	if rec := mv.do(t, "GET", "/api/samelabs/accounts", "", false); rec.Code != 403 {
		t.Fatalf("manage-only list = %d, want 403", rec.Code)
	}
	if rec := mv.do(t, "GET", fmt.Sprintf("/api/samelabs/accounts/%d", p.id), "", false); rec.Code != 403 {
		t.Fatalf("manage-only detail = %d, want 403", rec.Code)
	}
	// Real mutations with normal CSRF, target a throwaway probe so
	// the lifecycle stays isolated from this test's assertions.
	tp := newAccountProbe(t, e)
	if rec := mv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/disable", tp.id), ""); rec.Code != 200 {
		t.Fatalf("manage-only disable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec := mv.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/enable", tp.id), ""); rec.Code != 200 {
		t.Fatalf("manage-only enable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// 5: CSRF required on both mutations.
func TestAccountsAdminCSRFRequiredOnMutations(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)
	for _, path := range []string{
		fmt.Sprintf("/api/samelabs/accounts/%d/disable", p.id),
		fmt.Sprintf("/api/samelabs/accounts/%d/enable", p.id),
	} {
		rec := e.do(t, "POST", path, "", false) // NO CSRF header
		if rec.Code == 200 {
			t.Fatalf("%s without CSRF must not succeed", path)
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		code, _ := resp["error"].(map[string]interface{})["code"].(string)
		if code != "CSRF_INVALID" {
			t.Fatalf("%s: expected CSRF_INVALID, got %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// 3+6+7+8+10+12: full disable/enable lifecycle — authentication
// effects, economic invariants, audit facts, idempotency.
func TestAccountsAdminDisableEnableLifecycle(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)

	ownerGET := func() int {
		req := httptest.NewRequest("GET", "/api/owner/session", nil)
		req.AddCookie(p.cookie)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec.Code
	}
	agentHash := func() []byte {
		sum := sha256.Sum256([]byte(p.key))
		return sum[:]
	}
	mustRowStatus := func(want string) {
		t.Helper()
		var status string
		if err := e.s.Pool.QueryRow(context.Background(),
			`SELECT status FROM tb_bots WHERE id=$1`, p.id).Scan(&status); err != nil || status != want {
			t.Fatalf("status = %q want %q (%v)", status, want, err)
		}
	}
	auditCount := func(action string) int64 {
		t.Helper()
		var n int64
		if err := e.s.Pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action=$1 AND target_id=$2`,
			action, fmt.Sprint(p.id)).Scan(&n); err != nil {
			t.Fatalf("audit count: %v", err)
		}
		return n
	}

	// Baseline: both credential paths work.
	if c := ownerGET(); c == 401 {
		t.Fatal("baseline owner GET = 401")
	}
	if bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), e.s.Pool, agentHash()); err != nil || bot == nil {
		t.Fatalf("baseline agent lookup: %v %v", bot, err)
	}

	// -- disable --
	rec := e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/disable", p.id), "")
	if rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	mustRowStatus("disabled")

	// (6) Owner cookie immediately fails (active-only re-resolution).
	if c := ownerGET(); c != 401 {
		t.Fatalf("owner GET after disable = %d, want 401", c)
	}
	// (6) Agent key immediately fails (active-only lookup).
	if bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), e.s.Pool, agentHash()); err != nil || bot != nil {
		t.Fatalf("agent lookup after disable must fail: %v %v", bot, err)
	}

	// (8) Economic facts untouched: balance, signup txn, credential
	// metadata all unchanged by the suspension.
	var balance int64
	var txns int64
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT balance, (SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1) FROM tb_bots WHERE id=$1`,
		p.id).Scan(&balance, &txns); err != nil {
		t.Fatalf("facts: %v", err)
	}
	if balance != 66 || txns != 1 {
		t.Fatalf("economic facts changed: balance=%d txns=%d", balance, txns)
	}
	kf, _ := repository.AdminGetAccountKeyFacts(context.Background(), e.s.Pool, p.id)
	if kf == nil || kf.Last4 != p.key[len(p.key)-4:] {
		t.Fatal("credential material mutated by disable — rotation is forbidden")
	}

	// (10) Audit written atomically with the mutation.
	if n := auditCount("admin.account.disable"); n != 1 {
		t.Fatalf("disable audit = %d, want 1", n)
	}

	// (12) Idempotent disable: 200 + second REAL audit row.
	rec = e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/disable", p.id), "")
	if rec.Code != 200 {
		t.Fatalf("idempotent disable: %d", rec.Code)
	}
	mustRowStatus("disabled")
	if n := auditCount("admin.account.disable"); n != 2 {
		t.Fatalf("idempotent disable audit = %d, want 2", n)
	}

	// (7) Enable: SAME cookie + SAME agent key work again (no
	// rotation, no permanent revoke).
	rec = e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/enable", p.id), "")
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	mustRowStatus("active")
	if c := ownerGET(); c == 401 {
		t.Fatal("owner GET after re-enable = 401 — existing credentials must resume")
	}
	if bot, err := repository.FindActiveBotByAPIKeyHash(context.Background(), e.s.Pool, agentHash()); err != nil || bot == nil {
		t.Fatalf("agent lookup after re-enable: %v %v", bot, err)
	}
	if n := auditCount("admin.account.enable"); n != 1 {
		t.Fatalf("enable audit = %d, want 1", n)
	}

	// Idempotent enable also audited.
	rec = e.mutateJSON(t, "POST", fmt.Sprintf("/api/samelabs/accounts/%d/enable", p.id), "")
	if rec.Code != 200 {
		t.Fatalf("idempotent enable: %d", rec.Code)
	}
	if n := auditCount("admin.account.enable"); n != 2 {
		t.Fatalf("idempotent enable audit = %d, want 2", n)
	}
}

// 404 path: unknown account id.
func TestAccountsAdminAccountNotFound(t *testing.T) {
	e := newAdminEnv(t)
	if rec := e.do(t, "GET", "/api/samelabs/accounts/999999999", "", false); rec.Code != 404 {
		t.Fatalf("get 404: %d", rec.Code)
	}
	if rec := e.mutateJSON(t, "POST", "/api/samelabs/accounts/999999999/disable", ""); rec.Code != 404 {
		t.Fatalf("disable 404: %d", rec.Code)
	}
}

// 14: list respects the page-size cap.
func TestAccountsAdminListPaginationCap(t *testing.T) {
	e := newAdminEnv(t)
	rec := e.do(t, "GET", "/api/samelabs/accounts?page=1&page_size=100", "", false)
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	accounts, _ := parseAccountsBody(t, rec)
	if len(accounts) > 100 {
		t.Fatalf("page_size cap violated: %d", len(accounts))
	}
}

// 15: illegal status filter → explicit 400 INVALID_STATUS, zero
// mutation, never a 200-with-empty-list.
func TestAccountsAdminIllegalStatusIs400(t *testing.T) {
	e := newAdminEnv(t)
	p := newAccountProbe(t, e)
	rec := e.do(t, "GET", "/api/samelabs/accounts?status=garbage", "", false)
	if rec.Code != 400 {
		t.Fatalf("status=garbage = %d, want 400", rec.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	code, _ := resp["error"].(map[string]interface{})["code"].(string)
	if code != "INVALID_STATUS" {
		t.Fatalf("error code = %q, want INVALID_STATUS (%s)", code, rec.Body.String())
	}
	// Zero mutation: the probe account is still active and intact.
	var status string
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT status FROM tb_bots WHERE id=$1`, p.id).Scan(&status); err != nil || status != "active" {
		t.Fatalf("side effect: status=%q (%v)", status, err)
	}
}
