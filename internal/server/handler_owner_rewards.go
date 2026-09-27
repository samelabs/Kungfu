package server

// Owner-facing Rewards/Redemption entry points. The owner session resolves
// to bot_id — the ONLY subject. No owner_id/owner wallet/owner redemption
// entities exist; handlers translate HTTP to rewards-domain calls and map
// domain results to external DTOs that never expose internal numeric ids.

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/rewards"
)

// rewardsProductDTO is the external product contract: no internal numeric id.
type rewardsProductDTO struct {
	Code string `json:"code"`
	// CreditsPrice is the authoritative whole-credit integer, on the
	// wire as a canonical decimal STRING: JS Number cannot hold the
	// full int64 range, so the browser never converts it.
	CreditsPrice string  `json:"credits_price"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	Description  *string `json:"description,omitempty"`
}

// rewardsRedemptionDTO is the external redemption contract: no internal
// numeric id, no product_id, no bot_id.
type rewardsRedemptionDTO struct {
	Code string `json:"code"`
	// CreditsCost: canonical decimal string on the wire (see
	// rewardsProductDTO.CreditsPrice).
	CreditsCost     string  `json:"credits_cost"`
	ProductTitle    string  `json:"product_title"`
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

func productToDTO(p *model.RewardsProduct) rewardsProductDTO {
	return rewardsProductDTO{
		Code:         p.Code,
		Title:        p.Title,
		Description:  p.Description,
		CreditsPrice: econString(p.CreditsPrice),
		Status:       p.Status,
		CreatedAt:    p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func redemptionToDTO(r *model.Redemption) rewardsRedemptionDTO {
	dto := rewardsRedemptionDTO{
		Code:         r.Code,
		ProductTitle: r.ProductTitle,
		CreditsCost:  econString(r.CreditsCost),
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

// handleOwnerRewardsProducts: GET /api/owner/rewards/products — active catalog.
func (s *Server) handleOwnerRewardsProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	if _, err := s.requireOwnerAuth(r); err != nil {
		handleAppError(w, err)
		return
	}
	products, err := rewards.ListActiveProducts(r.Context(), s.Pool)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items := make([]rewardsProductDTO, 0, len(products))
	for i := range products {
		items = append(items, productToDTO(&products[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"products": items,
		"meta":     map[string]interface{}{"returned": len(items)},
	}, "")
}

// handleOwnerRewardsRedeem: POST /api/owner/rewards/redemptions.
// Body: {"product_code": "...", "request_key": "..."} — nothing else.
func (s *Server) handleOwnerRewardsRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	// Strict bounded read — 1 MiB cap, oversize/read failure
	// fail closed (existing INVALID_JSON contract).
	body, err := readBoundedRequestBody(r, 1<<20)
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

	res, err := rewards.Redeem(r.Context(), s.Pool, bot.ID, input.ProductCode, input.RequestKey)
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

// handleOwnerRewardsRedemptionGet: GET /api/owner/rewards/redemptions/{code}
// — ownership-scoped in the SQL itself.
func (s *Server) handleOwnerRewardsRedemptionGet(w http.ResponseWriter, r *http.Request) {
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
	redemption, err := rewards.GetRedemptionForBot(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{
		"redemption": redemptionToDTO(redemption),
	}, "")
}
