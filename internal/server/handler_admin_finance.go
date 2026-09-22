package server

// 012 Finance Admin HTTP surface — READ-ONLY. Handlers do: HTTP
// parse → admin auth → finance.read permission (inside the admin
// domain call) → DTO serialization. There is deliberately no POST /
// PATCH / DELETE route anywhere in this file.
//
// Wire contract: every economic BIGINT (amount_minor, credits,
// amount, balance_after, and the adjustment monetary fields) is
// serialized via econString — the canonical integer Credits safe
// representation. No float64 anywhere.

import (
	"net/http"
	"strconv"
	"strings"

	"kungfu.md/internal/admin"
)

// botIDQuery parses the optional bot_id filter, fail closed.
//
//	omitted / empty      -> no filter (0)
//	valid positive int64 -> exact bot filter
//	malformed / 0 / negative / overflow -> 400 INVALID_FINANCE_FILTER
//
// A malformed value must NEVER degrade into an unfiltered query.
func botIDQuery(q map[string][]string) (int64, error) {
	vs := q["bot_id"]
	if len(vs) == 0 || vs[0] == "" {
		return 0, nil
	}
	raw := strings.TrimSpace(vs[0])
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, admin.ErrInvalidFinanceFilter
	}
	if n <= 0 {
		return 0, admin.ErrInvalidFinanceFilter
	}
	return n, nil
}

func financePaymentDTO(p admin.FinancePayment) map[string]interface{} {
	return map[string]interface{}{
		"id":                  p.ID,
		"code":                p.Code,
		"bot_id":              p.BotID,
		"bot_name":            p.BotName,
		"provider":            p.Provider,
		"provider_product_id": p.ProviderProductID,
		"provider_order_id":   p.ProviderOrderID,
		"amount_minor":        econString(p.AmountMinor),
		"currency":            p.Currency,
		"credits":             econString(p.Credits),
		"status":              p.Status,
		"created_at":          p.CreatedAt,
		"updated_at":          p.UpdatedAt,
		"paid_at":             nullableTimeJSON(p.PaidAt),
	}
}

func financeAdjustmentDTO(a admin.FinanceAdjustment) map[string]interface{} {
	return map[string]interface{}{
		"id":                       a.ID,
		"payment_code":             a.PaymentCode,
		"bot_id":                   a.BotID,
		"bot_name":                 a.BotName,
		"provider":                 a.Provider,
		"provider_event_id":        a.ProviderEventID,
		"provider_object_id":       a.ProviderObjectID,
		"kind":                     a.Kind,
		"provider_transaction_id":  a.ProviderTransactionID,
		"provider_order_id":        a.ProviderOrderID,
		"amount_minor":             econString(a.AmountMinor),
		"currency":                 a.Currency,
		"transaction_amount_minor": econString(a.TransactionAmountMinor),
		"amount_paid_minor":        econString(a.AmountPaidMinor),
		"refunded_amount_minor":    econString(a.RefundedAmountMinor),
		"object_status":            a.ObjectStatus,
		"transaction_status":       a.TransactionStatus,
		"reason":                   a.Reason,
		"provider_created_at":      a.ProviderCreatedAt,
		"created_at":               a.CreatedAt,
	}
}

func financeLedgerDTO(e admin.FinanceLedgerEntry) map[string]interface{} {
	return map[string]interface{}{
		"id":            e.ID,
		"bot_id":        e.BotID,
		"bot_name":      e.BotName,
		"type":          e.Type,
		"amount":        econString(e.Amount),
		"balance_after": econString(e.BalanceAfter),
		"ref_type":      e.RefType,
		"ref_id":        e.RefID,
		"created_at":    e.CreatedAt,
	}
}

func (s *Server) handleAdminFinanceSummary(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	summary, err := admin.GetFinanceSummary(r.Context(), s.Pool, principal)
	if err != nil {
		handleAppError(w, err)
		return
	}
	volumes := make([]map[string]interface{}, 0, len(summary.PaidVolume))
	for _, v := range summary.PaidVolume {
		volumes = append(volumes, map[string]interface{}{
			"currency":     v.Currency,
			"amount_minor": econString(v.AmountMinor),
			"count":        v.Count,
		})
	}
	SuccessResponse(w, map[string]interface{}{
		"payments_by_status": summary.PaymentsByStatus,
		"paid_volume":        volumes, // per-currency buckets, never summed across currencies
		"granted_credits":    econString(summary.GrantedCreditsTotal),
		"reversed_credits":   econString(summary.ReversedCreditsTotal),
		"refund_fact_count":  summary.RefundFactCount,
		"dispute_fact_count": summary.DisputeFactCount,
	}, "")
}

func (s *Server) handleAdminFinancePayments(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	botID, err := botIDQuery(q)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items, total, err := admin.ListFinancePayments(r.Context(), s.Pool, principal, admin.FinancePaymentFilter{
		Status:   q.Get("status"),
		Provider: q.Get("provider"),
		BotID:    botID,
		Q:        q.Get("q"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, financePaymentDTO(items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"payments": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminFinancePaymentDetail(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := r.PathValue("code")
	if code == "" {
		MissingField(w, "code")
		return
	}
	d, err := admin.GetFinancePaymentDetail(r.Context(), s.Pool, principal, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	adjustments := make([]map[string]interface{}, 0, len(d.Adjustments))
	for _, a := range d.Adjustments {
		adjustments = append(adjustments, financeAdjustmentDTO(a))
	}
	grants := make([]map[string]interface{}, 0, len(d.GrantEntries))
	for _, e := range d.GrantEntries {
		grants = append(grants, financeLedgerDTO(e))
	}
	reverses := make([]map[string]interface{}, 0, len(d.ReverseEntries))
	for _, e := range d.ReverseEntries {
		reverses = append(reverses, financeLedgerDTO(e))
	}
	rec := d.Reconciliation
	var latestLedger interface{}
	if rec.LatestAccountLedgerBalanceAfter != nil {
		latestLedger = econString(*rec.LatestAccountLedgerBalanceAfter)
	}
	SuccessResponse(w, map[string]interface{}{
		"payment":                financePaymentDTO(d.Payment),
		"adjustments":            adjustments,
		"grant_payment_ledger":   grants,
		"reverse_payment_ledger": reverses,
		"reconciliation": map[string]interface{}{
			"grant_payment_count":                            rec.GrantPaymentCount,
			"grant_payment_sum":                              econString(rec.GrantPaymentSum),
			"reverse_payment_count":                          rec.ReversePaymentCount,
			"reverse_payment_sum":                            econString(rec.ReversePaymentSum),
			"adjustment_count":                               rec.AdjustmentCount,
			"refund_fact_count":                              rec.RefundFactCount,
			"dispute_fact_count":                             rec.DisputeFactCount,
			"distinct_adjustment_provider_transaction_count": rec.DistinctAdjustmentProviderTransactionCount,
			"distinct_adjustment_amount_paid_count":          rec.DistinctAdjustmentAmountPaidCount,
			"max_persisted_refunded_amount":                  econString(rec.MaxPersistedRefundedAmount),
			"current_account_balance":                        econString(rec.CurrentAccountBalance),
			"latest_account_ledger_balance_after":            latestLedger, // null = no ledger rows (explicit n/a)
			"integrity": map[string]interface{}{
				"paid_grant_exact":                            rec.PaidGrantExact,
				"adjustment_basis_consistent":                 rec.AdjustmentBasisConsistent,
				"reverse_payment_nonpositive":                 rec.ReversePaymentNonPositive,
				"reverse_payment_within_original_entitlement": rec.ReversePaymentWithinOriginalEntitlement,
				"account_balance_matches_latest_ledger":       rec.AccountBalanceMatchesLatestLedger,
			},
		},
	}, "")
}

func (s *Server) handleAdminFinanceAdjustments(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	botID, err := botIDQuery(q)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items, total, err := admin.ListFinanceAdjustments(r.Context(), s.Pool, principal, admin.FinanceAdjustmentFilter{
		Kind:        q.Get("kind"),
		Provider:    q.Get("provider"),
		PaymentCode: q.Get("payment_code"),
		BotID:       botID,
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, financeAdjustmentDTO(items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"adjustments": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}

func (s *Server) handleAdminFinanceLedger(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := pageParams(q.Get("page"), q.Get("page_size"))
	botID, err := botIDQuery(q)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items, total, err := admin.ListFinanceLedger(r.Context(), s.Pool, principal, admin.FinanceLedgerFilter{
		BotID:    botID,
		Type:     q.Get("type"),
		RefType:  q.Get("ref_type"),
		RefID:    q.Get("ref_id"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		out = append(out, financeLedgerDTO(items[i]))
	}
	SuccessResponse(w, map[string]interface{}{
		"entries": out, "page": page, "page_size": pageSize, "total": total,
	}, "")
}
