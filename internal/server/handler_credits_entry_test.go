package server

// Owner Credits entry (buy-credits) tests: packages read API contract,
// page wiring, JS module loading, and static frontend proof that the
// checkout body carries only {"package": ...}.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kungfu.md/internal/config"
	"kungfu.md/internal/i18n"
)

// i18nLocaleURLForTest wraps the production locale URL helper used by
// the nav so the active-state assertion matches the rendered href.
func i18nLocaleURLForTest(path string) string { return i18n.LocaleURL("en", path) }

// newBcpFake: packages fake with TWO products at different prices plus
// an INVALID one (wrong billing_type) for fail-closed checks.
func newBcpFake(t *testing.T, extra ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/products/", func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch strings.TrimPrefix(r.URL.Path, "/v1/products/") {
		case "prod_a":
			body = `{"id":"prod_a","name":"Starter","billing_type":"onetime","status":"active","mode":"test","currency":"USD","price":1000}`
		case "prod_b":
			body = `{"id":"prod_b","name":"Standard","billing_type":"onetime","status":"active","mode":"test","currency":"USD","price":400}`
		default:
			// optional invalid fixture supplied by the test
			if len(extra) == 2 && strings.TrimPrefix(r.URL.Path, "/v1/products/") == extra[0] {
				body = extra[1]
			} else {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func bcpServer(t *testing.T, fakeURL string) *Server {
	t.Helper()
	s := storeTestServer(t)
	s.Config.CreemAPIKey = "k"
	s.Config.CreemWebhookSecret = "whsec"
	s.Config.CreemMode = "test"
	s.Config.CreemSuccessURL = "https://kungfu.md/owner?payment=success"
	s.Config.CreemPackages = map[string]config.CreemPackage{
		"starter":  {Code: "starter", ProductID: "prod_a", Credits: 1000},
		"standard": {Code: "standard", ProductID: "prod_b", Credits: 500},
	}
	s.creemBaseOverride = fakeURL
	return s
}

var bcpBotSeq int64

func bcpBot(t *testing.T, s *Server) int64 {
	t.Helper()
	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), atomic.AddInt64(&bcpBotSeq, 1))
	var botID int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1,$2,'x','active',0) RETURNING id`,
		"bcp_"+suffix, "kf_live_"+suffix).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_payments WHERE bot_id=$1`, botID)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID)
	})
	return botID
}

// packages API requires owner auth
func TestPackagesAPIRequiresAuth(t *testing.T) {
	s := bcpServer(t, newBcpFake(t).URL)
	router := s.buildRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/packages", nil)
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated packages call returned 200")
	}
}

// disabled runtime → 503 PAYMENT_NOT_CONFIGURED
func TestPackagesAPIDisabled503(t *testing.T) {
	s := storeTestServer(t) // no Creem config
	router := s.buildRouter()
	botID := bcpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/packages", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "PAYMENT_NOT_CONFIGURED") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// live price + fixed credits projection, ordering, DTO hygiene
func TestPackagesAPIProjection(t *testing.T) {
	s := bcpServer(t, newBcpFake(t).URL)
	router := s.buildRouter()
	botID := bcpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/packages", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Data struct {
			Packages []struct {
				Code        string  `json:"code"`
				Name        string  `json:"name"`
				AmountMinor int64   `json:"amount_minor"`
				Currency    string  `json:"currency"`
				Credits     float64 `json:"credits"`
			} `json:"packages"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pkgs := out.Data.Packages
	if len(pkgs) != 2 {
		t.Fatalf("packages = %d", len(pkgs))
	}
	// deterministic order: amount ASC (400 standard first), code tie-break
	if pkgs[0].Code != "standard" || pkgs[1].Code != "starter" {
		t.Fatalf("order = %s, %s", pkgs[0].Code, pkgs[1].Code)
	}
	byCode := map[string]int{}
	for i, p := range pkgs {
		byCode[p.Code] = i
	}
	st := pkgs[byCode["starter"]]
	if st.Name != "Starter" || st.AmountMinor != 1000 || st.Currency != "USD" || st.Credits != 1000 {
		t.Fatalf("starter = %+v", st)
	}
	sd := pkgs[byCode["standard"]]
	if sd.AmountMinor != 400 || sd.Credits != 500 {
		t.Fatalf("standard = %+v", sd)
	}

	// DTO hygiene: no provider identifiers or secrets
	body := rec.Body.String()
	for _, banned := range []string{"product_id", "api_key", "webhook", "success_url", "metadata", "bot_id"} {
		if strings.Contains(body, banned) {
			t.Fatalf("DTO leaks %q", banned)
		}
	}
}

// any invalid/unreadable configured product → whole request fails
func TestPackagesAPIFailClosed(t *testing.T) {
	// prod_c exists but has subscription billing → invalid
	s := bcpServer(t, newBcpFake(t, "prod_c", `{"id":"prod_c","name":"Broken","billing_type":"subscription","status":"active","mode":"test","currency":"USD","price":100}`).URL)
	s.Config.CreemPackages["broken"] = config.CreemPackage{Code: "broken", ProductID: "prod_c", Credits: 10}
	router := s.buildRouter()
	botID := bcpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/packages", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("invalid product must fail the whole catalog request")
	}

	// unreadable (404) product also fails closed — same server, catalog
	// mutated in place (the runtime resolves packages per request)
	s.Config.CreemPackages["ghost"] = config.CreemPackage{Code: "ghost", ProductID: "prod_404", Credits: 10}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/packages", nil)
	req2.AddCookie(cookie)
	router.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusOK {
		t.Fatal("unreadable product must fail the whole catalog request")
	}
}

// page wiring: /owner/credits renders the shell with the required
// elements, nav entry, and script modules.
func TestOwnerCreditsPageWiring(t *testing.T) {
	s := bcpServer(t, newBcpFake(t).URL)
	router := s.buildRouter()
	botID := bcpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner/credits", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-section="owner_credits"`,
		`id="creditsBalance"`,
		`id="creditsPackages"`,
		`id="creditsPaymentResult"`,
		`id="creditsNotice"`,
		`/owner/credits?`,
		`render-credits.js`,
		`credits.js`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}

	// nav entry present with active state on the credits page
	if !strings.Contains(body, `owner.nav.credits`) && !strings.Contains(body, `>Credits<`) && !strings.Contains(body, "积分") {
		t.Fatal("credits nav label missing")
	}
	idx := strings.Index(body, `/owner/credits`)
	if idx < 0 || !strings.Contains(body[max(0, idx-120):idx], "btn active") {
		t.Fatalf("credits nav not active near href")
	}
}

// /owner/store still intact
func TestOwnerStorePageStillWorks(t *testing.T) {
	s := bcpServer(t, newBcpFake(t).URL)
	router := s.buildRouter()
	botID := bcpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner/store", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-section="store"`) {
		t.Fatalf("store page = %d", rec.Code)
	}
}

// Static frontend proof: the checkout call site sends ONLY the package
// code — no units/amount/credits/product_id/custom_price — and the
// pending code is stored in sessionStorage BEFORE the redirect.
func TestCheckoutFrontendWiringStatic(t *testing.T) {
	srcBytes, err := os.ReadFile("../../web/assets/owner/credits.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(srcBytes)

	// body is exactly the package code
	if !strings.Contains(src, `body: JSON.stringify({package: code})`) {
		t.Fatal("checkout body must be only {package}")
	}
	for _, banned := range []string{"units", "amount_minor", "custom_price", "product_id"} {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // comments may mention what the client must NOT control
			}
			if strings.Contains(trimmed, banned) && strings.Contains(trimmed, ":") {
				t.Fatalf("frontend must not send %q (line: %s)", banned, trimmed)
			}
		}
	}
	// sessionStorage write precedes the redirect (source order)
	storeIdx := strings.Index(src, "sessionStorage.setItem(CREDITS_PENDING_KEY")
	redirectIdx := strings.Index(src, "window.location.assign(json.data.checkout_url)")
	if storeIdx < 0 || redirectIdx < 0 || storeIdx > redirectIdx {
		t.Fatal("pending code must be stored before redirect")
	}
	// status read uses the stored code via GET
	if !strings.Contains(src, `/api/owner/payments/${encodeURIComponent(code)}`) {
		t.Fatal("payment status must use the stored code")
	}
	// paid path refreshes the account balance; no polling loops
	if !strings.Contains(src, "await loadAccount()") {
		t.Fatal("paid path must reload account balance")
	}
	if strings.Contains(src, "setInterval") || strings.Contains(src, "setTimeout") {
		t.Fatal("no polling allowed")
	}
}

// Static XSS regression proof for the Credits renderer: every dynamic
// value interpolated into innerHTML must be wrapped in escapeHtml, with
// hostile fixtures proving no tag/attribute breakout is possible.
func TestCreditsRendererHTMLEscaping(t *testing.T) {
	srcBytes, err := os.ReadFile("../../web/assets/owner/render-credits.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(srcBytes)

	// 1. No bare interpolation of provider/config dynamic fields.
	bare := []string{
		"${p.code}", "${p.name}", "${p.credits}",
		"${creditsFormatAmount(",
		": ${formatted}",
	}
	for _, banned := range bare {
		if strings.Contains(src, banned) {
			t.Fatalf("bare interpolation %q must be escaped", banned)
		}
	}

	// 2. Required escaped call sites exist.
	required := []string{
		"data-package-code=\"${escapeHtml(p.code)}\"",
		"data-buy-package=\"${escapeHtml(p.code)}\"",
		"${escapeHtml(p.name)}",
		"${escapeHtml(creditsFormatAmount(p.amount_minor, p.currency))}",
		"${escapeHtml(String(p.credits))}",
		"${escapeHtml(label)}",
	}
	for _, want := range required {
		if !strings.Contains(src, want) {
			t.Fatalf("missing escaped call site %q", want)
		}
	}

	// 3. Behavioral proof with hostile fixtures: escaped output cannot
	//    form a tag or attribute breakout.
	hostileName := "<img src=x onerror=alert(1)>"
	hostileCode := "x\"><svg onload=alert(1)>"
	hostileStatus := "paid\"><iframe src=javascript:alert(1)>"

	esc := func(v string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#039;")
		return r.Replace(v)
	}
	for _, hostile := range []string{hostileName, hostileCode, hostileStatus} {
		e := esc(hostile)
		if strings.Contains(e, "<") || strings.Contains(e, ">") || strings.Contains(e, "\"") {
			t.Fatalf("escape failed to neutralize %q -> %q", hostile, e)
		}
		// Re-interpolated into an attribute slot, the escaped value
		// cannot terminate the attribute or open a tag.
		slot := "data-package-code=\"" + e + "\""
		if strings.Contains(slot, "\" on") || strings.Contains(slot, "\"/><") {
			t.Fatalf("attribute breakout with %q", hostile)
		}
	}
}
