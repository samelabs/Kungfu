package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/security"
)

// Payment settings live in the database: saved through the admin API,
// sealed at rest, and picked up by the payment runtime without restart.
func TestPaymentSettingsSavedInDBDriveRuntime(t *testing.T) {
	e := newB12Env(t)
	ctx := context.Background()
	key, _ := security.ParseSecretBoxKey(strings.Repeat("5e", 32))
	e.s.secretBox, _ = security.NewSecretBox(key)
	_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_payment_provider_settings`)
	t.Cleanup(func() { _, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_payment_provider_settings`) })

	type settingsResp struct {
		Data struct {
			Configured   bool   `json:"configured"`
			APIKeyMasked string `json:"api_key_masked"`
		} `json:"data"`
	}
	decode := func(body string) settingsResp {
		var r settingsResp
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		return r
	}
	get := func() string {
		req := httptest.NewRequest("GET", "/api/samelabs/settings/payment", nil)
		req.AddCookie(e.cookie)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if decode(get()).Data.Configured {
		t.Fatal("fresh DB should be unconfigured")
	}

	// Unconfigured: webhook is 503, packages are unavailable.
	wh := func(secret string) int {
		body := []byte(`{"id":"evt_settings_probe","eventType":"unknown.event","object":{}}`)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req := httptest.NewRequest("POST", "/api/webhooks/creem", strings.NewReader(string(body)))
		req.Header.Set("creem-signature", hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := wh("whsec_db_1234"); code != 503 {
		t.Fatalf("unconfigured webhook = %d, want 503", code)
	}

	// Fractional credits and unknown fields are rejected at the boundary.
	for _, bad := range []string{
		`{"enabled":true,"mode":"test","success_url":"https://kungfu.md/x","packages":[{"code":"s","product_id":"p","credits":10.5}],"api_key":"k","webhook_secret":"w"}`,
		`{"enabled":true,"mode":"test","success_url":"https://kungfu.md/x","packages":[],"api_key":"k","webhook_secret":"w","extra":1}`,
	} {
		if rec := e.mutateJSON(t, "PUT", "/api/samelabs/settings/payment", bad); rec.Code != 400 {
			t.Fatalf("bad body accepted: %d %s", rec.Code, rec.Body.String())
		}
	}

	// Save with checkout OFF: webhooks for existing payments must still
	// verify against the DB secret.
	rec := e.mutateJSON(t, "PUT", "/api/samelabs/settings/payment",
		`{"enabled":false,"mode":"test","success_url":"https://kungfu.md/owner/credits","packages":[{"code":"starter","product_id":"prod_x","credits":100}],"api_key":"creem_test_key_abcd","webhook_secret":"whsec_db_1234"}`)
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "creem_test_key_abcd") || strings.Contains(body, "whsec_db_1234") {
		t.Fatal("save response echoes a secret")
	}
	if decode(body).Data.APIKeyMasked != "••••abcd" {
		t.Fatalf("masked key missing: %s", body)
	}
	if code := wh("wrong-secret"); code != 401 {
		t.Fatalf("wrong signature = %d, want 401", code)
	}
	if code := wh("whsec_db_1234"); code == 401 || code == 503 {
		t.Fatalf("webhook with DB secret = %d, want accepted past signature", code)
	}
	if st := e.s.creemSettings(ctx); st == nil || st.CheckoutEnabled {
		t.Fatalf("checkout should be off: %+v", st)
	}
	if rt := e.s.creemCheckoutRuntime(ctx); rt != nil {
		t.Fatal("checkout runtime available while switched off")
	}

	// Switching checkout on takes effect immediately (cache invalidated).
	rec = e.mutateJSON(t, "PUT", "/api/samelabs/settings/payment",
		`{"enabled":true,"mode":"test","success_url":"https://kungfu.md/owner/credits","packages":[{"code":"starter","product_id":"prod_x","credits":100}]}`)
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	rt := e.s.creemCheckoutRuntime(ctx)
	if rt == nil || rt.Packages["starter"].Credits != 100 || rt.Mode != "test" {
		t.Fatalf("runtime after enable = %+v", rt)
	}

	// Without CSRF the save is refused.
	req := httptest.NewRequest("PUT", "/api/samelabs/settings/payment", strings.NewReader(`{}`))
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("no-CSRF save = %d", rr.Code)
	}
}
