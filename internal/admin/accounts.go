package admin

// Platform Account Administration control-plane (011).
//
// Boundary: this manages PLATFORM accounts (tb_bots — the Agent /
// Owner identities), NOT admin accounts (management.go) and NOT
// finance. The Credits balance is strictly read-only here; every
// economic mutation remains with Credits / Payment / Task / Store
// authority.
//
// Pipeline: server handler → internal/admin (this file) →
// internal/repository account-admin primitives → PostgreSQL.
// No SQL in this package; no second RBAC — RequirePermission with
// the 011 codes, superadmin via the existing '*' wildcard.

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// AccountView / AccountDetailView are the server-facing read
// projections. Aliases of the repository allowlist types keep the
// server → admin → repository pipeline direction (admin handlers do
// not import repository directly — enforced by architecture guard).
type (
	AccountView       = repository.AdminAccount
	AccountDetailView = repository.AdminAccountDetail
)

// AccountDetail is the promoted detail struct with both the base
// view and the aggregate facts (server serializes via the embedded
// view + its own fields).
type AccountDetail = repository.AdminAccountDetail

// AccountListFilter carries the list parameters into the repository.
type AccountListFilter struct {
	Status   string
	Q        string
	Page     int
	PageSize int
}

// ListAccounts requires accounts.read and returns the paginated
// platform account list (no aggregates — flat projection).
func ListAccounts(ctx context.Context, pool *pg.Pool, principal *Principal, f AccountListFilter) ([]AccountView, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "accounts.read"); err != nil {
		return nil, 0, err
	}
	return repository.AdminListAccounts(ctx, pool, repository.AdminAccountFilter{
		Status:   f.Status,
		Q:        f.Q,
		Page:     f.Page,
		PageSize: f.PageSize,
	})
}

// GetAccount requires accounts.read and returns the account detail
// with the existing light aggregates (published_task_count and
// submission_count are separate business facts and never merged).
func GetAccount(ctx context.Context, pool *pg.Pool, principal *Principal, botID int64) (*AccountDetailView, error) {
	if err := RequirePermission(ctx, pool, principal, "accounts.read"); err != nil {
		return nil, err
	}
	return repository.AdminGetAccountDetail(ctx, pool, botID)
}

// accountStatusFacts is the audit fact snapshot for a mutation:
// before/after status only — no credential material, no balance.
func accountStatusFacts(status string, a *repository.AdminAccount) map[string]interface{} {
	return map[string]interface{}{
		"bot_id":   a.ID,
		"bot_name": a.BotName,
		"status":   status,
	}
}

// DisablePlatformAccount: active → disabled access suspension.
//
// Semantics fixed by the work order:
//   - RequirePermission(accounts.manage)
//   - WithAuditTx: lock target row FOR UPDATE → re-read status →
//     idempotent transition → UPDATE status → audit SAME transaction
//   - disabling is access suspension, NOT credential rotation: no
//     key rewrite, no session-version mechanism, no deletion, no
//     economic side effects. Authentication fails immediately only
//     because Agent/Owner lookups filter on status='active'; the
//     existing (unexpired) credentials become valid again on
//     re-enable under the current identity mechanism.
func DisablePlatformAccount(ctx context.Context, pool *pg.Pool, principal *Principal, botID int64) error {
	return setPlatformAccountStatus(ctx, pool, principal, botID, "disabled",
		"admin.account.disable", repository.AdminAccountStatusDisabled)
}

// EnablePlatformAccount: disabled → active. Idempotent, audited in
// the same transaction. Existing credentials resume under the
// current identity mechanism (no re-issue happens here).
func EnablePlatformAccount(ctx context.Context, pool *pg.Pool, principal *Principal, botID int64) error {
	return setPlatformAccountStatus(ctx, pool, principal, botID, "active",
		"admin.account.enable", repository.AdminAccountStatusActive)
}

func setPlatformAccountStatus(ctx context.Context, pool *pg.Pool, principal *Principal, botID int64, want, action, wantStatus string) error {
	if err := RequirePermission(ctx, pool, principal, "accounts.manage"); err != nil {
		return err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     action,
		TargetType: "bot",
		TargetID:   idToString(botID),
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		// Lock the target row, then re-read status UNDER the lock.
		target, err := repository.AdminLockBotForUpdate(ctx, tx, botID)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if target == nil {
			return errors.New(404, "ACCOUNT_NOT_FOUND", "Platform account not found")
		}
		// Idempotent no-op when already in the wanted status — still
		// audited with the REAL before/after facts.
		if target.Status == wantStatus {
			entry.Before = accountStatusFacts(target.Status, target)
			entry.After = accountStatusFacts(target.Status, target)
			return nil
		}
		if err := repository.AdminSetBotStatus(ctx, tx, botID, wantStatus); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		entry.Before = accountStatusFacts(target.Status, target)
		entry.After = accountStatusFacts(wantStatus, target)
		return nil
	})
}
