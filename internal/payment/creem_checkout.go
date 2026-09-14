package payment

// Creem fixed-package checkout flow: the server package config is the
// credits authority, the live Creem product is the fiat price authority,
// and the payment row snapshots BOTH before any provider call. The
// webhook later reconciles against that snapshot — never against the
// current package configuration.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// CreemRuntime carries the resolved, fully-validated provider config:
// the fixed package catalog plus the API client.
type CreemRuntime struct {
	Client     *CreemClient
	Mode       string // "test" | "prod"
	SuccessURL string
	Packages   map[string]CreemPackageSpec
}

// CreemPackageSpec is one fixed package resolved from config (the
// config layer defines CreemPackage; this alias keeps the runtime
// decoupled from config types).
type CreemPackageSpec = struct {
	Code      string  `json:"code"`
	ProductID string  `json:"product_id"`
	Credits   float64 `json:"credits"`
}

// CheckoutResult is the owner-facing outcome of StartCreemCheckout.
type CheckoutResult struct {
	Payment     *model.Payment
	CheckoutURL string
}

// validateCreemProduct enforces the provider-side product contract:
// exact configured id, one-time billing, active, matching mode, positive
// price, valid currency. Creem is the fiat price authority.
func validateCreemProduct(p *CreemProduct, expectedID, mode string) error {
	if p.ID != expectedID {
		return fmt.Errorf("product id mismatch: got %q want %q", p.ID, expectedID)
	}
	if p.BillingType != "onetime" {
		return fmt.Errorf("product billing_type %q, want onetime", p.BillingType)
	}
	if p.Status != "active" {
		return fmt.Errorf("product status %q, want active", p.Status)
	}
	if p.Mode != mode {
		return fmt.Errorf("product mode %q, want %q", p.Mode, mode)
	}
	if p.Price <= 0 {
		return fmt.Errorf("product price %d not positive", p.Price)
	}
	if !currencyPattern.MatchString(p.Currency) {
		return fmt.Errorf("product currency %q invalid", p.Currency)
	}
	return nil
}

// StartCreemCheckout: owner session (bot_id resolved by the handler)
// selects a fixed package by code. Everything else — product, price,
// currency, credits, success URL, metadata — is server-owned.
//
// request_id is the Kungfu payment code — the provider
// correlation/reference key tying a checkout to its local payment row.
//
// Order of operations:
//  1. package lookup: unknown code → 400 INVALID_PACKAGE
//  2. live GetProduct(package.product_id) → validate (fiat authority)
//  3. CreatePendingPayment snapshot: provider_product_id, live price,
//     currency, and the CONFIGURED package credits — the row exists
//     BEFORE any provider call, and the webhook will reconcile against
//     this snapshot even if the package config later changes
//  4. one Creem POST /v1/checkouts (one attempt = one POST; no
//     automatic resubmission)
//  5. verify the response facts, return checkout_url
//
// Failure handling:
//   - definitive Creem rejection (400/401/403/404): FailPayment, provider error
//   - ambiguous failure (network/429/5xx): payment stays pending, 502
//     returned. If Creem actually created the checkout, the later
//     webhook still finds the local payment by request_id and completes
//     it. If it did not, the row remains pending — a known runtime gap
//     (no recovery job). A new checkout call creates a NEW payment; the
//     same code is never resubmitted.
func StartCreemCheckout(ctx context.Context, pool *pg.Pool, rt *CreemRuntime, botID int64, packageCode string) (*CheckoutResult, error) {
	pkg, ok := rt.Packages[packageCode]
	if !ok {
		return nil, errors.New(400, "INVALID_PACKAGE", "Unknown package")
	}

	product, err := rt.Client.GetProduct(ctx, pkg.ProductID)
	if err != nil {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider is not reachable")
	}
	if verr := validateCreemProduct(product, pkg.ProductID, rt.Mode); verr != nil {
		return nil, errors.New(502, "PAYMENT_PRODUCT_INVALID", "Payment product configuration is invalid")
	}

	providerProduct := pkg.ProductID
	p, err := CreatePendingPayment(ctx, pool, botID, PaymentSpec{
		Provider:          "creem",
		ProviderProductID: &providerProduct,
		AmountMinor:       product.Price, // live fiat authority, units=1
		Currency:          product.Currency,
		Credits:           pkg.Credits, // server package authority
	})
	if err != nil {
		return nil, err
	}

	checkout, err := rt.Client.CreateCheckout(ctx, CreateCheckoutInput{
		ProductID:  pkg.ProductID,
		RequestID:  p.Code, // provider correlation/reference key
		Units:      1,      // fixed packages: never a quantity selector
		SuccessURL: rt.SuccessURL,
		Metadata: map[string]string{
			"payment_code": p.Code,
			"bot_id":       strconv.FormatInt(botID, 10),
			"source":       "kungfu_owner",
		},
	})
	if err != nil {
		if _, isDefinitive := err.(*ErrCreemDefinitive); isDefinitive {
			_, _ = FailPayment(ctx, pool, p.Code)
			return nil, errors.New(502, "PAYMENT_PROVIDER_REJECTED", "Payment provider rejected the checkout")
		}
		// Ambiguous: the checkout may exist upstream with our request_id.
		// The local payment stays pending; if Creem actually created and
		// completed the checkout, the checkout.completed webhook finds the
		// payment by request_id and completes it. If it was never created,
		// the pending row remains — a recorded runtime gap (no recovery
		// job). No retry of the same payment code is offered: a new
		// checkout call creates a NEW payment.
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE",
			"Payment provider result is uncertain; the payment remains pending.")
	}

	// Response fact checks — official schema fields are mandatory facts.
	// Even a `completed` status here grants nothing: the only economic
	// trigger remains the signature-valid checkout.completed webhook
	// reconciled through Payment Core.
	if checkout.ID == "" || checkout.CheckoutURL == "" {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned an incomplete checkout")
	}
	if checkout.RequestID != p.Code {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched request id")
	}
	if checkout.Mode != rt.Mode {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched mode")
	}
	if checkout.ProductID != providerProduct {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched product")
	}
	if checkout.Units != 1 {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned mismatched units")
	}
	switch checkout.Status {
	case "pending", "processing", "completed", "expired":
		// official checkout status enum
	default:
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned an invalid checkout state")
	}

	return &CheckoutResult{Payment: p, CheckoutURL: checkout.CheckoutURL}, nil
}

// -- webhook envelope + reconciliation --

// CreemWebhookEvent is the signed envelope Creem delivers.
type CreemWebhookEvent struct {
	ID        string          `json:"id"`
	EventType string          `json:"eventType"` // NOTE: eventType, not event.type
	CreatedAt int64           `json:"created_at"`
	Object    json.RawMessage `json:"object"`
}

// CreemCheckoutObject is the checkout.completed payload.
type CreemCheckoutObject struct {
	ID        string            `json:"id"`
	RequestID string            `json:"request_id"`
	Status    string            `json:"status"`
	OrderID   string            `json:"order_id"`
	Mode      string            `json:"mode"`
	Metadata  map[string]string `json:"metadata"`
	Order     *CreemOrder       `json:"order"`
}

// CreemOrder is the paid order fact inside the checkout object.
type CreemOrder struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Product  string `json:"product"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
	Units    int64  `json:"units"`
}

// ReconcileCreemCompletion performs the full fact check of a signature-
// valid checkout.completed against the PAYMENT SNAPSHOT. The snapshot is
// the authority — the current package configuration is never consulted,
// so a payment created under an old package config completes with its
// original entitlements even after the config changes or the package is
// removed. The lookup key is request_id == payment code; metadata is
// proof of consistency, never an authority for amounts or credits.
func ReconcileCreemCompletion(ctx context.Context, pool *pg.Pool, rt *CreemRuntime, ev *CreemWebhookEvent) error {
	var co CreemCheckoutObject
	if err := json.Unmarshal(ev.Object, &co); err != nil {
		return fmt.Errorf("checkout object decode: %w", err)
	}

	if co.Status != "completed" {
		return fmt.Errorf("checkout.status %q, want completed", co.Status)
	}
	if co.RequestID == "" {
		return fmt.Errorf("checkout.request_id empty")
	}

	p, err := GetPayment(ctx, pool, co.RequestID)
	if err != nil {
		return err // 404 propagates: unknown payment code
	}
	if p.Provider != "creem" {
		return fmt.Errorf("payment provider %q, want creem", p.Provider)
	}
	// Snapshot authority: the persisted provider product fact decides,
	// never the current config.
	if p.ProviderProductID == nil || *p.ProviderProductID == "" {
		return fmt.Errorf("payment %s has no provider product snapshot", p.Code)
	}

	if co.Order == nil || co.Order.ID == "" {
		return fmt.Errorf("order.id empty")
	}
	if co.Order.Status != "paid" {
		return fmt.Errorf("order.status %q, want paid", co.Order.Status)
	}
	if co.Order.Product != *p.ProviderProductID {
		return fmt.Errorf("order.product %q, want payment snapshot %q", co.Order.Product, *p.ProviderProductID)
	}
	if co.Order.Currency != p.Currency {
		return fmt.Errorf("order.currency %q, want %q", co.Order.Currency, p.Currency)
	}
	if co.Order.Amount != p.AmountMinor {
		return fmt.Errorf("order.amount %d, want %d", co.Order.Amount, p.AmountMinor)
	}
	if co.Mode != rt.Mode {
		return fmt.Errorf("checkout.mode %q, want %q", co.Mode, rt.Mode)
	}

	// Metadata consistency proof (never an amount/credits authority).
	if co.Metadata["payment_code"] != p.Code {
		return fmt.Errorf("metadata.payment_code mismatch")
	}
	if co.Metadata["bot_id"] != strconv.FormatInt(p.BotID, 10) {
		return fmt.Errorf("metadata.bot_id mismatch")
	}

	// Bind the provider order BEFORE any grant.
	if err := BindProviderOrder(ctx, pool, "creem", p.Code, co.Order.ID); err != nil {
		return err
	}

	// The only grant path: Payment Core → Credits. p.Credits is the
	// creation-time snapshot — exactly what gets granted.
	_, _, err = CompletePayment(ctx, pool, p.Code)
	return err
}
