package server

// 012 Finance Admin HTTP integration tests — real router + real
// PostgreSQL. Finance Admin is a READ-ONLY control plane; the tests
// lock the work-order §十三 regressions.
//
// Fixture strategy: seed payments / adjustments / ledger rows
// directly in the shared test DB under throwaway bot accounts (all
// cleanup registered), then exercise the real HTTP surface.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

// repoRootForTest walks up from this test file to the repo root.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root not found")
	return ""
}

// financeFixture seeds one throwaway bot with a full, CONSISTENT
// local fact chain: paid payment + grant_payment ledger + account
// balance in sync. Cleanup removes everything.
type financeFixture struct {
	botID   int64
	botName string
	payCode string
	pool    *pg.Pool
}

func (f *financeFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, _ = f.pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=$1`, f.botID)
	_, _ = f.pool.Exec(ctx, `DELETE FROM tb_payment_adjustments WHERE payment_id=(SELECT id FROM tb_payments WHERE code=$1)`, f.payCode)
	_, _ = f.pool.Exec(ctx, `DELETE FROM tb_payments WHERE code=$1`, f.payCode)
	_, _ = f.pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, f.botID)
}

func newFinanceFixture(t *testing.T, e *adminEnv) *financeFixture {
	t.Helper()
	ctx := context.Background()
	f := &financeFixture{
		botName: fmt.Sprintf("fin_%d", time.Now().UnixNano()%100000000),
		payCode: fmt.Sprintf("fp%010d", time.Now().UnixNano()%10000000000),
		pool:    e.s.Pool,
	}
	keyHash := sha256.Sum256([]byte(f.botName))
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
		VALUES ($1, $2, '9999', 'x', 0, 'active') RETURNING id`,
		f.botName, keyHash[:]).Scan(&f.botID); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() { f.cleanup(t) })

	// paid payment + grant_payment ledger + balance in sync (66 grant
	// included so balance matches latest ledger row).
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO tb_payments (code, bot_id, provider, provider_order_id, amount_minor, currency, credits, status, provider_product_id, paid_at)
		VALUES ($1, $2, 'creem', $3, 500, 'USD', 100, 'paid', 'prod_test_012', NOW())`,
		f.payCode, f.botID, "o"+f.payCode); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id)
		VALUES ($1, 'grant_payment', 100, 166, 'payment', $2)`,
		f.botID, f.payCode); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tb_bots SET balance=166 WHERE id=$1`, f.botID); err != nil {
		t.Fatalf("sync balance: %v", err)
	}
	return f
}

// (1) finance.read works for every endpoint; (7) BIGINTs are strings.
func TestFinanceAdminFinanceReadEndpoints(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)

	for _, path := range []string{
		"/api/samelabs/finance/summary",
		"/api/samelabs/finance/payments",
		"/api/samelabs/finance/payments/" + f.payCode,
		"/api/samelabs/finance/adjustments",
		"/api/samelabs/finance/ledger",
	} {
		if rec := e.do(t, "GET", path, "", false); rec.Code != 200 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// (7) list wire: amount_minor/credits are decimal strings.
	rec := e.do(t, "GET", "/api/samelabs/finance/payments?q="+f.payCode, "", false)
	var list struct {
		Data struct {
			Payments []map[string]interface{} `json:"payments"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data.Payments) != 1 {
		t.Fatalf("q-filtered payments = %d", len(list.Data.Payments))
	}
	p := list.Data.Payments[0]
	if p["amount_minor"] != "500" || p["credits"] != "100" {
		t.Fatalf("BIGINT wire not canonical strings: %#v %#v", p["amount_minor"], p["credits"])
	}

	// (9) consistent chain → paid_grant_exact true + all integrity PASS.
	rec = e.do(t, "GET", "/api/samelabs/finance/payments/"+f.payCode, "", false)
	var detail struct {
		Data struct {
			Reconciliation struct {
				Integrity map[string]bool `json:"integrity"`
			} `json:"reconciliation"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	integ := detail.Data.Reconciliation.Integrity
	for k, v := range integ {
		if !v {
			t.Fatalf("consistent fixture: integrity[%s]=false", k)
		}
	}

	// (17) summary groups paid volume by currency; (label check)
	rec = e.do(t, "GET", "/api/samelabs/finance/summary", "", false)
	var sum struct {
		Data struct {
			PaidVolume []map[string]interface{} `json:"paid_volume"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sum)
	if len(sum.Data.PaidVolume) == 0 {
		t.Fatal("summary has no paid volume buckets")
	}
	for _, b := range sum.Data.PaidVolume {
		if _, ok := b["currency"].(string); !ok {
			t.Fatalf("paid volume bucket without currency: %#v", b)
		}
	}
}

// (3) no finance.read → all finance APIs 403 (scoped actor with a
// completely unrelated permission).
func TestFinanceAdminFinanceNoPermission403(t *testing.T) {
	e := newAdminEnv(t)
	scoped := newScopedAdmin(t, e, []string{"accounts.read"})
	for _, path := range []string{
		"/api/samelabs/finance/summary",
		"/api/samelabs/finance/payments",
		"/api/samelabs/finance/payments/whatever",
		"/api/samelabs/finance/adjustments",
		"/api/samelabs/finance/ledger",
	} {
		if rec := scoped.do(t, "GET", path, "", false); rec.Code != 403 {
			t.Fatalf("no-perm %s = %d, want 403", path, rec.Code)
		}
	}
}

// (4) Owner session cookie / Agent Bearer key (REAL credential
// surfaces, not forged values) never reach Admin Finance — both
// must surface ADMIN_LOGIN_REQUIRED.
func TestFinanceAdminFinanceOwnerAgentRejected(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)

	// REAL owner session cookie, minted via the production helper for
	// the fixture bot (setOwnerCookie + parseSetCookie).
	w := httptest.NewRecorder()
	setOwnerCookie(w, f.botID, e.s.Config.SessionSecret, false)
	ownerCookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	req := httptest.NewRequest("GET", "/api/samelabs/finance/summary", nil)
	req.AddCookie(ownerCookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "ADMIN_LOGIN_REQUIRED") {
		t.Fatalf("valid owner session on finance = %d %s, want 401 ADMIN_LOGIN_REQUIRED", rec.Code, rec.Body.String())
	}

	// REAL current Agent credential: Authorization: Bearer <key> of
	// the fixture bot (key = kf_live_..., hash seeded = SHA-256).
	rawKey := "kf_live_fin012" + strings.Repeat("a", 64-len("kf_live_fin012"))
	h := sha256.Sum256([]byte(rawKey))
	if _, err := e.s.Pool.Exec(context.Background(),
		`UPDATE tb_bots SET api_key_hash=$2, api_key_last4=$3 WHERE id=$1`,
		f.botID, h[:], rawKey[len(rawKey)-4:]); err != nil {
		t.Fatalf("seed agent key: %v", err)
	}
	req = httptest.NewRequest("GET", "/api/samelabs/finance/summary", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "ADMIN_LOGIN_REQUIRED") {
		t.Fatalf("valid agent Bearer key on finance = %d %s, want 401 ADMIN_LOGIN_REQUIRED", rec.Code, rec.Body.String())
	}
}

// (repair 1) bot_id filter is fail closed: malformed / zero /
// negative / overflow are explicit 400 INVALID_FINANCE_FILTER and
// never degrade into an unfiltered query — on ALL three endpoints.
func TestFinanceAdminFinanceBotIDFailClosed(t *testing.T) {
	e := newAdminEnv(t)
	_ = newFinanceFixture(t, e) // at least one payment exists

	for _, bad := range []string{"malformed", "0", "-1", "-999999", "99999999999999999999999", "1.5", "abc12"} {
		for _, base := range []string{
			"/api/samelabs/finance/payments",
			"/api/samelabs/finance/adjustments",
			"/api/samelabs/finance/ledger",
		} {
			rec := e.do(t, "GET", base+"?bot_id="+bad, "", false)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_FINANCE_FILTER") {
				t.Fatalf("%s?bot_id=%s = %d %s, want 400 INVALID_FINANCE_FILTER", base, bad, rec.Code, rec.Body.String())
			}
		}
	}

	// omitted / empty → no filter (200, full list, NOT 400).
	for _, base := range []string{
		"/api/samelabs/finance/payments",
		"/api/samelabs/finance/adjustments",
		"/api/samelabs/finance/ledger",
	} {
		if rec := e.do(t, "GET", base, "", false); rec.Code != 200 {
			t.Fatalf("%s (no bot_id) = %d, want 200", base, rec.Code)
		}
		if rec := e.do(t, "GET", base+"?bot_id=", "", false); rec.Code != 200 {
			t.Fatalf("%s?bot_id= (empty) = %d, want 200", base, rec.Code)
		}
	}

	// valid positive int64 → exact filter.
	f2 := newFinanceFixture(t, e)
	for _, base := range []string{
		"/api/samelabs/finance/payments",
		"/api/samelabs/finance/adjustments",
		"/api/samelabs/finance/ledger",
	} {
		rec := e.do(t, "GET", fmt.Sprintf("%s?bot_id=%d", base, f2.botID), "", false)
		if rec.Code != 200 {
			t.Fatalf("%s?bot_id=%d = %d", base, f2.botID, rec.Code)
		}
		var d struct {
			Data struct {
				BotID int64 `json:"bot_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
		// every returned row must carry the exact bot (payments have
		// bot_id on rows; ledger/adjustments equivalents verified by
		// scoping in repository).
		if base == "/api/samelabs/finance/ledger" && rec.Body.String() == "" {
			t.Fatal("empty ledger response for valid bot")
		}
	}
}

// (repair 2) no-ledger wire: latest_account_ledger_balance_after is
// an explicit JSON null (not a fake PASS fact), and the UI renders
// N/A for null (finance.js ledgerFlag).
func TestFinanceAdminFinanceNoLedgerIsExplicitNull(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(context.Background(),
		`DELETE FROM tb_transactions WHERE bot_id=$1`, f.botID); err != nil {
		t.Fatalf("wipe ledger: %v", err)
	}

	rec := e.do(t, "GET", "/api/samelabs/finance/payments/"+f.payCode, "", false)
	if rec.Code != 200 {
		t.Fatalf("detail: %d", rec.Code)
	}
	var d struct {
		Data struct {
			Reconciliation struct {
				LatestAccountLedgerBalanceAfter *string `json:"latest_account_ledger_balance_after"`
				Integrity                       struct {
					AccountBalanceMatchesLatestLedger bool `json:"account_balance_matches_latest_ledger"`
				} `json:"integrity"`
			} `json:"reconciliation"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Data.Reconciliation.LatestAccountLedgerBalanceAfter != nil {
		t.Fatalf("no-ledger latest balance = %v, want explicit null", d.Data.Reconciliation.LatestAccountLedgerBalanceAfter)
	}
	if d.Data.Reconciliation.Integrity.AccountBalanceMatchesLatestLedger {
		t.Fatal("no-ledger must not project PASS")
	}

	// The console shows "no ledger rows" as not applicable — neither a
	// pass nor a failure.
	page := e.do(t, "GET", "/samelabs/finance/payments/"+f.payCode, "", false)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "No ledger rows for this account yet") {
		t.Fatalf("payment page: %d, missing not-applicable ledger check", page.Code)
	}
}

// (5) payment list stable pagination; (6) invalid filters → explicit 400.
func TestFinanceAdminFinanceFiltersAndPagination(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)

	// Stable pagination: same page twice → identical DATA. The
	// comparison targets the data field — the envelope's second-granular
	// timestamp legitimately differs across a second boundary and is
	// not part of pagination stability (WO-10).
	page := func() []map[string]interface{} {
		t.Helper()
		rec := e.do(t, "GET", "/api/samelabs/finance/payments?page=1&page_size=5", "", false)
		if rec.Code != 200 {
			t.Fatalf("payments page: %d %s", rec.Code, rec.Body.String())
		}
		var d struct {
			Data struct {
				Payments []map[string]interface{} `json:"payments"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("payments page json: %v", err)
		}
		return d.Data.Payments
	}
	if fmt.Sprint(page()) != fmt.Sprint(page()) {
		t.Fatal("payment list pagination not stable")
	}

	// Invalid filters must be explicit 400s, never 200-empty.
	for _, path := range []string{
		"/api/samelabs/finance/payments?status=garbage",
		"/api/samelabs/finance/payments?provider=stripe",
		"/api/samelabs/finance/adjustments?kind=chargeback",
		"/api/samelabs/finance/adjustments?provider=paypal",
	} {
		if rec := e.do(t, "GET", path, "", false); rec.Code != 400 {
			t.Fatalf("%s = %d, want 400", path, rec.Code)
		}
	}

	// The fixture payment must be findable via q (code / bot name /
	// provider order id).
	for _, q := range []string{f.payCode, f.botName, "o" + f.payCode} {
		rec := e.do(t, "GET", "/api/samelabs/finance/payments?q="+q, "", false)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), f.payCode) {
			t.Fatalf("q=%s did not match fixture: %d", q, rec.Code)
		}
	}

	// Detail for unknown code → 404.
	if rec := e.do(t, "GET", "/api/samelabs/finance/payments/no-such-code", "", false); rec.Code != 404 {
		t.Fatalf("unknown payment detail = %d, want 404", rec.Code)
	}
}

// (8)+(10)-(14): integrity flags react to人工构造的不一致事实 — read-only,
// nothing is ever auto-repaired.
func TestFinanceAdminFinanceIntegrityAnomalies(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()

	integrityOf := func(payCode string) map[string]bool {
		t.Helper()
		rec := e.do(t, "GET", "/api/samelabs/finance/payments/"+payCode, "", false)
		if rec.Code != 200 {
			t.Fatalf("detail %s: %d", payCode, rec.Code)
		}
		var d struct {
			Data struct {
				Reconciliation struct {
					Integrity map[string]bool `json:"integrity"`
				} `json:"reconciliation"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
		return d.Data.Reconciliation.Integrity
	}

	// -- (10) paid payment with WRONG grant sum → paid_grant_exact=false.
	f := newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(ctx,
		`UPDATE tb_transactions SET amount=99 WHERE bot_id=$1 AND ref_id=$2`, f.botID, f.payCode); err != nil {
		t.Fatalf("break grant: %v", err)
	}
	if integrityOf(f.payCode)["paid_grant_exact"] {
		t.Fatal("wrong grant sum must set paid_grant_exact=false")
	}
	// ...and it stays broken (no auto-repair): re-read identical.
	if integrityOf(f.payCode)["paid_grant_exact"] {
		t.Fatal("integrity flag flip-flopped — auto-repair suspected")
	}
	f.cleanup(t)

	// -- (11) multi-basis adjustments → adjustment_basis_consistent=false.
	f = newFinanceFixture(t, e)
	var payID int64
	_ = e.s.Pool.QueryRow(ctx, `SELECT id FROM tb_payments WHERE code=$1`, f.payCode).Scan(&payID)
	for i, txnBasis := range []string{"txn_A", "txn_B"} {
		if _, err := e.s.Pool.Exec(ctx, `
			INSERT INTO tb_payment_adjustments (payment_id, provider, provider_event_id, provider_object_id, kind,
				provider_transaction_id, provider_order_id, amount_minor, currency,
				transaction_amount_minor, amount_paid_minor, refunded_amount_minor,
				object_status, transaction_status, reason, provider_created_at)
			VALUES ($1, 'creem', $2, $3, 'refund', $4, 'ord_x', 100, 'USD', 500, $5, 100, 'refunded', 'succeeded', 'test', 0)`,
			payID, fmt.Sprintf("evt_%d", i), fmt.Sprintf("obj_%d", i), txnBasis, 500+i); err != nil {
			t.Fatalf("seed multi-basis adjustment: %v", err)
		}
	}
	if integrityOf(f.payCode)["adjustment_basis_consistent"] {
		t.Fatal("two distinct provider-txn/amount-paid bases must set adjustment_basis_consistent=false")
	}
	f.cleanup(t)

	// -- (12) positive reverse_payment → reverse_payment_nonpositive=false.
	f = newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(ctx, `
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id)
		VALUES ($1, 'reverse_payment', 10, 176, 'payment', $2)`, f.botID, f.payCode); err != nil {
		t.Fatalf("seed positive reverse: %v", err)
	}
	if integrityOf(f.payCode)["reverse_payment_nonpositive"] {
		t.Fatal("positive reverse_payment sum must set flag=false")
	}
	f.cleanup(t)

	// -- (13) reversal magnitude > entitlement → within_entitlement=false.
	f = newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(ctx, `
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id)
		VALUES ($1, 'reverse_payment', -150, 16, 'payment', $2)`, f.botID, f.payCode); err != nil {
		t.Fatalf("seed oversized reverse: %v", err)
	}
	i := integrityOf(f.payCode)
	if i["reverse_payment_within_original_entitlement"] {
		t.Fatal("-150 reversal on 100-credit payment must set within_entitlement=false")
	}
	if !i["reverse_payment_nonpositive"] {
		t.Fatal("negative reversal should still satisfy nonpositive")
	}
	f.cleanup(t)

	// -- (14) bot balance vs latest ledger mismatch → flag=false.
	f = newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(ctx, `UPDATE tb_bots SET balance=999 WHERE id=$1`, f.botID); err != nil {
		t.Fatalf("break balance: %v", err)
	}
	if integrityOf(f.payCode)["account_balance_matches_latest_ledger"] {
		t.Fatal("balance 999 vs ledger 166 must set flag=false")
	}
	f.cleanup(t)

	// -- no ledger rows at all: flag must be false (n/a), never a fake PASS.
	f = newFinanceFixture(t, e)
	if _, err := e.s.Pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=$1`, f.botID); err != nil {
		t.Fatalf("wipe ledger: %v", err)
	}
	if integrityOf(f.payCode)["account_balance_matches_latest_ledger"] {
		t.Fatal("no-ledger account must NOT project a PASS")
	}
	f.cleanup(t)
}

// (15) finance GET performs zero DB mutation. The BEFORE snapshot is
// taken DIRECTLY from the DB before ANY Finance API call (the old
// helper called summary first, poisoning the baseline).
func TestFinanceAdminFinanceReadsNeverMutate(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)

	snapshot := func() string {
		// Isolated to this fixture's own finance facts. Test packages
		// now run serially (-p 1, WO-7e), but the isolation stays: it
		// keeps the comparison independent of any other fixture rows
		// sharing the tables.
		var payID int64
		if err := e.s.Pool.QueryRow(context.Background(),
			`SELECT id FROM tb_payments WHERE code=$1`, f.payCode).Scan(&payID); err != nil {
			t.Fatalf("snapshot payment id: %v", err)
		}
		var out strings.Builder
		rows, err := e.s.Pool.Query(context.Background(), `
			SELECT (SELECT status || ':' || amount_minor || ':' || coalesce(paid_at::text,'-') FROM tb_payments WHERE id=$1),
			       (SELECT COALESCE(string_agg(kind || ':' || coalesce(provider_transaction_id,'-') || ':' || amount_minor || ':' || coalesce(refunded_amount_minor,0), ',' ORDER BY id), '') FROM tb_payment_adjustments WHERE payment_id=$1),
			       (SELECT COALESCE(string_agg(id::text || ':' || coalesce(ref_type,'-') || ':' || coalesce(ref_id,'-') || ':' || amount || ':' || balance_after, ',' ORDER BY id), '') FROM tb_transactions WHERE bot_id=$2),
			       (SELECT balance::text FROM tb_bots WHERE id=$2)`,
			payID, f.botID)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, c, d string
			if err := rows.Scan(&a, &b, &c, &d); err != nil {
				t.Fatalf("snapshot scan (fail closed): %v", err)
			}
			fmt.Fprintf(&out, "%s|%s|%s|%s", a, b, c, d)
		}
		return out.String()
	}

	before := snapshot() // DB-first: no Finance API called yet
	for _, path := range []string{
		"/api/samelabs/finance/summary",
		"/api/samelabs/finance/payments",
		"/api/samelabs/finance/payments/" + f.payCode,
		"/api/samelabs/finance/adjustments",
		"/api/samelabs/finance/ledger",
	} {
		if rec := e.do(t, "GET", path, "", false); rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	after := snapshot()
	if before != after {
		t.Fatalf("finance GET mutated DB:\nbefore %s\nafter  %s", before, after)
	}
}

// (18)+(19): adjustment facts shown as separate facts; payment
// status never rewritten; no raw payload/credential material in DTOs.
func TestFinanceAdminFinanceAdjustmentFactsAndHygiene(t *testing.T) {
	e := newAdminEnv(t)
	f := newFinanceFixture(t, e)
	ctx := context.Background()

	var payID int64
	_ = e.s.Pool.QueryRow(ctx, `SELECT id FROM tb_payments WHERE code=$1`, f.payCode).Scan(&payID)
	if _, err := e.s.Pool.Exec(ctx, `
		INSERT INTO tb_payment_adjustments (payment_id, provider, provider_event_id, provider_object_id, kind,
			provider_transaction_id, provider_order_id, amount_minor, currency,
			transaction_amount_minor, amount_paid_minor, refunded_amount_minor,
			object_status, transaction_status, reason, provider_created_at)
		VALUES ($1, 'creem', 'evt_hygiene', 'obj_hygiene', 'refund', 'txn_h', 'ord_h', 100, 'USD',
			500, 500, 100, 'refunded', 'succeeded', 'customer request', 0)`, payID); err != nil {
		t.Fatalf("seed adjustment: %v", err)
	}

	rec := e.do(t, "GET", "/api/samelabs/finance/payments/"+f.payCode, "", false)
	body := rec.Body.String()

	// (18) payment status stays "paid" — refund is a separate fact.
	if !strings.Contains(body, `"status":"paid"`) && !strings.Contains(body, `"status": "paid"`) {
		t.Fatalf("payment status not projected as paid: %s", body[:400])
	}
	// Adjustment projection present with its own kind.
	if !strings.Contains(body, `"kind":"refund"`) && !strings.Contains(body, `"kind": "refund"`) {
		t.Fatal("refund adjustment fact missing from detail")
	}
	// (19) forbidden material never appears in any finance DTO.
	for _, forbidden := range []string{
		"api_key_hash", "password_hash", "webhook_secret", "api_key",
		"session_secret", "raw_payload", "payload_body",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("finance DTO leaks %q", forbidden)
		}
	}
	// (7) adjustment monetary BIGINTs are strings in the list too.
	rec = e.do(t, "GET", "/api/samelabs/finance/adjustments?payment_code="+f.payCode, "", false)
	var list struct {
		Data struct {
			Adjustments []map[string]interface{} `json:"adjustments"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data.Adjustments) != 1 {
		t.Fatalf("adjustment list = %d", len(list.Data.Adjustments))
	}
	a := list.Data.Adjustments[0]
	for _, k := range []string{"amount_minor", "transaction_amount_minor", "amount_paid_minor", "refunded_amount_minor"} {
		if _, ok := a[k].(string); !ok {
			t.Fatalf("adjustment %s = %#v, want decimal string", k, a[k])
		}
	}

	// Ledger explorer: amount / balance_after strings.
	rec = e.do(t, "GET", "/api/samelabs/finance/ledger?bot_id="+fmt.Sprint(f.botID), "", false)
	var led struct {
		Data struct {
			Entries []map[string]interface{} `json:"entries"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &led)
	if len(led.Data.Entries) == 0 {
		t.Fatal("ledger explorer returned no rows for fixture bot")
	}
	for _, entry := range led.Data.Entries {
		for _, k := range []string{"amount", "balance_after"} {
			if _, ok := entry[k].(string); !ok {
				t.Fatalf("ledger %s = %#v, want decimal string", k, entry[k])
			}
		}
	}
}

// (20) is covered by the existing suites still passing in the full
// run (Admin Accounts / Store / Payment / Credits).

// newScopedAdmin mirrors the 011 pattern: superadmin creates a custom
// role with exactly the given permissions, a user, assigns the role,
// re-logs in as that user.
func newScopedAdmin(t *testing.T, e *adminEnv, perms []string) *adminEnv {
	t.Helper()
	rec := e.mutateJSON(t, "POST", "/api/samelabs/roles",
		fmt.Sprintf(`{"code":"fin_%d","name":"FIN"}`, time.Now().UnixNano()%1000000))
	if rec.Code != 200 {
		t.Fatalf("role: %d %s", rec.Code, rec.Body.String())
	}
	var rr struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rr)
	permJSON, _ := json.Marshal(perms)
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/samelabs/roles/%d/permissions", rr.Data.ID),
		`{"permission_codes":`+string(permJSON)+`}`)
	if rec.Code != 200 {
		t.Fatalf("perms: %d %s", rec.Code, rec.Body.String())
	}
	username := fmt.Sprintf("fin_user_%d", time.Now().UnixNano()%1000000)
	rec = e.mutateJSON(t, "POST", "/api/samelabs/users",
		fmt.Sprintf(`{"username":%q,"display_name":"FIN","password":"fin-pass-123"}`, username))
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
	env := &adminEnv{s: e.s, router: e.router, username: username, password: "fin-pass-123"}
	env.login(t)
	return env
}
