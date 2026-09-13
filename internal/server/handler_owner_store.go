package server

// Owner-facing Store/Redemption entry points. The owner session resolves
// to bot_id — the ONLY subject. No owner_id/owner wallet/owner redemption
// entities exist; handlers translate HTTP to store-domain calls and map
// domain results to external DTOs that never expose internal numeric ids.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/store"
)

// storeProductDTO is the external product contract: no internal numeric id.
type storeProductDTO struct {
	Code         string  `json:"code"`
	Title        string  `json:"title"`
	Description  *string `json:"description,omitempty"`
	CreditsPrice float64 `json:"credits_price"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// storeRedemptionDTO is the external redemption contract: no internal
// numeric id, no product_id, no bot_id.
type storeRedemptionDTO struct {
	Code            string  `json:"code"`
	ProductTitle    string  `json:"product_title"`
	CreditsCost     float64 `json:"credits_cost"`
	RequestKey      string  `json:"request_key"`
	Status          string  `json:"status"`
	ReviewNote      *string `json:"review_note,omitempty"`
	FulfillmentNote *string `json:"fulfillment_note,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	ReviewedAt      *string `json:"reviewed_at,omitempty"`
	FulfilledAt     *string `json:"fulfilled_at,omitempty"`
	CancelledAt     *string `json:"cancelled_at,omitempty"`
	Created         *bool   `json:"created,omitempty"` // redeem response only
}

func productToDTO(p *model.StoreProduct) storeProductDTO {
	return storeProductDTO{
		Code:         p.Code,
		Title:        p.Title,
		Description:  p.Description,
		CreditsPrice: p.CreditsPrice,
		Status:       p.Status,
		CreatedAt:    p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func redemptionToDTO(r *model.Redemption) storeRedemptionDTO {
	dto := storeRedemptionDTO{
		Code:         r.Code,
		ProductTitle: r.ProductTitle,
		CreditsCost:  r.CreditsCost,
		RequestKey:   r.RequestKey,
		Status:       r.Status,
		CreatedAt:    r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.ReviewNote != nil {
		dto.ReviewNote = r.ReviewNote
	}
	if r.FulfillmentNote != nil {
		dto.FulfillmentNote = r.FulfillmentNote
	}
	if r.ReviewedAt != nil {
		s := r.ReviewedAt.UTC().Format("2006-01-02T15:04:05Z")
		dto.ReviewedAt = &s
	}
	if r.FulfilledAt != nil {
		s := r.FulfilledAt.UTC().Format("2006-01-02T15:04:05Z")
		dto.FulfilledAt = &s
	}
	if r.CancelledAt != nil {
		s := r.CancelledAt.UTC().Format("2006-01-02T15:04:05Z")
		dto.CancelledAt = &s
	}
	return dto
}

// handleOwnerStoreProducts: GET /api/owner/store/products — active catalog.
func (s *Server) handleOwnerStoreProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	if _, err := s.requireOwnerAuth(r); err != nil {
		handleAppError(w, err)
		return
	}
	products, err := store.ListActiveProducts(r.Context(), s.Pool)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items := make([]storeProductDTO, 0, len(products))
	for i := range products {
		items = append(items, productToDTO(&products[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"products": items,
		"meta":     map[string]interface{}{"returned": len(items)},
	}, "")
}

// handleOwnerStoreRedeem: POST /api/owner/store/redemptions.
// Body: {"product_code": "...", "request_key": "..."} — nothing else.
func (s *Server) handleOwnerStoreRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		InvalidJSON(w, "Could not read request body")
		return
	}
	var input struct {
		ProductCode string `json:"product_code"`
		RequestKey  string `json:"request_key"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		InvalidJSON(w, "Request body must be valid JSON object")
		return
	}
	if input.ProductCode == "" {
		handleAppError(w, errors.New(400, "MISSING_FIELD", "Missing required field: product_code"))
		return
	}
	if input.RequestKey == "" {
		handleAppError(w, errors.New(400, "MISSING_FIELD", "Missing required field: request_key"))
		return
	}

	res, err := store.Redeem(r.Context(), s.Pool, bot.ID, input.ProductCode, input.RequestKey)
	if err != nil {
		handleAppError(w, err)
		return
	}

	dto := redemptionToDTO(res.Redemption)
	created := res.Created
	dto.Created = &created
	msg := "Redemption created"
	if !res.Created {
		msg = "Redemption already exists (idempotent replay)"
	}
	SuccessResponse(w, map[string]interface{}{"redemption": dto}, msg)
}

// handleOwnerStoreRedemptionGet: GET /api/owner/store/redemptions/{code}
// — ownership-scoped in the SQL itself.
func (s *Server) handleOwnerStoreRedemptionGet(w http.ResponseWriter, r *http.Request) {
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
	redemption, err := store.GetRedemptionForBot(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{
		"redemption": redemptionToDTO(redemption),
	}, "")
}
