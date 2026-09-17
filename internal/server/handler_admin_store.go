package server

// B2 Store Administration HTTP surface. Handlers do: HTTP parse →
// admin auth/CSRF/permission wiring → admin control-plane call → DTO
// serialization. No repository/store/credits imports; no SQL.

import (
	"net/http"
	"strconv"
	"strings"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/model"
)

// -- DTOs (explicit allowlists) --

func adminStoreProductDTO(p *model.StoreProduct) map[string]interface{} {
	var desc interface{}
	if p.Description != nil {
		desc = *p.Description
	}
	return map[string]interface{}{
		"id":            p.ID,
		"code":          p.Code,
		"title":         p.Title,
		"description":   desc,
		"credits_price": p.CreditsPrice,
		"status":        p.Status,
		"created_at":    p.CreatedAt,
		"updated_at":    p.UpdatedAt,
	}
}

func adminStoreRedemptionDTO(r *model.Redemption) map[string]interface{} {
	return map[string]interface{}{
		"id":               r.ID,
		"code":             r.Code,
		"bot_id":           r.BotID,
		"product_id":       r.ProductID,
		"product_title":    r.ProductTitle,
		"credits_cost":     r.CreditsCost,
		"request_key":      r.RequestKey,
		"status":           r.Status,
		"review_note":      nullableStringJSON(r.ReviewNote),
		"fulfillment_note": nullableStringJSON(r.FulfillmentNote),
		"created_at":       r.CreatedAt,
		"updated_at":       r.UpdatedAt,
		"reviewed_at":      nullableTimeJSON(r.ReviewedAt),
		"fulfilled_at":     nullableTimeJSON(r.FulfilledAt),
		"cancelled_at":     nullableTimeJSON(r.CancelledAt),
	}
}

// -- products --

func (s *Server) handleAdminStoreProductsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	items, total, err := admin.ListStoreProducts(r.Context(), s.Pool, principal, admin.StoreProductListFilter{
		Status:   strings.TrimSpace(q.Get("status")),
		Q:        strings.TrimSpace(q.Get("q")),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, adminStoreProductDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"products": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminStoreProductsCreate(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	principal, err := s.requireAdminMutation(r, "store.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	title, _ := input["title"].(string)
	description, _ := input["description"].(string)
	price, ok := jsonFloat(input["credits_price"])
	if !ok {
		MissingField(w, "title, description, credits_price")
		return
	}
	created, err := admin.CreateStoreProduct(r.Context(), s.Pool, principal, admin.StoreProductInput{
		Title: title, Description: description, CreditsPrice: price,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreProductDTO(created), "Product created")
}

func (s *Server) handleAdminStoreProductGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	p, err := admin.GetStoreProduct(r.Context(), s.Pool, principal, r.PathValue("code"))
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreProductDTO(p), "")
}

func (s *Server) handleAdminStoreProductPatch(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "store.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	// partial PATCH: omitted = preserve; provided-but-invalid-type = 400
	patch := admin.StoreProductPatch{}
	provided := 0
	if v, exists := input["title"]; exists {
		sv, ok := v.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_TITLE", "title must be a string", nil)
			return
		}
		patch.Title = &sv
		provided++
	}
	if v, exists := input["description"]; exists {
		sv, ok := v.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_DESCRIPTION", "description must be a string", nil)
			return
		}
		patch.Description = &sv // "" clears
		provided++
	}
	if v, exists := input["credits_price"]; exists {
		fv, ok := jsonFloat(v)
		if !ok {
			ErrorResponse(w, 400, "INVALID_PRICE", "credits_price must be a number", nil)
			return
		}
		patch.CreditsPrice = &fv
		provided++
	}
	if provided == 0 {
		ErrorResponse(w, 400, "EMPTY_PATCH", "PATCH must include at least one of title, description, credits_price", nil)
		return
	}
	updated, err := admin.UpdateStoreProduct(r.Context(), s.Pool, principal, code, patch)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreProductDTO(updated), "Product updated")
}

func (s *Server) handleAdminStoreProductStatus(w http.ResponseWriter, r *http.Request, status string) {
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "store.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	updated, err := admin.SetStoreProductStatus(r.Context(), s.Pool, principal, code, status)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreProductDTO(updated), "Product status updated")
}

func (s *Server) handleAdminStoreProductActivate(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreProductStatus(w, r, "active")
}

func (s *Server) handleAdminStoreProductDeactivate(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreProductStatus(w, r, "inactive")
}

// -- redemptions --

func (s *Server) handleAdminStoreRedemptionsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	var botID int64
	if v := strings.TrimSpace(q.Get("bot_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			ErrorResponse(w, 400, "INVALID_BOT_ID", "bot_id must be a positive integer", nil)
			return
		}
		botID = id
	}
	items, total, err := admin.ListStoreRedemptions(r.Context(), s.Pool, principal, admin.StoreRedemptionListFilter{
		Status:   strings.TrimSpace(q.Get("status")),
		BotID:    botID,
		Q:        strings.TrimSpace(q.Get("q")),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, adminStoreRedemptionDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"redemptions": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminStoreRedemptionGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	red, err := admin.GetStoreRedemption(r.Context(), s.Pool, principal, r.PathValue("code"))
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreRedemptionDTO(red), "")
}

// transition routes share one shape: optional note field.
func (s *Server) handleAdminStoreRedemptionTransition(w http.ResponseWriter, r *http.Request, noteField string,
	run func(principal *admin.Principal, code, note string) error) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "store.redemptions.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	note := ""
	if noteField != "" {
		if v, exists := input[noteField]; exists {
			sv, ok := v.(string)
			if !ok {
				ErrorResponse(w, 400, "INVALID_NOTE", noteField+" must be a string", nil)
				return
			}
			note = sv
		}
	}
	if err := run(principal, code, note); err != nil {
		handleAppError(w, err)
		return
	}
	// return the fresh authoritative state
	red, err := admin.GetStoreRedemption(r.Context(), s.Pool, principal, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminStoreRedemptionDTO(red), "")
}

func (s *Server) handleAdminStoreRedemptionApprove(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreRedemptionTransition(w, r, "review_note", func(p *admin.Principal, code, note string) error {
		_, err := admin.ApproveStoreRedemption(r.Context(), s.Pool, p, code, note)
		return err
	})
}

func (s *Server) handleAdminStoreRedemptionReject(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreRedemptionTransition(w, r, "review_note", func(p *admin.Principal, code, note string) error {
		_, err := admin.RejectStoreRedemption(r.Context(), s.Pool, p, code, note)
		return err
	})
}

func (s *Server) handleAdminStoreRedemptionFulfill(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreRedemptionTransition(w, r, "fulfillment_note", func(p *admin.Principal, code, note string) error {
		_, err := admin.FulfillStoreRedemption(r.Context(), s.Pool, p, code, note)
		return err
	})
}

func (s *Server) handleAdminStoreRedemptionCancel(w http.ResponseWriter, r *http.Request) {
	s.handleAdminStoreRedemptionTransition(w, r, "", func(p *admin.Principal, code, note string) error {
		_, err := admin.CancelStoreRedemption(r.Context(), s.Pool, p, code)
		return err
	})
}

// -- helpers --

func pageParams(pageStr, sizeStr string) (int, int) {
	page, pageSize := 1, 50
	if v, err := strconv.Atoi(pageStr); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(sizeStr); err == nil && v > 0 {
		pageSize = v
		if pageSize > 100 {
			pageSize = 100
		}
	}
	return page, pageSize
}

func jsonFloat(v interface{}) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
