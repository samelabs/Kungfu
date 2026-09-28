package repository

// Finance Admin read-only repository primitives (012).
//
// Provider = payment facts authority; Payment domain = payment
// policy authority; Credits = balance/ledger mutation authority;
// Finance Admin = observation/reconciliation authority ONLY.
//
// Every function here is a pure read (SELECT). There is no finance
// write primitive in this file by design. All monetary BIGINTs are
// returned as int64 and wired by the server layer as canonical
// decimal strings — never float64.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// ErrInvalidFinanceFilter is returned for unknown enum-ish filter
// values (status/kind/provider beyond their vocabularies). Bad user
// input is an explicit 400 — never a 200 with a masquerading empty
// list.
var ErrInvalidFinanceFilter = apperr.New(400, "INVALID_FINANCE_FILTER", "invalid finance filter value")

var ErrFinanceNotFound = apperr.New(404, "NOT_FOUND", "finance fact not found")

// -- Payment list ------------------------------------------------

// FinancePayment is one row of the payment list projection. Allowlist
// only: no API keys, no webhook secrets, no session material (those
// do not even exist in tb_payments). provider_product_id /
// provider_order_id are nullable columns (legacy / manual payments
// have no provider order) — they stay pointers so a NULL scans
// without error and serializes as JSON null.
type FinancePayment struct {
	ID                int64
	Code              string
	BotID             int64
	BotName           string
	Provider          string
	ProviderProductID *string
	ProviderOrderID   *string
	AmountMinor       int64
	Currency          string
	Credits           int64
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	PaidAt            *time.Time
}

// FinancePaymentFilter carries the payment list parameters.
type FinancePaymentFilter struct {
	Status   string // "" | pending | paid | failed | cancelled (chk_payments_status)
	Provider string // "" | creem
	BotID    int64  // 0 = no filter
	Q        string // code / bot_name / provider_order_id substring
	Page     int
	PageSize int
}

var financePaymentStatusVocab = map[string]bool{
	"pending": true, "paid": true, "failed": true, "cancelled": true,
}

// AdminListFinancePayments returns the stable paginated payment list
// (id DESC) plus the total count of matching rows.
func AdminListFinancePayments(ctx context.Context, pool *pg.Pool, f FinancePaymentFilter) ([]FinancePayment, int64, error) {
	where := " WHERE 1=1"
	args := []interface{}{}
	addArg := func(v interface{}) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Status != "" {
		if !financePaymentStatusVocab[f.Status] {
			return nil, 0, ErrInvalidFinanceFilter
		}
		where += " AND p.status = " + addArg(f.Status)
	}
	if f.Provider != "" {
		// Enumerate the known provider vocabulary; anything else is
		// bad input (explicit 400, not silent empty).
		if f.Provider != "creem" {
			return nil, 0, ErrInvalidFinanceFilter
		}
		where += " AND p.provider = " + addArg(f.Provider)
	}
	if f.BotID != 0 {
		where += " AND p.bot_id = " + addArg(f.BotID)
	}
	if f.Q != "" {
		q := addArg("%" + f.Q + "%")
		where += " AND (p.code ILIKE " + q + " OR b.bot_name ILIKE " + q + " OR p.provider_order_id ILIKE " + q + ")"
	}

	var total int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM tb_payments p JOIN tb_bots b ON b.id = p.bot_id"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Clamp before splicing: normPage guarantees page ≥ 1 and
	// 1 ≤ pageSize ≤ 200, so LIMIT/OFFSET can never go negative or
	// unbounded regardless of what the caller passed.
	page, size := normPage(f.Page, f.PageSize)
	offset := (page - 1) * size
	rows, err := pool.Query(ctx, `
		SELECT p.id, p.code, p.bot_id, b.bot_name, p.provider,
		       p.provider_product_id, p.provider_order_id,
		       p.amount_minor, p.currency, p.credits, p.status,
		       p.created_at, p.updated_at, p.paid_at
		FROM tb_payments p
		JOIN tb_bots b ON b.id = p.bot_id`+where+`
		ORDER BY p.id DESC
		LIMIT `+strconv.Itoa(size)+` OFFSET `+strconv.Itoa(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FinancePayment
	for rows.Next() {
		var p FinancePayment
		if err := rows.Scan(&p.ID, &p.Code, &p.BotID, &p.BotName, &p.Provider,
			&p.ProviderProductID, &p.ProviderOrderID,
			&p.AmountMinor, &p.Currency, &p.Credits, &p.Status,
			&p.CreatedAt, &p.UpdatedAt, &p.PaidAt); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// -- Payment detail ----------------------------------------------

// FinanceAdjustment mirrors tb_payment_adjustments facts (allowlist;
// no raw webhook payload — none is persisted by the Payment domain).
// object_status / transaction_status / reason / refunded_amount_minor
// mirror migration 005's nullable columns and stay pointers.
type FinanceAdjustment struct {
	ID                     int64
	PaymentCode            string
	BotID                  int64
	BotName                string
	Provider               string
	ProviderEventID        string
	ProviderObjectID       string
	Kind                   string
	ProviderTransactionID  string
	ProviderOrderID        string
	AmountMinor            int64
	Currency               string
	TransactionAmountMinor int64
	AmountPaidMinor        int64
	RefundedAmountMinor    *int64
	ObjectStatus           *string
	TransactionStatus      *string
	Reason                 *string
	ProviderCreatedAt      int64
	CreatedAt              time.Time
}

// FinanceLedgerEntry is one tb_transactions row in the explorer
// projection.
type FinanceLedgerEntry struct {
	ID           int64
	BotID        int64
	BotName      string
	Type         string
	Amount       int64
	BalanceAfter int64
	RefType      string
	RefID        string
	CreatedAt    time.Time
}

// scanFinanceLedgerRow scans one ledger row, tolerating the legacy
// NULL ref_id of spend_push rows (rendered as "").
func scanFinanceLedgerRow(scan func(...interface{}) error) (FinanceLedgerEntry, error) {
	var e FinanceLedgerEntry
	var refType, refID *string
	if err := scan(&e.ID, &e.BotID, &e.BotName, &e.Type, &e.Amount, &e.BalanceAfter,
		&refType, &refID, &e.CreatedAt); err != nil {
		return e, err
	}
	if refType != nil {
		e.RefType = *refType
	}
	if refID != nil {
		e.RefID = *refID
	}
	return e, nil
}

// FinanceReconciliation is the local-facts integrity projection for
// one payment. Computed facts only — this is NOT a provider
// re-query, NOT a second Payment policy, and never mutates anything.
type FinanceReconciliation struct {
	GrantPaymentCount   int64
	GrantPaymentSum     int64
	ReversePaymentCount int64
	ReversePaymentSum   int64
	AdjustmentCount     int64
	RefundFactCount     int64
	DisputeFactCount    int64

	DistinctAdjustmentProviderTransactionCount int64
	DistinctAdjustmentAmountPaidCount          int64

	MaxPersistedRefundedAmount int64

	CurrentAccountBalance           int64
	LatestAccountLedgerBalanceAfter *int64 // nil = bot has no ledger rows

	// Integrity facts (computed strictly from the persisted
	// invariants; nothing is ever auto-repaired).
	PaidGrantExact                          bool
	AdjustmentBasisConsistent               bool
	ReversePaymentNonPositive               bool
	ReversePaymentWithinOriginalEntitlement bool
	AccountBalanceMatchesLatestLedger       bool
}

// FinancePaymentDetail is the full local fact chain for one payment.
type FinancePaymentDetail struct {
	Payment        FinancePayment
	Adjustments    []FinanceAdjustment
	GrantEntries   []FinanceLedgerEntry
	ReverseEntries []FinanceLedgerEntry
	Reconciliation FinanceReconciliation
}

// AdminGetFinancePaymentDetail loads the complete local fact chain:
// payment snapshot → bot identity → adjustment facts → grant_payment
// ledger facts → reverse_payment ledger facts → local reconciliation
// facts. Payment status semantics are untouched (paid stays paid;
// adjustments are separate facts, never a new status).
func AdminGetFinancePaymentDetail(ctx context.Context, pool *pg.Pool, code string) (*FinancePaymentDetail, error) {
	d := &FinancePaymentDetail{}
	var paidAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT p.id, p.code, p.bot_id, b.bot_name, p.provider,
		       p.provider_product_id, p.provider_order_id,
		       p.amount_minor, p.currency, p.credits, p.status,
		       p.created_at, p.updated_at, p.paid_at
		FROM tb_payments p JOIN tb_bots b ON b.id = p.bot_id
		WHERE p.code = $1`, code).Scan(
		&d.Payment.ID, &d.Payment.Code, &d.Payment.BotID, &d.Payment.BotName, &d.Payment.Provider,
		&d.Payment.ProviderProductID, &d.Payment.ProviderOrderID,
		&d.Payment.AmountMinor, &d.Payment.Currency, &d.Payment.Credits, &d.Payment.Status,
		&d.Payment.CreatedAt, &d.Payment.UpdatedAt, &paidAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFinanceNotFound
		}
		return nil, err
	}
	d.Payment.PaidAt = paidAt

	// Adjustment facts for this payment.
	rows, err := pool.Query(ctx, `
		SELECT a.id, p.code, a.provider, a.provider_event_id, a.provider_object_id,
		       a.kind, a.provider_transaction_id, a.provider_order_id,
		       a.amount_minor, a.currency, a.transaction_amount_minor,
		       a.amount_paid_minor, a.refunded_amount_minor,
		       a.object_status, a.transaction_status, a.reason,
		       a.provider_created_at, a.created_at
		FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id = a.payment_id
		WHERE p.code = $1
		ORDER BY a.id`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a FinanceAdjustment
		if err := rows.Scan(&a.ID, &a.PaymentCode, &a.Provider, &a.ProviderEventID, &a.ProviderObjectID,
			&a.Kind, &a.ProviderTransactionID, &a.ProviderOrderID,
			&a.AmountMinor, &a.Currency, &a.TransactionAmountMinor,
			&a.AmountPaidMinor, &a.RefundedAmountMinor,
			&a.ObjectStatus, &a.TransactionStatus, &a.Reason,
			&a.ProviderCreatedAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.BotID = d.Payment.BotID
		a.BotName = d.Payment.BotName
		d.Adjustments = append(d.Adjustments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Grant / reverse ledger facts.
	entries, err := adminFinanceLedgerByRef(ctx, pool, d.Payment.BotID, "payment", code)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		switch e.Type {
		case "grant_payment":
			d.GrantEntries = append(d.GrantEntries, e)
		case "reverse_payment":
			d.ReverseEntries = append(d.ReverseEntries, e)
		}
	}

	// Reconciliation + integrity facts.
	rec, err := adminFinanceReconcile(ctx, pool, d.Payment, d.Adjustments)
	if err != nil {
		return nil, err
	}
	d.Reconciliation = *rec
	return d, nil
}

func adminFinanceLedgerByRef(ctx context.Context, pool *pg.Pool, botID int64, refType, refID string) ([]FinanceLedgerEntry, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.bot_id, b.bot_name, t.type, t.amount, t.balance_after,
		       t.ref_type, t.ref_id, t.created_at
		FROM tb_transactions t JOIN tb_bots b ON b.id = t.bot_id
		WHERE t.bot_id = $1 AND t.ref_type = $2 AND t.ref_id = $3
		ORDER BY t.id`, botID, refType, refID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FinanceLedgerEntry
	for rows.Next() {
		e, err := scanFinanceLedgerRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// adminFinanceReconcile computes the local integrity projection.
// Read-only, invariant-based, never repairs anything.
func adminFinanceReconcile(ctx context.Context, pool *pg.Pool, p FinancePayment, adjustments []FinanceAdjustment) (*FinanceReconciliation, error) {
	r := &FinanceReconciliation{
		// Innocent until a persisted fact contradicts the invariant.
		PaidGrantExact:                          true,
		AdjustmentBasisConsistent:               true,
		ReversePaymentNonPositive:               true,
		ReversePaymentWithinOriginalEntitlement: true,
		AccountBalanceMatchesLatestLedger:       true,
	}

	if err := pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE type = 'grant_payment'),
			COALESCE(SUM(amount) FILTER (WHERE type = 'grant_payment'), 0),
			COUNT(*) FILTER (WHERE type = 'reverse_payment'),
			COALESCE(SUM(amount) FILTER (WHERE type = 'reverse_payment'), 0)
		FROM tb_transactions
		WHERE bot_id = $1 AND ref_type = 'payment' AND ref_id = $2`,
		p.BotID, p.Code).Scan(
		&r.GrantPaymentCount, &r.GrantPaymentSum,
		&r.ReversePaymentCount, &r.ReversePaymentSum); err != nil {
		return nil, err
	}

	r.AdjustmentCount = int64(len(adjustments))
	basisTxn := map[string]bool{}
	basisPaid := map[int64]bool{}
	for _, a := range adjustments {
		switch a.Kind {
		case "refund":
			r.RefundFactCount++
		case "dispute":
			r.DisputeFactCount++
		}
		basisTxn[a.ProviderTransactionID] = true
		basisPaid[a.AmountPaidMinor] = true
		if a.RefundedAmountMinor != nil && *a.RefundedAmountMinor > r.MaxPersistedRefundedAmount {
			r.MaxPersistedRefundedAmount = *a.RefundedAmountMinor
		}
	}
	r.DistinctAdjustmentProviderTransactionCount = int64(len(basisTxn))
	r.DistinctAdjustmentAmountPaidCount = int64(len(basisPaid))

	// paid_grant_exact: paid ⇒ grant sum == credits; otherwise 0.
	if p.Status == "paid" {
		r.PaidGrantExact = r.GrantPaymentSum == p.Credits
	} else {
		r.PaidGrantExact = r.GrantPaymentSum == 0
	}
	// adjustment_basis_consistent: at most one basis per dimension.
	r.AdjustmentBasisConsistent =
		r.DistinctAdjustmentProviderTransactionCount <= 1 &&
			r.DistinctAdjustmentAmountPaidCount <= 1
	// reverse_payment_nonpositive.
	r.ReversePaymentNonPositive = r.ReversePaymentSum <= 0
	// reverse_payment_within_original_entitlement.
	reversed := -r.ReversePaymentSum
	r.ReversePaymentWithinOriginalEntitlement = reversed >= 0 && reversed <= p.Credits

	// account_balance_matches_latest_ledger (n/a must be explicit).
	if err := pool.QueryRow(ctx, `
		SELECT b.balance, l.balance_after
		FROM tb_bots b
		LEFT JOIN LATERAL (
			SELECT balance_after FROM tb_transactions
			WHERE bot_id = b.id ORDER BY id DESC LIMIT 1
		) l ON TRUE
		WHERE b.id = $1`, p.BotID).Scan(
		&r.CurrentAccountBalance, &r.LatestAccountLedgerBalanceAfter); err != nil {
		return nil, err
	}
	if r.LatestAccountLedgerBalanceAfter == nil {
		// No ledger rows at all: explicitly NOT a PASS.
		r.AccountBalanceMatchesLatestLedger = false
	} else {
		r.AccountBalanceMatchesLatestLedger = r.CurrentAccountBalance == *r.LatestAccountLedgerBalanceAfter
	}
	return r, nil
}

// -- Adjustments list --------------------------------------------

// FinanceAdjustmentFilter carries the adjustment list parameters.
type FinanceAdjustmentFilter struct {
	Kind        string // "" | refund | dispute
	Provider    string // "" | creem
	PaymentCode string
	BotID       int64
	Page        int
	PageSize    int
}

// AdminListFinanceAdjustments returns the paginated adjustment list
// (id DESC) plus total. Raw webhook payloads are not persisted by
// the Payment domain and are therefore never present here.
func AdminListFinanceAdjustments(ctx context.Context, pool *pg.Pool, f FinanceAdjustmentFilter) ([]FinanceAdjustment, int64, error) {
	where := " WHERE 1=1"
	args := []interface{}{}
	addArg := func(v interface{}) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Kind != "" {
		if f.Kind != "refund" && f.Kind != "dispute" {
			return nil, 0, ErrInvalidFinanceFilter
		}
		where += " AND a.kind = " + addArg(f.Kind)
	}
	if f.Provider != "" {
		if f.Provider != "creem" {
			return nil, 0, ErrInvalidFinanceFilter
		}
		where += " AND a.provider = " + addArg(f.Provider)
	}
	if f.PaymentCode != "" {
		where += " AND p.code = " + addArg(f.PaymentCode)
	}
	if f.BotID != 0 {
		where += " AND p.bot_id = " + addArg(f.BotID)
	}

	var total int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id = a.payment_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Clamp before splicing (see AdminListFinancePayments).
	page, size := normPage(f.Page, f.PageSize)
	offset := (page - 1) * size
	rows, err := pool.Query(ctx, `
		SELECT a.id, p.code, p.bot_id, b.bot_name, a.provider,
		       a.provider_event_id, a.provider_object_id, a.kind,
		       a.provider_transaction_id, a.provider_order_id,
		       a.amount_minor, a.currency, a.transaction_amount_minor,
		       a.amount_paid_minor, a.refunded_amount_minor,
		       a.object_status, a.transaction_status, a.reason,
		       a.provider_created_at, a.created_at
		FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id = a.payment_id
		JOIN tb_bots b ON b.id = p.bot_id`+where+`
		ORDER BY a.id DESC
		LIMIT `+strconv.Itoa(size)+` OFFSET `+strconv.Itoa(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FinanceAdjustment
	for rows.Next() {
		var a FinanceAdjustment
		if err := rows.Scan(&a.ID, &a.PaymentCode, &a.BotID, &a.BotName, &a.Provider,
			&a.ProviderEventID, &a.ProviderObjectID, &a.Kind,
			&a.ProviderTransactionID, &a.ProviderOrderID,
			&a.AmountMinor, &a.Currency, &a.TransactionAmountMinor,
			&a.AmountPaidMinor, &a.RefundedAmountMinor,
			&a.ObjectStatus, &a.TransactionStatus, &a.Reason,
			&a.ProviderCreatedAt, &a.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// -- Ledger explorer ---------------------------------------------

// FinanceLedgerFilter carries the ledger explorer parameters.
type FinanceLedgerFilter struct {
	BotID    int64
	Type     string
	RefType  string
	RefID    string
	Page     int
	PageSize int
}

// AdminListFinanceLedger returns the paginated ledger (id DESC).
// Pure read; finance code has no ledger write primitive at all.
func AdminListFinanceLedger(ctx context.Context, pool *pg.Pool, f FinanceLedgerFilter) ([]FinanceLedgerEntry, int64, error) {
	where := " WHERE 1=1"
	args := []interface{}{}
	addArg := func(v interface{}) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.BotID != 0 {
		where += " AND t.bot_id = " + addArg(f.BotID)
	}
	if f.Type != "" {
		where += " AND t.type = " + addArg(f.Type)
	}
	if f.RefType != "" {
		where += " AND t.ref_type = " + addArg(f.RefType)
	}
	if f.RefID != "" {
		where += " AND t.ref_id = " + addArg(f.RefID)
	}

	var total int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions t`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Clamp before splicing (see AdminListFinancePayments).
	page, size := normPage(f.Page, f.PageSize)
	offset := (page - 1) * size
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.bot_id, b.bot_name, t.type, t.amount, t.balance_after,
		       t.ref_type, t.ref_id, t.created_at
		FROM tb_transactions t JOIN tb_bots b ON b.id = t.bot_id`+where+`
		ORDER BY t.id DESC
		LIMIT `+strconv.Itoa(size)+` OFFSET `+strconv.Itoa(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FinanceLedgerEntry
	for rows.Next() {
		e, err := scanFinanceLedgerRow(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// -- Summary -----------------------------------------------------

// FinanceCurrencyTotal is one currency bucket of paid volume.
// Currencies are NEVER summed across each other.
type FinanceCurrencyTotal struct {
	Currency    string
	AmountMinor int64
	Count       int64
}

// FinanceSummary aggregates the read-only finance overview.
// "Paid volume" — NOT revenue/profit/payout/net: the database has no
// tax, Creem-fee, or payout accounting authority.
type FinanceSummary struct {
	PaymentsByStatus map[string]int64
	PaidVolume       []FinanceCurrencyTotal

	GrantedCreditsTotal  int64 // SUM(grant_payment)
	ReversedCreditsTotal int64 // SUM(reverse_payment) — negative or 0

	RefundFactCount  int64
	DisputeFactCount int64
}

// AdminGetFinanceSummary computes the summary in a handful of reads.
func AdminGetFinanceSummary(ctx context.Context, pool *pg.Pool) (*FinanceSummary, error) {
	s := &FinanceSummary{PaymentsByStatus: map[string]int64{}}

	rows, err := pool.Query(ctx, `SELECT status, COUNT(*) FROM tb_payments GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		s.PaymentsByStatus[st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	vrows, err := pool.Query(ctx, `
		SELECT currency, SUM(amount_minor), COUNT(*)
		FROM tb_payments WHERE status = 'paid'
		GROUP BY currency ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var c FinanceCurrencyTotal
		if err := vrows.Scan(&c.Currency, &c.AmountMinor, &c.Count); err != nil {
			return nil, err
		}
		s.PaidVolume = append(s.PaidVolume, c)
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}

	if err := pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(amount) FILTER (WHERE type = 'grant_payment'), 0),
			COALESCE(SUM(amount) FILTER (WHERE type = 'reverse_payment'), 0)
		FROM tb_transactions`).Scan(&s.GrantedCreditsTotal, &s.ReversedCreditsTotal); err != nil {
		return nil, err
	}

	if err := pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE kind = 'refund'),
			COUNT(*) FILTER (WHERE kind = 'dispute')
		FROM tb_payment_adjustments`).Scan(&s.RefundFactCount, &s.DisputeFactCount); err != nil {
		return nil, err
	}
	return s, nil
}
