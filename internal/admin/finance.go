package admin

// Finance Admin control plane (012) — READ-ONLY observation and
// reconciliation authority over Payments, Payment Adjustments, and
// the Credits Ledger.
//
// Fixed authorities (work order):
//   Provider       = payment facts authority
//   Payment domain = payment policy authority
//   Credits        = balance/ledger mutation authority
//   Finance Admin  = observation / reconciliation authority ONLY
//
// This file performs NO economic mutation and contains no reversal
// policy: no floor(C*R/P) recomputation, no dispute→target-C logic.
// It shows entitlement C, persisted adjustment facts, and the actual
// reverse_payment ledger sum; the policy stays in the Payment domain.
//
// Dependency direction: server → admin (here) → repository. This
// package imports neither payment nor credits.

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// Finance view aliases keep the server → admin → repository
// pipeline (admin handlers must not import repository directly —
// enforced by the architecture guard).
type (
	FinancePayment          = repository.FinancePayment
	FinancePaymentDetail    = repository.FinancePaymentDetail
	FinanceAdjustment       = repository.FinanceAdjustment
	FinanceLedgerEntry      = repository.FinanceLedgerEntry
	FinanceSummary          = repository.FinanceSummary
	FinancePaymentFilter    = repository.FinancePaymentFilter
	FinanceAdjustmentFilter = repository.FinanceAdjustmentFilter
	FinanceLedgerFilter     = repository.FinanceLedgerFilter
)

// ListFinancePayments (finance.read): paginated payment list.
func ListFinancePayments(ctx context.Context, pool *pg.Pool, principal *Principal, f FinancePaymentFilter) ([]FinancePayment, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "finance.read"); err != nil {
		return nil, 0, err
	}
	return repository.AdminListFinancePayments(ctx, pool, f)
}

// GetFinancePaymentDetail (finance.read): full local fact chain for
// one payment, including the local reconciliation projection.
func GetFinancePaymentDetail(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*FinancePaymentDetail, error) {
	if err := RequirePermission(ctx, pool, principal, "finance.read"); err != nil {
		return nil, err
	}
	return repository.AdminGetFinancePaymentDetail(ctx, pool, code)
}

// ListFinanceAdjustments (finance.read): paginated adjustment list.
func ListFinanceAdjustments(ctx context.Context, pool *pg.Pool, principal *Principal, f FinanceAdjustmentFilter) ([]FinanceAdjustment, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "finance.read"); err != nil {
		return nil, 0, err
	}
	return repository.AdminListFinanceAdjustments(ctx, pool, f)
}

// ListFinanceLedger (finance.read): paginated credits ledger.
func ListFinanceLedger(ctx context.Context, pool *pg.Pool, principal *Principal, f FinanceLedgerFilter) ([]FinanceLedgerEntry, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "finance.read"); err != nil {
		return nil, 0, err
	}
	return repository.AdminListFinanceLedger(ctx, pool, f)
}

// GetFinanceSummary (finance.read): overview aggregates.
func GetFinanceSummary(ctx context.Context, pool *pg.Pool, principal *Principal) (*FinanceSummary, error) {
	if err := RequirePermission(ctx, pool, principal, "finance.read"); err != nil {
		return nil, err
	}
	return repository.AdminGetFinanceSummary(ctx, pool)
}

// errFinanceReadOnly is the standing answer to any future caller
// asking the Finance domain to mutate economic facts.
var errFinanceReadOnly = errors.New(403, "FINANCE_READ_ONLY", "Finance Admin is a read-only control plane: economic mutations belong to Credits/Payment/Store authorities")
