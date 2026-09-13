// Package payment — Creem adapter. Minimal, current-official-paradigm
// Hosted Checkout + webhook client only. No framework, no subscription
// support, no Customer Credits (double-ledger risk), no client-supplied
// base URL (tests inject one via the constructor).
package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CreemClient talks to the Creem API. BaseURL is derived from the
// configured mode (test/prod) — never from a client request.
type CreemClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewCreemClient builds a client for the configured mode.
func NewCreemClient(cfg CreemConfig) *CreemClient {
	return &CreemClient{
		baseURL: strings.TrimRight(cfg.APIBase, "/"),
		apiKey:  cfg.APIKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// CreemConfig is the resolved provider configuration handed to the client.
type CreemConfig struct {
	APIBase string // https://api.creem.io or https://test-api.creem.io
	APIKey  string
}

// CreemProduct is the provider-side fiat price authority fetched live
// from GET /v1/products.
type CreemProduct struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BillingType string `json:"billing_type"`
	Status      string `json:"status"`
	Mode        string `json:"mode"`
	Currency    string `json:"currency"`
	// Creem returns the unit price in minor units (e.g. cents).
	Price int64 `json:"price"`
}

// CreemCheckout is the created checkout session.
type CreemCheckout struct {
	ID          string `json:"id"`
	RequestID   string `json:"request_id"`
	CheckoutURL string `json:"checkout_url"`
	Status      string `json:"status"`
	Mode        string `json:"mode"`
}

// ErrCreemDefinitive marks a provider response that definitively failed
// (4xx authorization/request rejection): the payment may be marked failed.
// Transient errors (network, 429, 5xx) are NOT definitive — the checkout
// may exist upstream and the same request_id can still recover.
type ErrCreemDefinitive struct {
	Status int
	Body   string
}

func (e *ErrCreemDefinitive) Error() string {
	return fmt.Sprintf("creem definitive rejection (HTTP %d): %s", e.Status, truncate(e.Body, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// doRaw performs the request and returns the raw response body bytes on
// 2xx; non-2xx maps to definitive/ambiguous errors as in do.
func (c *CreemClient) doRaw(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err // network ambiguity — not definitive
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return nil, &ErrCreemDefinitive{Status: resp.StatusCode, Body: string(raw)}
	}
	return nil, fmt.Errorf("creem ambiguous failure (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
}

func (c *CreemClient) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err // network ambiguity — not definitive
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return &ErrCreemDefinitive{Status: resp.StatusCode, Body: string(raw)}
	}
	// 429 / 5xx / anything else: ambiguous.
	return fmt.Errorf("creem ambiguous failure (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
}

// GetProduct fetches the configured product live. The caller validates it
// against expectations (id, onetime, active, mode, price, currency).
// GET is safe to call twice; a single request is issued and the response
// is parsed tolerating both envelope shapes (wrapped / direct object).
func (c *CreemClient) GetProduct(ctx context.Context, productID string) (*CreemProduct, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/v1/products?product_id="+url.QueryEscape(productID), nil)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Product CreemProduct `json:"product"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Product.ID != "" {
		return &wrapped.Product, nil
	}
	var direct CreemProduct
	if err := json.Unmarshal(raw, &direct); err != nil || direct.ID == "" {
		return nil, fmt.Errorf("creem product response missing product id")
	}
	return &direct, nil
}

// CreateCheckoutInput is the server-owned checkout request payload.
// request_id is the Kungfu payment code — the correlation/idempotency key.
type CreateCheckoutInput struct {
	ProductID  string            `json:"product_id"`
	RequestID  string            `json:"request_id"`
	Units      int64             `json:"units"`
	SuccessURL string            `json:"success_url"`
	Metadata   map[string]string `json:"metadata"`
}

// CreateCheckout creates a hosted checkout. Exactly ONE POST per call —
// retries with the SAME RequestID are the caller's idempotency strategy
// (provider-side request_id tracking), never a duplicated request here.
func (c *CreemClient) CreateCheckout(ctx context.Context, in CreateCheckoutInput) (*CreemCheckout, error) {
	raw, err := c.doRaw(ctx, http.MethodPost, "/v1/checkouts", in)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Checkout CreemCheckout `json:"checkout"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Checkout.ID != "" {
		return &wrapped.Checkout, nil
	}
	var direct CreemCheckout
	if err := json.Unmarshal(raw, &direct); err != nil || direct.ID == "" {
		return nil, fmt.Errorf("creem checkout response missing id")
	}
	return &direct, nil
}

// VerifyCreemWebhookSignature authenticates a webhook delivery:
// HMAC-SHA256 over the RAW request body with the webhook secret,
// lowercase hex, constant-time compare against creem-signature.
// Returns false on any mismatch — zero DB mutation is the caller's rule.
func VerifyCreemWebhookSignature(rawBody []byte, secret, signatureHeader string) bool {
	if secret == "" || signatureHeader == "" {
		return false
	}
	sig, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(signatureHeader)))
	if err != nil || len(sig) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	return hmac.Equal(mac.Sum(nil), sig)
}
