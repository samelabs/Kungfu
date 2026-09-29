package admin

// 011 Platform Account Administration domain tests. Runs against a
// private throwaway DB created from the FULL migration chain
// (001→011), so migration-fact assertions are deterministic.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func newAccountTestDB(t *testing.T) *pg.Pool {
	t.Helper()
	return createPrivateDB(t)
}

// testKeyHashN returns a UNIQUE real 32-byte digest per seed row
// (uk_api_key_hash): hash the fixed seed plus a discriminator.
func testKeyHashN(discriminator string) []byte {
	h := sha256.Sum256([]byte("011-test-seed:" + discriminator))
	return h[:]
}

// (1) Migration 011 adds exactly the two account permission facts —
// nothing more (no second RBAC, no finance codes).
func TestAccountsAdminMigrationAddsExactlyTwoPermissions(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()
	var count int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_permissions WHERE code IN ('accounts.read','accounts.manage')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("account permissions = %d, want 2 (%v)", count, err)
	}
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_permissions WHERE code LIKE 'accounts.%'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("accounts.* permissions = %d, want exactly 2 (%v)", count, err)
	}
	// No accidental extra migration artifacts: descriptions present.
	var n int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_permissions WHERE code LIKE 'accounts.%' AND (description IS NULL OR description = '')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("011 codes without description: %d (%v)", n, err)
	}
}

// List returns exactly the seeded rows — the flat projection
// contract (aggregates only exist on the detail call).
func TestAccountsAdminListIsFlatProjection(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		name := string(rune('a'+i)) + "_flat"
		if _, err := db.Exec(ctx, `
			INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
			VALUES ($1, $2, 'aaaa', 'x', 10, 'active')`, name, testKeyHashN(name)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	items, total, err := repository.AdminListAccounts(ctx, db, repository.AdminAccountFilter{
		Status: "all", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 || len(items) != 5 {
		t.Fatalf("list = %d/%d, want 5/5", len(items), total)
	}
	for _, a := range items {
		if a.Balance != 10 {
			t.Fatalf("balance projection = %d, want 10", a.Balance)
		}
		if a.APIKeyLast4 != "aaaa" {
			t.Fatalf("last4 = %q", a.APIKeyLast4)
		}
	}
}

// Stable pagination: ORDER BY created_at DESC, id DESC — identical
// seeds page without duplicates or gaps.
func TestAccountsAdminListPaginationStable(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		if _, err := db.Exec(ctx, `
			INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
			VALUES ($1, $2, 'aaaa', 'x', 'active')`, string(rune('a'+i))+"_page", testKeyHashN(fmt.Sprintf("page%d", i))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		items, _, err := repository.AdminListAccounts(ctx, db, repository.AdminAccountFilter{
			Status: "all", Page: page, PageSize: 3,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, a := range items {
			if seen[a.BotName] {
				t.Fatalf("duplicate %s across pages — pagination unstable", a.BotName)
			}
			seen[a.BotName] = true
		}
	}
	if len(seen) != 7 {
		t.Fatalf("paged through %d unique accounts, want 7", len(seen))
	}
}

// Aggregate distinctness: 2 published tasks + 3 submissions report
// as exactly those two separate counts (never a merged 5).
func TestAccountsAdminDetailAggregatesNeverMerged(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()
	var id int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
		VALUES ('agg_probe', $1, 'aaaa', 'x', 5, 'active') RETURNING id`, testKeyHashN("agg")).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(ctx, `
			INSERT INTO tb_task (code, publisher_id, status, budget_locked)
			VALUES ($2, $1, 'open', 1)`,
			id, fmt.Sprintf("agg_task_%d", i)); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		reqKey := fmt.Sprintf("pr_sub%09d", i)
		if _, err := db.Exec(ctx, `
			INSERT INTO tb_task_submission (task_id, agent_id, request_key, payload_hash, amount, state)
			VALUES ((SELECT id FROM tb_task WHERE publisher_id=$1 ORDER BY id DESC LIMIT 1), 1, $1, $2, '\x00', 1, 'rejected')`,
			id, reqKey); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}
	d, err := repository.AdminGetAccountDetail(ctx, db, id)
	if err != nil || d == nil {
		t.Fatalf("detail: %v", err)
	}
	if d.PublishedTaskCount != 2 {
		t.Fatalf("published_task_count = %d, want 2", d.PublishedTaskCount)
	}
	if d.SubmissionCount != 3 {
		t.Fatalf("submission_count = %d, want 3", d.SubmissionCount)
	}
}

// Status filter vocabulary: unknown status is an explicit 400
// INVALID_STATUS — bad input is never dressed up as a legitimate
// empty result.
func TestAccountsAdminListUnknownStatusIsExplicit400(t *testing.T) {
	db := newAccountTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
		VALUES ('fc_probe', $1, 'aaaa', 'x', 'active')`, testKeyHashN("fc")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := repository.AdminListAccounts(ctx, db, repository.AdminAccountFilter{
		Status: "bogus", Page: 1, PageSize: 50,
	})
	if err == nil {
		t.Fatal("unknown status must return an error, not an empty list")
	}
	if err != repository.ErrInvalidAccountStatus {
		t.Fatalf("err = %v, want ErrInvalidAccountStatus", err)
	}
}
