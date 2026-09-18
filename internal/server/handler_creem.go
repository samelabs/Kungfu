package server

// Creem fixed-package payment runtime endpoints:
//   POST /api/owner/payments/checkout  (owner session; package-code-only input)
//   POST /api/webhooks/creem           (signature-authenticated, public)
//   GET  /api/owner/payments/{code}    (owner session; SQL ownership scope)
//
// No UI in this round. The success redirect never grants credits — the
// only economic trigger is a signature-valid checkout.completed webhook
// reconciled against the payment snapshot through Payment Core.

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/payment"
)

// creemRuntime resolves the validated provider runtime; nil when disabled.
func (s *Server) creemRuntime() *payment.CreemRuntime {
	if s.Config == nil || !s.Config.CreemEnabled() {
		return nil
	}
	base := s.Config.CreemAPIBase()
	if s.creemBaseOverride != "" {
		base = s.creemBaseOverride // test injection only
	}
	packages := make(map[string]payment.CreemPackageSpec, len(s.Config.CreemPackages))
	for code, pkg := range s.Config.CreemPackages {
		packages[code] = payment.CreemPackageSpec(pkg)
	}
	return &payment.CreemRuntime{
		Client:     payment.NewCreemClient(payment.CreemConfig{APIBase: base, APIKey: s.Config.CreemAPIKey}),
		Mode:       s.Config.CreemMode,
		SuccessURL: s.Config.CreemSuccessURL,
		Packages:   packages,
	}
}

// handleOwnerPaymentCheckout: POST /api/owner/payments/checkout
// Body: {"package": "starter"} — the ONLY client-controlled fact. Any
// other client-sent field (units, amount_minor, credits, product_id,
// custom_price, success_url, metadata) is structurally ignored: the JSON
// struct has no field for it, so it can never reach the provider or the
// payment row.
func (s *Server) handleOwnerPaymentCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	rt := s.creemRuntime()
	if rt == nil {
		handleAppError(w, errors.New(503, "PAYMENT_NOT_CONFIGURED", "Payment is not configured on this server"))
		return
	}

	// R2.2: strict bounded read — 64 KiB cap, oversize/read failure
	// fail closed (existing INVALID_JSON contract).
	body, err := readBoundedRequestBody(r, 1<<16)
	if err != nil {
		InvalidJSON(w, "Could not read request body")
		return
	}
	var input struct {
		Package string `json:"package"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		InvalidJSON(w, "Request body must be valid JSON object")
		return
	}
	if input.Package == "" {
		handleAppError(w, errors.New(400, "MISSING_FIELD", "Missing required field: package"))
		return
	}

	res, err := payment.StartCreemCheckout(r.Context(), s.Pool, rt, bot.ID, input.Package)
	if err != nil {
		handleAppError(w, err)
		return
	}

	SuccessResponse(w, map[string]interface{}{
		"payment": map[string]interface{}{
			"code":         res.Payment.Code,
			"status":       res.Payment.Status,
			"amount_minor": res.Payment.AmountMinor,
			"currency":     res.Payment.Currency,
			"credits":      res.Payment.Credits,
		},
		"checkout_url": res.CheckoutURL,
	}, "Checkout created")
}

// handleCreemWebhook: POST /api/webhooks/creem — public endpoint whose
// ONLY authentication is the creem-signature HMAC over the raw body.
// No IP allowlist (Creem has no static source IPs); no session; no API key.
//
// checkout.completed → snapshot reconciliation → Payment Core grant.
// Everything else → 200 acknowledged with a clear log, zero mutation.
// refund.created / dispute.created are recognized and logged as warnings:
// the refund economic policy is explicitly undecided (go-live blocker)
// and this handler must never mutate Credits for them.
func (s *Server) handleCreemWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	if s.Config == nil || s.Config.CreemWebhookSecret == "" {
		ErrorResponse(w, http.StatusServiceUnavailable, "PAYMENT_NOT_CONFIGURED", "Payment is not configured on this server", nil)
		return
	}

	// Raw body FIRST: signature is HMAC over the exact bytes. No decode
	// → re-encode → sign, ever.
	// R2.2: strict bounded read — 1 MiB cap; oversize fails closed with
	// the existing INVALID_BODY contract BEFORE signature verification,
	// JSON decode, or any DB/domain mutation.
	raw, err := readBoundedRequestBody(r, 1<<20)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, "INVALID_BODY", "Could not read request body", nil)
		return
	}

	sig := r.Header.Get("creem-signature")
	if !payment.VerifyCreemWebhookSignature(raw, s.Config.CreemWebhookSecret, sig) {
		// Missing/malformed/mismatched signature: 401, zero DB mutation.
		ErrorResponse(w, http.StatusUnauthorized, "INVALID_SIGNATURE", "Webhook signature verification failed", nil)
		return
	}

	var ev payment.CreemWebhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.ID == "" || ev.EventType == "" {
		ErrorResponse(w, http.StatusBadRequest, "INVALID_EVENT", "Webhook payload is not a valid Creem event", nil)
		return
	}

	switch {
	case ev.EventType == "checkout.completed":
		rt := s.creemRuntime()
		if rt == nil {
			ErrorResponse(w, http.StatusServiceUnavailable, "PAYMENT_NOT_CONFIGURED", "Payment is not configured on this server", nil)
			return
		}
		if err := payment.ReconcileCreemCompletion(r.Context(), s.Pool, rt, &ev); err != nil {
			// Reconciliation failure: log and 4xx so Creem retries with the
			// same facts; the payment state is unchanged (pending or already
			// paid). Never grant on a failed check.
			log.Printf("creem webhook reconciliation failed: event=%s err=%v", ev.ID, err)
			ErrorResponse(w, http.StatusBadRequest, "RECONCILIATION_FAILED", "Webhook facts did not reconcile", nil)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))

	case ev.EventType == "refund.created" || ev.EventType == "dispute.created":
		// Adjustment fact + authoritative Credits reversal, atomically:
		// reconcile against the paid payment, persist the idempotent
		// tb_payment_adjustments row, and advance the cumulative
		// reverse_payment ledger by the provider refunded-amount ratio
		// (may drive the balance negative). Ordinary spends still cannot
		// cross zero; only this reversal path can.
		if err := payment.HandleCreemAdjustmentEvent(r.Context(), s.Pool, &ev); err != nil {
			log.Printf("creem webhook %s reconciliation failed: event=%s err=%v", ev.EventType, ev.ID, err)
			ErrorResponse(w, http.StatusBadRequest, "RECONCILIATION_FAILED", "Adjustment facts did not reconcile", nil)
			return
		}
		log.Printf("creem webhook %s recorded as payment adjustment fact with authoritative reversal: event=%s", ev.EventType, ev.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))

	default:
		log.Printf("creem webhook ignored eventType=%s event=%s", ev.EventType, ev.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"ignored":true}`))
	}
}

// handleOwnerPaymentGet: GET /api/owner/payments/{code} — ownership is
// scoped inside the SQL (WHERE code = $1 AND bot_id = $2).
func (s *Server) handleOwnerPaymentGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := chi.URLParam(r, "code")

	p, err := payment.GetPaymentForBot(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}

	var paidAt interface{}
	if p.PaidAt != nil {
		paidAt = p.PaidAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	SuccessResponse(w, map[string]interface{}{
		"payment": map[string]interface{}{
			"code":         p.Code,
			"provider":     p.Provider,
			"status":       p.Status,
			"amount_minor": p.AmountMinor,
			"currency":     p.Currency,
			"credits":      p.Credits,
			"created_at":   p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"paid_at":      paidAt,
		},
	}, "")
}

// handleOwnerPaymentPackages: GET /api/owner/payments/packages — the
// read-only fixed-package catalog for the Owner Credits page. Owner
// session required; disabled runtime → 503 PAYMENT_NOT_CONFIGURED; any
// invalid provider product → whole-request failure (fail closed).
// DTO never includes product_id or any provider secret.
func (s *Server) handleOwnerPaymentPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	if _, err := s.requireOwnerAuth(r); err != nil {
		handleAppError(w, err)
		return
	}
	rt := s.creemRuntime()
	if rt == nil {
		handleAppError(w, errors.New(503, "PAYMENT_NOT_CONFIGURED", "Payment is not configured on this server"))
		return
	}
	pkgs, err := payment.ListCreemPackages(r.Context(), rt)
	if err != nil {
		log.Printf("owner payment packages failed: %v", err)
		handleAppError(w, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Could not load the payment catalog"))
		return
	}
	SuccessResponse(w, map[string]interface{}{"packages": pkgs}, "")
}
