package server

// Rewards Administration HTTP surface. Handlers do: HTTP parse →
// admin auth/CSRF/permission wiring → admin control-plane call → DTO
// serialization. No repository/rewards/credits imports; no SQL.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/model"
)

// -- DTOs (explicit allowlists) --

func adminRewardsProductDTO(p *model.RewardsProduct) map[string]interface{} {
	var desc interface{}
	if p.Description != nil {
		desc = *p.Description
	}
	return map[string]interface{}{
		"id":    p.ID,
		"code":  p.Code,
		"title": p.Title,
		// credits_price: authoritative whole-credit integer on the
		// wire as a canonical decimal STRING — the admin browser must
		// never route it through JS Number (int64 corruption boundary).
		"credits_price": econString(p.CreditsPrice),
		"description":   desc,
		"status":        p.Status,
		"created_at":    p.CreatedAt,
		"updated_at":    p.UpdatedAt,
	}
}

func adminRewardsRedemptionDTO(r *model.Redemption) map[string]interface{} {
	return map[string]interface{}{
		"id":               r.ID,
		"code":             r.Code,
		"bot_id":           r.BotID,
		"product_id":       r.ProductID,
		"product_title":    r.ProductTitle,
		"credits_cost":     econString(r.CreditsCost),
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

func (s *Server) handleAdminRewardsProductsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	items, total, err := admin.ListRewardsProducts(r.Context(), s.Pool, principal, admin.RewardsProductListFilter{
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
		out = append(out, adminRewardsProductDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"products": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminRewardsProductsCreate(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBodyNumbers(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	principal, err := s.requireAdminMutation(r, "rewards.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	// Fail-closed field parsing: title and credits_price are REQUIRED
	// (string / number); description is OPTIONAL — omitted is legal
	// (no description), present must be a string, any other type is
	// a 400 with ZERO mutation. No silent coercion.
	titleV, exists := input["title"]
	if !exists {
		MissingField(w, "title, credits_price")
		return
	}
	title, ok := titleV.(string)
	if !ok {
		ErrorResponse(w, 400, "INVALID_TITLE", "title must be a string", nil)
		return
	}
	priceV, exists := input["credits_price"]
	if !exists {
		MissingField(w, "title, credits_price")
		return
	}
	price, ok := jsonCredits(priceV)
	if !ok {
		ErrorResponse(w, 400, "INVALID_PRICE", "credits_price must be a whole number", nil)
		return
	}
	description := ""
	if dv, exists := input["description"]; exists {
		// JSON null is a PRESENT non-string value → 400 (fail closed;
		// omit the field entirely for "no description")
		ds, ok := dv.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_DESCRIPTION", "description must be a string", nil)
			return
		}
		description = ds
	}
	created, err := admin.CreateRewardsProduct(r.Context(), s.Pool, principal, admin.RewardsProductInput{
		Title: title, Description: description, CreditsPrice: price,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRewardsProductDTO(created), "Product created")
}

func (s *Server) handleAdminRewardsProductGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	p, err := admin.GetRewardsProduct(r.Context(), s.Pool, principal, r.PathValue("code"))
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRewardsProductDTO(p), "")
}

func (s *Server) handleAdminRewardsProductPatch(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBodyNumbers(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "rewards.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	// partial PATCH: omitted = preserve; provided-but-invalid-type = 400
	patch := admin.RewardsProductPatch{}
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
		fv, ok := jsonCredits(v)
		if !ok {
			ErrorResponse(w, 400, "INVALID_PRICE", "credits_price must be a whole number", nil)
			return
		}
		patch.CreditsPrice = &fv
		provided++
	}
	if provided == 0 {
		ErrorResponse(w, 400, "EMPTY_PATCH", "PATCH must include at least one of title, description, credits_price", nil)
		return
	}
	updated, err := admin.UpdateRewardsProduct(r.Context(), s.Pool, principal, code, patch)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRewardsProductDTO(updated), "Product updated")
}

func (s *Server) handleAdminRewardsProductStatus(w http.ResponseWriter, r *http.Request, status string) {
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "rewards.products.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	updated, err := admin.SetRewardsProductStatus(r.Context(), s.Pool, principal, code, status)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRewardsProductDTO(updated), "Product status updated")
}

func (s *Server) handleAdminRewardsProductActivate(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsProductStatus(w, r, "active")
}

func (s *Server) handleAdminRewardsProductDeactivate(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsProductStatus(w, r, "inactive")
}

// -- redemptions --

func (s *Server) handleAdminRewardsRedemptionsList(w http.ResponseWriter, r *http.Request) {
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
	items, total, err := admin.ListRewardsRedemptions(r.Context(), s.Pool, principal, admin.RewardsRedemptionListFilter{
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
		out = append(out, adminRewardsRedemptionDTO(&items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"redemptions": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminRewardsRedemptionGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	red, err := admin.GetRewardsRedemption(r.Context(), s.Pool, principal, r.PathValue("code"))
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRewardsRedemptionDTO(red), "")
}

// transition routes share one shape: optional note field.
func (s *Server) handleAdminRewardsRedemptionTransition(w http.ResponseWriter, r *http.Request, noteField string,
	run func(principal *admin.Principal, code, note string) (*admin.RewardsTransitionOutcome, error)) {
	// Optional-object body: empty / {} / {note} are all legal; a
	// present note must be a string; malformed or non-object JSON is
	// a 400. Fail closed — never a silent coercion.
	input, err := parseAdminOptionalJSONObject(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := r.PathValue("code")
	principal, err := s.requireAdminMutation(r, "rewards.redemptions.manage")
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
	outcome, err := run(principal, code, note)
	if err != nil {
		handleAppError(w, err)
		return
	}
	// The response uses the transaction-local authoritative After —
	// NO second (permission-gated) read after the commit. An actor
	// with rewards.redemptions.manage but NOT rewards.redemptions.read
	// still gets the committed state here.
	SuccessResponse(w, adminRewardsRedemptionDTO(outcome.After), "")
}

func (s *Server) handleAdminRewardsRedemptionApprove(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsRedemptionTransition(w, r, "review_note", func(p *admin.Principal, code, note string) (*admin.RewardsTransitionOutcome, error) {
		return admin.ApproveRewardsRedemption(r.Context(), s.Pool, p, code, note)
	})
}

func (s *Server) handleAdminRewardsRedemptionReject(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsRedemptionTransition(w, r, "review_note", func(p *admin.Principal, code, note string) (*admin.RewardsTransitionOutcome, error) {
		return admin.RejectRewardsRedemption(r.Context(), s.Pool, p, code, note)
	})
}

func (s *Server) handleAdminRewardsRedemptionFulfill(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsRedemptionTransition(w, r, "fulfillment_note", func(p *admin.Principal, code, note string) (*admin.RewardsTransitionOutcome, error) {
		return admin.FulfillRewardsRedemption(r.Context(), s.Pool, p, code, note)
	})
}

func (s *Server) handleAdminRewardsRedemptionCancel(w http.ResponseWriter, r *http.Request) {
	s.handleAdminRewardsRedemptionTransition(w, r, "", func(p *admin.Principal, code, note string) (*admin.RewardsTransitionOutcome, error) {
		return admin.CancelRewardsRedemption(r.Context(), s.Pool, p, code)
	})
}

// parseAdminOptionalJSONObject accepts an EMPTY body (treated as an
// empty object), a JSON object, or fails closed on anything else.
// Bodies larger than 256KB are rejected explicitly — LimitReader alone
// would SILENTLY IGNORE bytes beyond the cap, so we read limit+1 and
// fail on the overflow. The required-body endpoints are untouched —
// this is a separate parser for the optional-body endpoints only.
func parseAdminOptionalJSONObject(r *http.Request) (map[string]interface{}, error) {
	const maxBody = 262144 // 256KB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, &parseError{msg: "Request body must be valid JSON"}
	}
	if len(body) > maxBody {
		return nil, &parseError{msg: "Request body exceeds the 256KB limit"}
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return map[string]interface{}{}, nil
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &data); err != nil || data == nil {
		return nil, &parseError{msg: "Request body must be a JSON object"}
	}
	return data, nil
}

// -- helpers --

// pageParams parses list pagination. page runs through clampPage so
// (page-1)*page_size stays a sane non-negative offset even before the
// repository's own clamp (P3-33).
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
	return clampPage(page, pageSize), pageSize
}

// jsonCredits extracts a whole-integer Credit value. EXACT integer
// parsing only — no float64 path ever. Accepted wire forms:
//   - json.Number (bodies parsed with UseNumber): Int64() on the
//     exact source text;
//   - canonical decimal integer STRING ("9007199254740993") parsed
//     with strconv.ParseInt — this is the browser write contract
//     (economic integers travel as strings so JS Number never
//     corrupts them). Fractional presentations and >int64 reject.
func jsonCredits(v interface{}) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
	case string:
		// Uses the ONE canonical decimal parser — exactly one
		// string-integer rule across owner and admin boundaries.
		if n, err := parseCanonicalEconInt(t); err == nil {
			return n, true
		}
	}
	return 0, false
}
