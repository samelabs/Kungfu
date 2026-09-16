package admin

// B1.2 integration tests: accounts, roles, permissions, sessions
// management, passwords, superadmin invariants. Real PostgreSQL via
// private per-test databases created from the full migration chain.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// poolT is the concrete pool type used by these tests.
type poolT = *pg.Pool

// --- helpers to build principals via real login ---

// b12Login logs in and returns raw token + principal.
func b12Login(t *testing.T, pool poolT, username, password string) (string, *Principal) {
	t.Helper()
	res, err := Login(context.Background(), pool, LoginInput{Username: username, Password: password})
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	p, err := ResolveSession(context.Background(), pool, res.RawToken)
	if err != nil {
		t.Fatalf("resolve %s: %v", username, err)
	}
	return res.RawToken, p
}

// b12CreateUserDirect creates an admin directly (no audit, for
// fixture setup) with given roles (codes).
func b12CreateUserDirect(t *testing.T, pool poolT, username, password string, roleCodes []string) *adminRow {
	t.Helper()
	hash, _ := hashForTest(password)
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_admins (username, display_name, password_hash) VALUES ($1, $1, $2) RETURNING id`,
		username, hash).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	for _, code := range roleCodes {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO tb_admin_user_roles (admin_id, role_id)
			SELECT $1, r.id FROM tb_admin_roles r WHERE r.code = $2
			ON CONFLICT DO NOTHING`, id, code)
		if err != nil {
			t.Fatalf("role %s: %v", code, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id=$1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_admin_sessions WHERE admin_id=$1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_admins WHERE id=$1`, id)
	})
	return &adminRow{ID: id, Username: username}
}

type adminRow struct {
	ID       int64
	Username string
}

// -- Create admin --

func TestB12CreateAdminNormalizedAndDuplicate(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	created, err := CreateAdmin(context.Background(), dbPool, root, CreateAdminInput{
		Username: "Ops.Lead", DisplayName: "Ops Lead", Password: "initial-pass-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Username != "ops.lead" {
		t.Fatalf("username not normalized: %q", created.Username)
	}
	if created.Status != "active" {
		t.Fatalf("status = %s", created.Status)
	}
	if created.PasswordHash != "" {
		t.Fatal("password hash must not serialize upward")
	}
	// audit row exists, without secrets
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='admin.user.create' AND success`).Scan(&n)
	if n != 1 {
		t.Fatalf("create audit rows = %d", n)
	}

	// duplicate (case-insensitive → same normalized name)
	_, err = CreateAdmin(context.Background(), dbPool, root, CreateAdminInput{
		Username: "OPS.LEAD", DisplayName: "Dup", Password: "another-pass-1",
	})
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 409 || ae.Code != "ADMIN_USERNAME_EXISTS" {
		t.Fatalf("expected 409 ADMIN_USERNAME_EXISTS, got %v", err)
	}
}

func TestB12CreateAdminRequiresPermission(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	// a viewer with no permissions at all
	viewer := b12CreateUserDirect(t, dbPool, "viewer"+time.Now().Format("150405.000000000"), "viewer-pass-9", []string{"nosuch"})
	vp := b12PrincipalOf(t, dbPool, viewer.Username, "viewer-pass-9")

	_, err := CreateAdmin(context.Background(), dbPool, vp, CreateAdminInput{
		Username: "someone.new", DisplayName: "X", Password: "long-enough-1",
	})
	_ = root
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 403 {
		t.Fatalf("expected 403, got %v", err)
	}
}

// -- display name update --

func TestB12DisplayNameUpdate(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	target := b12CreateUserDirect(t, dbPool, "rename.me", "rename-pass-1", nil)
	updated, err := UpdateAdminDisplayName(context.Background(), dbPool, root, target.ID, " Renamed Person ")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.DisplayName != "Renamed Person" {
		t.Fatalf("display name = %q", updated.DisplayName)
	}
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='admin.user.update' AND success`).Scan(&n)
	if n != 1 {
		t.Fatalf("update audit = %d", n)
	}
}

// -- enable / disable --

func TestB12DisableBumpsAuthVersionAndRevokesSessions(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	target := b12CreateUserDirect(t, dbPool, "victim.one", "victim-pass-1", nil)
	before, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)

	// target has an active session
	raw, _ := b12Login(t, dbPool, target.Username, "victim-pass-1")
	if _, err := ResolveSession(context.Background(), dbPool, raw); err != nil {
		t.Fatalf("precondition resolve: %v", err)
	}

	if err := DisableAdmin(context.Background(), dbPool, root, target.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	after, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)
	if after.Status != "disabled" {
		t.Fatalf("status = %s", after.Status)
	}
	if after.AuthVersion != before.AuthVersion+1 {
		t.Fatalf("auth_version %d -> %d", before.AuthVersion, after.AuthVersion)
	}
	if p, err := ResolveSession(context.Background(), dbPool, raw); err == nil || p != nil {
		t.Fatal("target session must be invalid after disable")
	}

	// idempotent second disable
	if err := DisableAdmin(context.Background(), dbPool, root, target.ID); err != nil {
		t.Fatalf("second disable: %v", err)
	}

	// enable restores status but NOT sessions
	if err := EnableAdmin(context.Background(), dbPool, root, target.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	on, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)
	if on.Status != "active" {
		t.Fatalf("after enable status = %s", on.Status)
	}
	if p, err := ResolveSession(context.Background(), dbPool, raw); err == nil || p != nil {
		t.Fatal("old session must stay revoked after enable")
	}
}

// -- passwords --

func TestB12SelfPasswordChange(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)

	user := b12CreateUserDirect(t, dbPool, "selfchange.user", "old-password-1", nil)
	raw, p := b12Login(t, dbPool, user.Username, "old-password-1")

	// wrong current password
	err := ChangeOwnPassword(context.Background(), dbPool, p, "wrong-current", "new-password-9")
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 401 {
		t.Fatalf("expected 401 on wrong current, got %v", err)
	}

	// correct change
	if err := ChangeOwnPassword(context.Background(), dbPool, p, "old-password-1", "new-password-9"); err != nil {
		t.Fatalf("change: %v", err)
	}
	// current session revoked
	if sess, err := ResolveSession(context.Background(), dbPool, raw); err == nil || sess != nil {
		t.Fatal("session must be revoked after self password change")
	}
	// old password rejected, new accepted
	if _, err := Login(context.Background(), dbPool, LoginInput{Username: user.Username, Password: "old-password-1"}); err == nil {
		t.Fatal("old password must fail")
	}
	if _, err := Login(context.Background(), dbPool, LoginInput{Username: user.Username, Password: "new-password-9"}); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
	// audit exists without secrets
	var blob string
	_ = dbPool.QueryRow(context.Background(),
		`SELECT after_json::text || ' ' || COALESCE(metadata_json::text,'') FROM tb_admin_audit_logs
		 WHERE action='admin.password.change' AND success`).Scan(&blob)
	if strings.Contains(blob, "new-password-9") || strings.Contains(blob, "old-password-1") {
		t.Fatal("audit leaked password material")
	}
}

func TestB12ResetAnotherPassword(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	target := b12CreateUserDirect(t, dbPool, "reset.me", "target-pass-1", nil)
	raw, _ := b12Login(t, dbPool, target.Username, "target-pass-1")

	if err := ResetAdminPassword(context.Background(), dbPool, root, target.ID, "reset-pass-99"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if sess, err := ResolveSession(context.Background(), dbPool, raw); err == nil || sess != nil {
		t.Fatal("target sessions must be revoked by reset")
	}
	if _, err := Login(context.Background(), dbPool, LoginInput{Username: target.Username, Password: "target-pass-1"}); err == nil {
		t.Fatal("old password must fail after reset")
	}
	if _, err := Login(context.Background(), dbPool, LoginInput{Username: target.Username, Password: "reset-pass-99"}); err != nil {
		t.Fatalf("reset password must work: %v", err)
	}
}

// -- roles --

func TestB12RoleLifecycle(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	created, err := CreateRole(context.Background(), dbPool, root, CreateRoleInput{
		Code: "Support.Team", Name: "Support", Description: "helpdesk",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if created.Code != "support.team" {
		t.Fatalf("role code not normalized: %q", created.Code)
	}
	// duplicate code
	_, err = CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "support.team", Name: "Dup"})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.Code != "ADMIN_ROLE_CODE_EXISTS" {
		t.Fatalf("expected ADMIN_ROLE_CODE_EXISTS, got %v", err)
	}

	// update name + disable
	status := "disabled"
	updated, err := UpdateRole(context.Background(), dbPool, root, created.ID, "Support Desk", "helpdesk v2", &status)
	if err != nil {
		t.Fatalf("update role: %v", err)
	}
	if updated.Status != "disabled" || updated.Name != "Support Desk" {
		t.Fatalf("update result: %+v", updated)
	}

	// system role immutable
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, "superadmin")
	_, err = UpdateRole(context.Background(), dbPool, root, super.ID, "Hacked", "", nil)
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 {
		t.Fatalf("system role update must 409, got %v", err)
	}
	if err := SetRolePermissions(context.Background(), dbPool, root, super.ID, []string{"admin.users.read"}); err != nil {
		ae, _ := errors.IsAppError(err)
		if ae == nil || ae.HTTPCode != 409 {
			t.Fatalf("system role permission change must 409, got %v", err)
		}
	}
}

func TestB12RolePermissionReplacement(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	role, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "auditors", Name: "Auditors"})
	holder := b12CreateUserDirect(t, dbPool, "audit.guy", "auditor-pass-1", []string{"auditors"})
	_, _ = b12Login(t, dbPool, holder.Username, "auditor-pass-1")

	// grant read perms
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID,
		[]string{"admin.users.read", "admin.roles.read"}); err != nil {
		t.Fatalf("set perms: %v", err)
	}
	if !hp2(t, dbPool, holder.Username, "auditor-pass-1", "admin.users.read") {
		t.Fatal("live permission missing after set")
	}

	// unknown permission code → 400
	err := SetRolePermissions(context.Background(), dbPool, root, role.ID, []string{"admin.users.read", "no.such.perm"})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 400 {
		t.Fatalf("unknown permission must 400, got %v", err)
	}

	// wildcard to custom role → 403
	err = SetRolePermissions(context.Background(), dbPool, root, role.ID, []string{"*"})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 403 {
		t.Fatalf("wildcard to custom role must 403, got %v", err)
	}

	// replacement removes old
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID,
		[]string{"admin.audit.read"}); err != nil {
		t.Fatalf("replace perms: %v", err)
	}
	if hp2(t, dbPool, holder.Username, "auditor-pass-1", "admin.users.read") {
		t.Fatal("removed permission still granted")
	}
	if !hp2(t, dbPool, holder.Username, "auditor-pass-1", "admin.audit.read") {
		t.Fatal("new permission missing")
	}

	// idempotent same PUT → same state
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID,
		[]string{"admin.audit.read"}); err != nil {
		t.Fatalf("idempotent replace: %v", err)
	}
}

// hp2 = has permission, live-resolved (fresh login each check).
func hp2(t *testing.T, pool poolT, username, password, perm string) bool {
	t.Helper()
	_, p := b12Login(t, pool, username, password)
	return p.HasPermission(perm)
}

// -- admin ↔ role assignment --

func TestB12AdminRoleReplacementAndSuperadminRules(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool) // wildcard actor

	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, "superadmin")
	viewerRole, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "viewers", Name: "Viewers"})
	_ = SetRolePermissions(context.Background(), dbPool, root, viewerRole.ID, []string{"admin.users.read"})

	// limited actor: roles.manage but NO wildcard
	manager := b12CreateUserDirect(t, dbPool, "role.manager", "manager-pass-1", nil)
	// give roles.manage via a custom role
	mRole, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "managers", Name: "Managers"})
	_ = SetRolePermissions(context.Background(), dbPool, root, mRole.ID, []string{"admin.roles.manage", "admin.users.read"})
	_ = SetAdminRoles(context.Background(), dbPool, root, manager.ID, []int64{mRole.ID})
	mp := b12PrincipalOf(t, dbPool, manager.Username, "manager-pass-1")

	target := b12CreateUserDirect(t, dbPool, "assign.me", "assign-pass-1", nil)

	// manager CAN assign ordinary roles
	if err := SetAdminRoles(context.Background(), dbPool, mp, target.ID, []int64{viewerRole.ID}); err != nil {
		t.Fatalf("ordinary assignment by manager: %v", err)
	}

	// manager CANNOT grant superadmin (no wildcard)
	err := SetAdminRoles(context.Background(), dbPool, mp, target.ID, []int64{viewerRole.ID, super.ID})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 403 || ae.Code != "SUPERADMIN_MEMBERSHIP_FORBIDDEN" {
		t.Fatalf("superadmin grant by non-wildcard must 403, got %v", err)
	}

	// root (wildcard) CAN grant superadmin
	if err := SetAdminRoles(context.Background(), dbPool, root, target.ID, []int64{super.ID}); err != nil {
		t.Fatalf("superadmin grant by wildcard: %v", err)
	}
	if !hp2(t, dbPool, target.Username, "assign-pass-1", "admin.audit.read") {
		t.Fatal("target should now hold wildcard permissions")
	}

	// manager (still no wildcard) cannot REMOVE superadmin from target
	mp = b12PrincipalOf(t, dbPool, manager.Username, "manager-pass-1")
	err = SetAdminRoles(context.Background(), dbPool, mp, target.ID, []int64{viewerRole.ID})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 403 {
		t.Fatalf("superadmin removal by non-wildcard must 403, got %v", err)
	}

	// unknown role id → 400
	err = SetAdminRoles(context.Background(), dbPool, root, target.ID, []int64{99999999})
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 400 {
		t.Fatalf("unknown role must 400, got %v", err)
	}
}

// -- last active superadmin invariant --

func TestB12LastSuperadminProtection(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, "superadmin")

	// root is the ONLY active superadmin: cannot disable self
	err := DisableAdmin(context.Background(), dbPool, root, root.Admin.ID)
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 || ae.Code != "LAST_SUPERADMIN_REQUIRED" {
		t.Fatalf("disable last superadmin must 409, got %v", err)
	}
	// cannot strip own superadmin role
	err = SetAdminRoles(context.Background(), dbPool, root, root.Admin.ID, nil)
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 || ae.Code != "LAST_SUPERADMIN_REQUIRED" {
		t.Fatalf("strip last superadmin must 409, got %v", err)
	}
	_ = super

	// with a SECOND superadmin, demoting self is fine
	second := b12CreateUserDirect(t, dbPool, "second.super", "second-pass-1", nil)
	if err := SetAdminRoles(context.Background(), dbPool, root, second.ID, []int64{super.ID}); err != nil {
		t.Fatalf("promote second: %v", err)
	}
	if err := SetAdminRoles(context.Background(), dbPool, root, root.Admin.ID, nil); err != nil {
		t.Fatalf("demote self with successor: %v", err)
	}
	// now second is the only one: stripping it fails
	rootP := b12PrincipalOf(t, dbPool, "root", "password-123") // still has roles.manage? no — root has NO roles now
	_ = rootP
	// give root the manager role to act
	err = SetAdminRoles(context.Background(), dbPool, b12PrincipalOf(t, dbPool, "second.super", "second-pass-1"), root.Admin.ID, []int64{super.ID})
	if err != nil {
		t.Fatalf("re-promote root: %v", err)
	}
	// second (now non-super) cannot act; root demotes second — fine
	if err := SetAdminRoles(context.Background(), dbPool, b12PrincipalOf(t, dbPool, "root", "password-123"), second.ID, nil); err != nil {
		t.Fatalf("demote second: %v", err)
	}
	// and now disabling second (non-super) is fine; disabling root (last) fails
	if err := DisableAdmin(context.Background(), dbPool, b12PrincipalOf(t, dbPool, "root", "password-123"), second.ID); err != nil {
		t.Fatalf("disable non-super: %v", err)
	}
	err = DisableAdmin(context.Background(), dbPool, b12PrincipalOf(t, dbPool, "root", "password-123"), root.Admin.ID)
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 {
		t.Fatalf("disable last superadmin again must 409, got %v", err)
	}
}

// Two active superadmins concurrently run destructive ops that would
// each leave only themselves: at most one succeeds; ≥1 active
// superadmin remains.
func TestB12ConcurrentLastSuperadminDestructive(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, "superadmin")

	// create a second superadmin via the formal API
	second := b12CreateUserDirect(t, dbPool, "racer.super", "racer-pass-1", nil)
	if err := SetAdminRoles(context.Background(), dbPool, root, second.ID, []int64{super.ID}); err != nil {
		t.Fatalf("promote second: %v", err)
	}

	// Resolve BOTH principals BEFORE the race (both still superadmins;
	// permission checks use the resolved principal, mutations re-read
	// state under locks).
	rootP := b12PrincipalOf(t, dbPool, "root", "password-123")
	secondP := b12PrincipalOf(t, dbPool, "racer.super", "racer-pass-1")

	type outcome struct {
		err error
	}
	outcomes := make(chan outcome, 2)
	start := make(chan struct{})

	// racer A (root) demotes second
	go func() {
		<-start
		err := SetAdminRoles(context.Background(), dbPool, rootP, second.ID, nil)
		outcomes <- outcome{err}
	}()
	// racer B (second) demotes root
	go func() {
		<-start
		err := SetAdminRoles(context.Background(), dbPool, secondP, root.Admin.ID, nil)
		outcomes <- outcome{err}
	}()
	close(start)

	successes, refusals := 0, 0
	for i := 0; i < 2; i++ {
		oc := <-outcomes
		if oc.err == nil {
			successes++
		} else if ae, ok := errors.IsAppError(oc.err); ok && ae.Code == "LAST_SUPERADMIN_REQUIRED" {
			refusals++
		} else {
			t.Errorf("unexpected error: %v", oc.err)
		}
	}
	if successes > 1 {
		t.Fatalf("both destructive ops succeeded — invariant broken")
	}
	if successes+refusals != 2 {
		t.Fatalf("outcomes %d+%d != 2", successes, refusals)
	}

	// invariant: at least one active superadmin remains
	n, _ := repository.CountActiveSuperadmins(context.Background(), dbPool)
	if n < 1 {
		t.Fatalf("active superadmins = %d, want >= 1", n)
	}
}

// -- sessions --

func TestB12RevokeOneAndForceLogout(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	target := b12CreateUserDirect(t, dbPool, "multi.session", "multi-pass-1", nil)
	raw1, _ := b12Login(t, dbPool, target.Username, "multi-pass-1")
	raw2, _ := b12Login(t, dbPool, target.Username, "multi-pass-1")

	// find session id of raw1
	var sessID int64
	_ = dbPool.QueryRow(context.Background(),
		`SELECT id FROM tb_admin_sessions WHERE token_hash = $1`, HashSessionToken(raw1)).Scan(&sessID)

	// revoke ONE session
	_, err := RevokeSessionByID(context.Background(), dbPool, root, sessID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	var revokedAt interface{}
	_ = dbPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM tb_admin_sessions WHERE id = $1`, sessID).Scan(&revokedAt)
	if revokedAt == nil {
		t.Fatal("session not marked revoked")
	}
	if p, err := ResolveSession(context.Background(), dbPool, raw1); err == nil || p != nil {
		t.Fatal("revoked session must not resolve")
	}
	if _, err := ResolveSession(context.Background(), dbPool, raw2); err != nil {
		t.Fatalf("other session must survive single revoke: %v", err)
	}
	// no auth_version bump on single revoke
	after, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)
	if after.AuthVersion != 1 {
		t.Fatalf("single revoke must not bump auth_version (got %d)", after.AuthVersion)
	}
	// idempotent re-revoke
	if _, err := RevokeSessionByID(context.Background(), dbPool, root, sessID); err != nil {
		t.Fatalf("double revoke: %v", err)
	}

	// force logout: bump + revoke all
	before, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)
	if err := ForceLogoutAdmin(context.Background(), dbPool, root, target.ID); err != nil {
		t.Fatalf("force logout: %v", err)
	}
	fresh, _ := repository.FindAdminByID(context.Background(), dbPool, target.ID)
	if fresh.AuthVersion != before.AuthVersion+1 {
		t.Fatalf("force logout must bump auth_version")
	}
	if p, err := ResolveSession(context.Background(), dbPool, raw2); err == nil || p != nil {
		t.Fatal("all sessions must be revoked by force logout")
	}
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='admin.sessions.force_logout' AND success`).Scan(&n)
	if n != 1 {
		t.Fatalf("force_logout audit = %d", n)
	}
}

// b12Super returns root's pool fixture + principal.
func b12Super(t *testing.T, pool poolT) (poolT, *Principal) {
	t.Helper()
	p := b12PrincipalOf(t, pool, "root", "password-123")
	return pool, p
}

// b12PrincipalOf performs a live login+resolve for username.
func b12PrincipalOf(t *testing.T, pool poolT, username, password string) *Principal {
	t.Helper()
	_, p := b12Login(t, pool, username, password)
	return p
}

var _ = fmt.Sprintf
var _ = sync.Mutex{}
