package payment

// Creem checkout flow: server-owned product/price authority, units-only
// client input, payment row created BEFORE the provider call, and full
// webhook reconciliation before any grant.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// CreemRuntime carries the resolved, fully-validated provider config.
type CreemRuntime struct {
	Client         *CreemClient
	ProductID      string
	CreditsPerUnit int64
	Mode           string // "test" | "prod"
	SuccessURL     string
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

// StartCreemCheckout: owner session (bot_id already resolved by the
// handler) requests `units` of the configured product. Amount and credits
// are server-computed; the client cannot influence price, currency,
// credits, product, or success URL.
//
// request_id is the Kungfu payment code — the provider
// correlation/reference key that ties a Creem checkout to its local
// payment row.
//
// Order of operations:
//  1. live GetProduct → validate (price authority + mode/status checks)
//  2. entitlement: amount_minor = product.price × units,
//     credits = CREEM_CREDITS_PER_UNIT × units
//  3. CreatePendingPayment (provider=creem, snapshot) — the payment row
//     exists BEFORE any provider call, so a webhook for it can never
//     reference a missing payment
//  4. one Creem POST /v1/checkouts (one attempt = one POST; no automatic
//     retry — see below)
//  5. verify the response facts, return checkout_url
//
// Failure handling:
//   - definitive Creem rejection (400/401/403/404): FailPayment, provider error
//   - ambiguous failure (network/429/5xx): payment stays pending, 502
//     returned. If Creem actually created the checkout, the later
//     webhook still finds the local payment by request_id and completes
//     it. If it did not, the row remains pending — a known runtime gap
//     (no recovery job in this round).
func StartCreemCheckout(ctx context.Context, pool *pg.Pool, rt *CreemRuntime, botID int64, units int64) (*CheckoutResult, error) {
	if units <= 0 || units > 1000 {
		return nil, errors.New(400, "INVALID_UNITS", "units must be a positive integer")
	}

	product, err := rt.Client.GetProduct(ctx, rt.ProductID)
	if err != nil {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider is not reachable")
	}
	if verr := validateCreemProduct(product, rt.ProductID, rt.Mode); verr != nil {
		return nil, errors.New(502, "PAYMENT_PRODUCT_INVALID", "Payment product configuration is invalid")
	}

	amountMinor := product.Price * units
	credits := float64(rt.CreditsPerUnit * units)

	p, err := CreatePendingPayment(ctx, pool, botID, PaymentSpec{
		Provider:    "creem",
		AmountMinor: amountMinor,
		Currency:    product.Currency,
		Credits:     credits,
	})
	if err != nil {
		return nil, err
	}

	checkout, err := rt.Client.CreateCheckout(ctx, CreateCheckoutInput{
		ProductID:  rt.ProductID,
		RequestID:  p.Code, // provider correlation/reference key
		Units:      units,
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
		// Keep pending; recovery retries the same request_id.
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE",
			"Payment provider result is uncertain; the payment stays pending and can be retried")
	}

	// Response fact checks — official schema fields are mandatory facts:
	// a missing/zero/wrong value is an invalid provider response, not a
	// tolerated gap. Even a `completed` status here grants nothing: the
	// only economic trigger remains the signature-valid
	// checkout.completed webhook reconciled through Payment Core.
	if checkout.ID == "" || checkout.CheckoutURL == "" {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned an incomplete checkout")
	}
	if checkout.RequestID != p.Code {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched request id")
	}
	if checkout.Mode != rt.Mode {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched mode")
	}
	if checkout.ProductID != rt.ProductID {
		return nil, errors.New(502, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider returned a mismatched product")
	}
	if checkout.Units != units {
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

// BindProviderOrder atomically records which provider order backs a
// payment, before any grant. Idempotent when already bound to the same
// order; conflict (no grant) when bound to a different one or when the
// provider order already belongs to another payment.
func BindProviderOrder(ctx context.Context, pool *pg.Pool, provider, code, providerOrderID string) error {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return fmt.Errorf("begin bind tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	other, err := repository.ProviderOrderBelongsToAnotherPayment(ctx, tx, provider, providerOrderID, code)
	if err != nil {
		return fmt.Errorf("provider order lookup: %w", err)
	}
	if other {
		return fmt.Errorf("provider order %s already belongs to another payment", providerOrderID)
	}

	res, err := repository.BindProviderOrderByCode(ctx, tx, code, provider, providerOrderID)
	if err != nil {
		return err
	}
	if res == repository.BindProviderOrderConflict {
		return fmt.Errorf("payment %s provider order binding conflict", code)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit bind tx: %w", err)
	}
	return nil
}

// GetPaymentForBot is the ownership-scoped read for owner-facing
// surfaces: the query itself filters by bot_id.
func GetPaymentForBot(ctx context.Context, pool *pg.Pool, botID int64, code string) (*model.Payment, error) {
	p, err := repository.FindPaymentByCodeForBot(ctx, pool, botID, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load payment")
	}
	if p == nil {
		return nil, errors.New(404, "PAYMENT_NOT_FOUND", "Payment not found")
	}
	return p, nil
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
	// Creem order amounts are in minor units.
	Amount int64 `json:"amount"`
	Units  int64 `json:"units"`
}

// ReconcileCreemCompletion performs the full fact check of a signature-
// valid checkout.completed against the local payment + provider product.
// The lookup key is request_id == payment code; metadata is proof of
// consistency, never an authority for amounts or credits.
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

	if co.Order == nil || co.Order.ID == "" {
		return fmt.Errorf("order.id empty")
	}
	if co.Order.Status != "paid" {
		return fmt.Errorf("order.status %q, want paid", co.Order.Status)
	}
	if co.Order.Product != rt.ProductID {
		return fmt.Errorf("order.product %q, want configured product", co.Order.Product)
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

	// The only grant path: Payment Core → Credits.
	_, _, err = CompletePayment(ctx, pool, p.Code)
	return err
}
