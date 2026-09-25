package server

// Regression lock for dashboard RBAC: each dashboard stat card is only
// rendered when the principal holds the matching read permission, and
// the loader must not populate stats for domains the admin cannot
// access. A restricted admin (tasks.read only) sees the task cards but
// no Accounts / Memories / Credits / Redemptions stats.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSamelabsDashboardRBACGating locks the permission-based trimming of
// the dashboard for a restricted admin (tasks.read only).
func TestSamelabsDashboardRBACGating(t *testing.T) {
	e := newAdminEnv(t)
	seedConsoleData(t, e)
	ctx := context.Background()

	// A second admin whose only role grants tasks.read.
	role := "dash_ro_" + time.Now().Format("150405.000000000")
	var roleID int64
	if err := e.s.Pool.QueryRow(ctx, `INSERT INTO tb_admin_roles (code, name) VALUES ($1, 'Dash RO') RETURNING id`, role).Scan(&roleID); err != nil {
		t.Fatalf("role: %v", err)
	}
	if _, err := e.s.Pool.Exec(ctx, `INSERT INTO tb_admin_role_permissions (role_id, permission_code) VALUES ($1, 'tasks.read')`, roleID); err != nil {
		t.Fatalf("perm: %v", err)
	}
	username, password := seedAdminForHTTP(t, e.s)
	var adminID int64
	_ = e.s.Pool.QueryRow(ctx, `SELECT id FROM tb_admins WHERE username=$1`, username).Scan(&adminID)
	_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, adminID)
	if _, err := e.s.Pool.Exec(ctx, `INSERT INTO tb_admin_user_roles (admin_id, role_id) VALUES ($1, $2)`, adminID, roleID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE role_id=$1`, roleID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_role_permissions WHERE role_id=$1`, roleID)
		_, _ = e.s.Pool.Exec(ctx, `DELETE FROM tb_admin_roles WHERE id=$1`, roleID)
	})

	ro := &adminEnv{s: e.s, router: e.router, username: username, password: password}
	ro.login(t)

	body := ro.page(t, "/samelabs").Body.String()

	// Visible: the task domain (permission held, seeded data present).
	if !strings.Contains(body, "Open tasks") {
		t.Fatal("restricted admin (tasks.read) must see the Open tasks card")
	}

	// Hidden: every domain without permission — neither link nor stats.
	for _, banned := range []string{
		`>Accounts<`,                        // accounts.read card
		"AccountsNew7d",                     // (rendered form checked below)
		`href="/samelabs/accounts"`,         // accounts link
		`href="/samelabs/memories`,          // memories link
		`>Memories<`,                        // memories card
		"Credits held by accounts",          // finance.read card
		"Credits earned",                    // finance.read subtext (loose match)
		`href="/samelabs/store/redemptions`, // store.redemptions.read link
		"Redemptions to review",             // store.redemptions.read card
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("restricted dashboard leaks %q", banned)
		}
	}
	// The accounts count itself must not appear via the (now unlinked)
	// Accounts card; assert on the stat label spelling used by the template.
	if strings.Contains(body, "Accounts</span>") {
		t.Fatal("restricted dashboard leaks the Accounts stat card")
	}

	// A superadmin still sees every card.
	super := e.page(t, "/samelabs").Body.String()
	for _, want := range []string{
		"Accounts</span>", "Open tasks", "Completed (7 days)",
		"Memories</span>", "Credits held by accounts", "Redemptions to review",
	} {
		if !strings.Contains(super, want) {
			t.Fatalf("superadmin dashboard missing %q", want)
		}
	}
}
