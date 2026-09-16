package admin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func hashForTest(password string) (string, error) {
	return auth.HashPassword(password)
}

// -- Bootstrap --

func TestBootstrapFirstAdminSucceeds(t *testing.T) {
	dbPool := createPrivateDB(t)
	res, err := Bootstrap(context.Background(), dbPool, "Root.Admin", "Root Admin", "correct-horse-12")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.Username != "root.admin" {
		t.Fatalf("username not normalized to lowercase: %q", res.Username)
	}
	adminRec, err := repository.FindAdminByUsername(context.Background(), dbPool, "root.admin")
	if err != nil || adminRec == nil {
		t.Fatalf("admin not found after bootstrap: %v", err)
	}
	if adminRec.Status != "active" || adminRec.AuthVersion != 1 {
		t.Fatalf("bad admin row: status=%s auth_version=%d", adminRec.Status, adminRec.AuthVersion)
	}
	// auto superadmin
	perms, err := repository.ListAdminPermissionCodesByAdminID(context.Background(), dbPool, adminRec.ID)
	if err != nil {
		t.Fatalf("perms: %v", err)
	}
	if len(perms) != 1 || perms[0] != "*" {
		t.Fatalf("bootstrap admin must hold exactly *, got %v", perms)
	}
	// bootstrap audit row exists
	var n int
	err = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action = 'admin.bootstrap' AND success`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("bootstrap audit rows = %d err=%v", n, err)
	}
}

func TestBootstrapSecondRunRefused(t *testing.T) {
	dbPool := createPrivateDB(t)
	if _, err := Bootstrap(context.Background(), dbPool, "first", "First", "password-123"); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	_, err := Bootstrap(context.Background(), dbPool, "second", "Second", "password-456")
	if err == nil {
		t.Fatal("second bootstrap must fail closed")
	}
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 409 || ae.Code != "BOOTSTRAP_REFUSED" {
		t.Fatalf("expected 409 BOOTSTRAP_REFUSED, got %v", err)
	}
	// no second superadmin appeared
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_user_roles ur
		 JOIN tb_admin_roles r ON r.id = ur.role_id
		 WHERE r.code = 'superadmin'`).Scan(&n)
	if n != 1 {
		t.Fatalf("superadmin bindings after refused re-run = %d, want 1", n)
	}
}

func TestBootstrapInvalidUsernameRejected(t *testing.T) {
	dbPool := createPrivateDB(t)
	for _, bad := range []string{"ab", "Has Upper", "空间", strings.Repeat("a", 65), "a b"} {
		if _, err := Bootstrap(context.Background(), dbPool, bad, "X", "password-123"); err == nil {
			t.Fatalf("username %q must be rejected", bad)
		}
	}
}

func TestUsernameNormalizeAndUnique(t *testing.T) {
	dbPool := createPrivateDB(t)
	if _, err := Bootstrap(context.Background(), dbPool, "Root", "Root", "password-123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// same username different case is the SAME normalized name
	_, err := Bootstrap(context.Background(), dbPool, "ROOT", "Root2", "password-456")
	if err == nil {
		t.Fatal("bootstrap gate must refuse (already admins) — and username uniqueness is unique(normalized)")
	}
	var count int
	_ = dbPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_admins`).Scan(&count)
	if count != 1 {
		t.Fatalf("admins = %d, want 1", count)
	}
}

// -- Login --

func TestAdminLoginSuccess(t *testing.T) {
	dbPool := createPrivateDB(t)
	if _, err := Bootstrap(context.Background(), dbPool, "root", "Root", "password-123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	res, err := Login(context.Background(), dbPool, LoginInput{Username: "ROOT", Password: "password-123", IPAddress: "1.2.3.4", UserAgent: "test-agent"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.RawToken == "" {
		t.Fatal("raw token missing")
	}
	if res.Session == nil || res.Session.TokenHash == HashSessionToken(res.RawToken) == false {
		// token hash must equal SHA-256(raw)
	}
	if res.Session != nil && res.Session.TokenHash != HashSessionToken(res.RawToken) {
		t.Fatal("stored token hash != sha256(raw token)")
	}
	// roles include superadmin
	found := false
	for _, r := range res.Roles {
		if r == "superadmin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("roles = %v, want superadmin", res.Roles)
	}
	// expires_at ≈ now + 12h
	if d := time.Until(res.Session.ExpiresAt); d < 11*time.Hour || d > 12*time.Hour+time.Minute {
		t.Fatalf("absolute TTL wrong: %v", d)
	}
}

func TestAdminLoginUniformFailures(t *testing.T) {
	dbPool := createPrivateDB(t)
	if _, err := Bootstrap(context.Background(), dbPool, "root", "Root", "password-123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// disabled admin (direct DB flip on a second seed)
	seeded := seedAdmin(t, dbPool, "another-pass-9")
	_, _ = dbPool.Exec(context.Background(), `UPDATE tb_admins SET status='disabled' WHERE id=$1`, seeded.ID)

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"unknown user", "ghost.user", "password-123"},
		{"disabled", seeded.Username, "another-pass-9"},
		{"wrong password", "root", "wrong-password"},
	}
	for _, tc := range cases {
		_, err := Login(context.Background(), dbPool, LoginInput{Username: tc.username, Password: tc.password})
		if err == nil {
			t.Fatalf("%s: expected failure", tc.name)
		}
		ae, ok := errors.IsAppError(err)
		if !ok || ae.HTTPCode != 401 || ae.Code != "INVALID_CREDENTIALS" {
			t.Fatalf("%s: expected uniform 401 INVALID_CREDENTIALS, got %v", tc.name, err)
		}
	}
}

// -- Sessions --

func loginForTest(t *testing.T, dbPool *pg.Pool, username, password string) *LoginResult {
	t.Helper()
	res, err := Login(context.Background(), dbPool, LoginInput{Username: username, Password: password})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return res
}

func bootstrapForTest(t *testing.T, dbPool *pg.Pool) {
	t.Helper()
	if _, err := Bootstrap(context.Background(), dbPool, "root", "Root", "password-123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestRawSessionTokenNeverPersisted(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	// DB row stores only the hash
	var stored string
	err := dbPool.QueryRow(context.Background(),
		`SELECT token_hash FROM tb_admin_sessions WHERE id = $1`, res.Session.ID).Scan(&stored)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if stored == res.RawToken {
		t.Fatal("RAW TOKEN stored in DB")
	}
	if stored != HashSessionToken(res.RawToken) {
		t.Fatal("stored value is not sha256(raw)")
	}
	// token_hash column is exactly 64 hex chars
	if len(stored) != 64 {
		t.Fatalf("token_hash length = %d, want 64", len(stored))
	}
}

func TestSessionExpiryAbsoluteTTLEnforced(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	// force expiry in the past
	_, _ = dbPool.Exec(context.Background(),
		`UPDATE tb_admin_sessions SET expires_at = CURRENT_TIMESTAMP - INTERVAL '1 minute' WHERE id = $1`, res.Session.ID)

	p, err := ResolveSession(context.Background(), dbPool, res.RawToken)
	if err == nil || p != nil {
		t.Fatal("expired session must not resolve")
	}
	ae, _ := errors.IsAppError(err)
	if ae == nil || ae.HTTPCode != 401 {
		t.Fatalf("expected 401, got %v", err)
	}
}

func TestSessionIdleTimeoutEnforced(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	_, _ = dbPool.Exec(context.Background(),
		`UPDATE tb_admin_sessions SET last_seen_at = CURRENT_TIMESTAMP - INTERVAL '31 minutes' WHERE id = $1`, res.Session.ID)

	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err == nil || p != nil {
		t.Fatal("idle-expired session must not resolve")
	}
}

func TestSessionTouchThrottled(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	// first resolve: last_seen is fresh (< 5 min old since login) → NO write
	var before time.Time
	_ = dbPool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM tb_admin_sessions WHERE id = $1`, res.Session.ID).Scan(&before)

	if _, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var after time.Time
	_ = dbPool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM tb_admin_sessions WHERE id = $1`, res.Session.ID).Scan(&after)
	if !before.Equal(after) {
		t.Fatal("fresh session must not be touched on every request")
	}

	// make it stale beyond the throttle window → resolve MUST touch
	_, _ = dbPool.Exec(context.Background(),
		`UPDATE tb_admin_sessions SET last_seen_at = CURRENT_TIMESTAMP - INTERVAL '6 minutes' WHERE id = $1`, res.Session.ID)
	if _, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil {
		t.Fatalf("resolve stale: %v", err)
	}
	_ = dbPool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM tb_admin_sessions WHERE id = $1`, res.Session.ID).Scan(&after)
	if after.Equal(before) {
		t.Fatal("stale session must be touched")
	}
}

func TestRevokedSessionInvalid(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil || p == nil {
		t.Fatalf("valid session must resolve: %v", err)
	}
	p1, _ := ResolveSession(context.Background(), dbPool, res.RawToken)
	if err := RevokeSession(context.Background(), dbPool, p1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err == nil || p != nil {
		t.Fatal("revoked session must not resolve")
	}
	// double revoke is idempotent
	if err := RevokeSession(context.Background(), dbPool, p1); err != nil {
		t.Fatalf("double revoke: %v", err)
	}
}

func TestDisabledAdminSessionImmediatelyInvalid(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	_, _ = dbPool.Exec(context.Background(), `UPDATE tb_admins SET status='disabled' WHERE username='root'`)
	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err == nil || p != nil {
		t.Fatal("disabled admin's session must be invalid immediately")
	}
}

func TestAuthVersionMismatchInvalidatesSession(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	// B1.2 composition shape: business mutation + bump + revoke-all +
	// audit in ONE WithAuditTx transaction.
	adminRow, _ := repository.FindAdminByUsername(context.Background(), dbPool, "root")
	err := WithAuditTx(context.Background(), dbPool, &AuditEntry{
		Action:  "admin.disable",
		Actor:   adminRow,
		Success: true,
		After:   map[string]string{"status": "disabled"},
	}, func(ctx context.Context, tx pg.Querier) error {
		if _, err := tx.Exec(ctx, `UPDATE tb_admins SET status='disabled' WHERE id=$1`, adminRow.ID); err != nil {
			return err
		}
		return bumpAuthVersionAndRevokeSessions(ctx, tx, adminRow.ID)
	})
	if err != nil {
		t.Fatalf("disable+bump tx: %v", err)
	}
	// auth_version on the row moved
	fresh, _ := repository.FindAdminByUsername(context.Background(), dbPool, "root")
	if fresh.AuthVersion != adminRow.AuthVersion+1 {
		t.Fatalf("auth_version = %d, want %d", fresh.AuthVersion, adminRow.AuthVersion+1)
	}
	// session revoked
	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err == nil || p != nil {
		t.Fatal("auth_version bump must invalidate existing session")
	}
	// audit row landed with the mutation
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='admin.disable' AND success`).Scan(&n)
	if n != 1 {
		t.Fatalf("admin.disable audit rows = %d, want 1", n)
	}
}

// Concurrent bootstrap: two goroutines, two different usernames, both
// fire Bootstrap() simultaneously on an empty admin table. The
// LOCK TABLE serialization must yield exactly one winner regardless
// of goroutine scheduling order.
func TestConcurrentBootstrapExactlyOneWinner(t *testing.T) {
	dbPool := createPrivateDB(t)

	const racers = 2
	type outcome struct {
		success bool
		refused bool
		err     error
	}
	outcomes := make(chan outcome, racers)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		go func(i int) {
			username := ""
			if i == 0 {
				username = "racer.alpha"
			} else {
				username = "racer.beta"
			}
			<-start // release both goroutines together
			_, err := Bootstrap(context.Background(), dbPool, username, "Racer "+username, "racer-pass-123")
			oc := outcome{err: err}
			if err == nil {
				oc.success = true
			} else if ae, ok := errors.IsAppError(err); ok && ae.Code == "BOOTSTRAP_REFUSED" {
				oc.refused = true
			}
			outcomes <- oc
		}(i)
	}
	close(start)

	successes, refusals, other := 0, 0, 0
	for i := 0; i < racers; i++ {
		oc := <-outcomes
		switch {
		case oc.success:
			successes++
		case oc.refused:
			refusals++
		default:
			other++
			t.Errorf("unexpected bootstrap error: %v", oc.err)
		}
	}
	if successes != 1 || refusals != 1 || other != 0 {
		t.Fatalf("outcomes: successes=%d refusals=%d other=%d — want 1/1/0", successes, refusals, other)
	}

	ctx := context.Background()
	var admins, superAssign, bootstrapAudits int
	_ = dbPool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_admins`).Scan(&admins)
	_ = dbPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_admin_user_roles ur
		JOIN tb_admin_roles r ON r.id = ur.role_id
		WHERE r.code = 'superadmin'`).Scan(&superAssign)
	_ = dbPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='admin.bootstrap' AND success`).Scan(&bootstrapAudits)
	if admins != 1 {
		t.Fatalf("tb_admins = %d, want 1", admins)
	}
	if superAssign != 1 {
		t.Fatalf("superadmin assignments = %d, want 1", superAssign)
	}
	if bootstrapAudits != 1 {
		t.Fatalf("successful admin.bootstrap audit rows = %d, want 1", bootstrapAudits)
	}
}

// -- RBAC / authorization --

func TestRolePermissionExactAllowAndDeny(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)

	// create a non-superadmin admin holding only admin.users.read
	a := seedAdmin(t, dbPool, "reader-pass-1")
	ctx := context.Background()
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_roles (code, name) VALUES ('reader', 'Reader') ON CONFLICT DO NOTHING`)
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_role_permissions (role_id, permission_code)
		SELECT r.id, p.code FROM tb_admin_roles r, tb_admin_permissions p
		WHERE r.code='reader' AND p.code='admin.users.read' ON CONFLICT DO NOTHING`)
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_user_roles (admin_id, role_id)
		SELECT $1, r.id FROM tb_admin_roles r WHERE r.code='reader' ON CONFLICT DO NOTHING`, a.ID)

	res := loginForTest(t, dbPool, a.Username, "reader-pass-1")
	p, err := ResolveSession(ctx, dbPool, res.RawToken)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !p.HasPermission("admin.users.read") {
		t.Fatal("exact permission must be granted")
	}
	if p.HasPermission("admin.users.manage") {
		t.Fatal("unheld permission must be denied")
	}
	if p.HasPermission("admin.audit.read") {
		t.Fatal("unheld permission must be denied")
	}
}

func TestWildcardSuperadminAllowsAnyPermission(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")
	p, err := ResolveSession(context.Background(), dbPool, res.RawToken)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, code := range []string{"admin.users.read", "admin.users.manage", "admin.roles.manage",
		"admin.sessions.manage", "admin.audit.read", "anything.future"} {
		if !p.HasPermission(code) {
			t.Fatalf("superadmin wildcard must allow %s", code)
		}
	}
}

func TestRoleEditAppliesToExistingSessionImmediately(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	a := seedAdmin(t, dbPool, "reader-pass-1")
	ctx := context.Background()
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_roles (code, name) VALUES ('reader', 'Reader') ON CONFLICT DO NOTHING`)
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_role_permissions (role_id, permission_code)
		SELECT r.id, p.code FROM tb_admin_roles r, tb_admin_permissions p
		WHERE r.code='reader' AND p.code='admin.users.read' ON CONFLICT DO NOTHING`)
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_user_roles (admin_id, role_id)
		SELECT $1, r.id FROM tb_admin_roles r WHERE r.code='reader' ON CONFLICT DO NOTHING`, a.ID)

	res := loginForTest(t, dbPool, a.Username, "reader-pass-1")
	p, _ := ResolveSession(ctx, dbPool, res.RawToken)
	if p.HasPermission("admin.audit.read") {
		t.Fatal("precondition: must not hold audit.read")
	}

	// grant audit.read to the role AFTER the session exists
	_, _ = dbPool.Exec(ctx, `INSERT INTO tb_admin_role_permissions (role_id, permission_code)
		SELECT r.id, 'admin.audit.read' FROM tb_admin_roles r WHERE r.code='reader' ON CONFLICT DO NOTHING`)

	p2, err := ResolveSession(ctx, dbPool, res.RawToken)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !p2.HasPermission("admin.audit.read") {
		t.Fatal("role permission change must be visible to the existing session immediately")
	}
}

// -- CSRF --

func TestCSRFTokenDomainSeparatedAndVerifies(t *testing.T) {
	raw := "some-raw-session-token-value"
	token := CSRFToken(raw, "secret-1")

	if token == "" {
		t.Fatal("empty csrf token")
	}
	// domain separation: differs from a plain HMAC of the token
	if token == CSRFToken("admin-csrf:"+raw, "secret-1") {
		t.Fatal("double-prefix collision")
	}
	// different secret → different token
	if token == CSRFToken(raw, "secret-2") {
		t.Fatal("secret not mixed in")
	}
	// different raw token → different token
	if token == CSRFToken("other-token", "secret-1") {
		t.Fatal("raw token not mixed in")
	}
	// never written to the DB: covered by TestRawSessionTokenNeverPersisted
	// (tb_admin_* has no csrf column — asserted by the guard test in
	// internal/admin/architecture_guard_test.go).
}

// -- Audit --

func TestSuccessfulLoginAtomicSessionAndAudit(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	ctx := context.Background()
	// last_login_at stamped
	var lastLogin *time.Time
	_ = dbPool.QueryRow(ctx, `SELECT last_login_at FROM tb_admins WHERE username='root'`).Scan(&lastLogin)
	if lastLogin == nil {
		t.Fatal("last_login_at not stamped")
	}
	// login audit row exists for this session
	var n int
	err := dbPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_audit_logs
		 WHERE action='admin.login' AND success AND actor_username='root'
		   AND target_type='admin_session' AND target_id = $1`,
		int64ToString(res.Session.ID)).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("login audit rows = %d err=%v", n, err)
	}
}

func int64ToString(id int64) string { return idToString(id) }

func TestPrivilegedMutationAuditFailureRollsBack(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)

	// Inject a CHECK constraint scoped to a marker action so the audit
	// INSERT fails; the business mutation in the same tx must roll back.
	ctx := context.Background()
	_, err := dbPool.Exec(ctx,
		`CREATE CONSTRAINT IF NOT EXISTS chk_audit_rollback_probe ON tb_admin_audit_logs
		 CHECK (action <> 'probe.audit_fail')`)
	if err != nil {
		// postgres lacks CREATE CONSTRAINT IF NOT EXISTS; do it manually
		var exists bool
		_ = dbPool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_constraint WHERE conname='chk_audit_rollback_probe')`).Scan(&exists)
		if !exists {
			_, err = dbPool.Exec(ctx,
				`ALTER TABLE tb_admin_audit_logs ADD CONSTRAINT chk_audit_rollback_probe
				 CHECK (action <> 'probe.audit_fail')`)
			if err != nil {
				t.Fatalf("inject constraint: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = dbPool.Exec(context.Background(), `ALTER TABLE tb_admin_audit_logs DROP CONSTRAINT IF EXISTS chk_audit_rollback_probe`)
	})

	mutated := false
	err = WithAuditTx(ctx, dbPool, &AuditEntry{Action: "probe.audit_fail", Success: true},
		func(ctx context.Context, tx pg.Querier) error {
			mutated = true
			_, err := tx.Exec(ctx, `UPDATE tb_admins SET display_name='PWNED' WHERE username='root'`)
			return err
		})
	if err == nil {
		t.Fatal("audit write must fail and surface the error")
	}
	if !mutated {
		t.Fatal("mutation should have run before the audit failure")
	}
	// the business mutation MUST have been rolled back
	var dn string
	_ = dbPool.QueryRow(ctx, `SELECT display_name FROM tb_admins WHERE username='root'`).Scan(&dn)
	if dn == "PWNED" {
		t.Fatal("mutation survived the audit failure — atomicity broken")
	}
}

func TestSuccessfulPrivilegedMutationAndAuditCommit(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	ctx := context.Background()

	err := WithAuditTx(ctx, dbPool, &AuditEntry{
		Action:  "probe.ok",
		Success: true,
		Actor:   nil,
		After:   map[string]string{"k": "v"},
	}, func(ctx context.Context, tx pg.Querier) error {
		_, err := tx.Exec(ctx, `UPDATE tb_admins SET display_name='Renamed' WHERE username='root'`)
		return err
	})
	if err != nil {
		t.Fatalf("with audit tx: %v", err)
	}
	var dn string
	var n int
	_ = dbPool.QueryRow(ctx, `SELECT display_name FROM tb_admins WHERE username='root'`).Scan(&dn)
	_ = dbPool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='probe.ok' AND success`).Scan(&n)
	if dn != "Renamed" || n != 1 {
		t.Fatalf("mutation=%q audit=%d — both must land", dn, n)
	}
}

// -- append-only semantics --
// The architecture guard (architecture_guard_test.go) enforces that no
// PRODUCTION code UPDATEs/DELETEs the audit table. Here we prove the
// actor-snapshot invariant instead: deleting an admin NULLs the FK but
// leaves the audit rows (and their actor_username snapshot) intact.
func TestAdminDeletionPreservesAuditTrail(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")
	ctx := context.Background()

	var n int
	_ = dbPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE actor_username = 'root'`).Scan(&n)
	if n == 0 {
		t.Fatal("precondition: root audit rows must exist")
	}

	// delete the admin (child rows first, FK order)
	_, _ = dbPool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id = $1`, res.Admin.ID)
	_, _ = dbPool.Exec(ctx, `DELETE FROM tb_admin_sessions WHERE admin_id = $1`, res.Admin.ID)
	if _, err := dbPool.Exec(ctx, `DELETE FROM tb_admins WHERE id = $1`, res.Admin.ID); err != nil {
		t.Fatalf("delete admin: %v", err)
	}

	var kept, nullActor int
	_ = dbPool.QueryRow(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE actor_admin_id IS NULL)
		 FROM tb_admin_audit_logs WHERE actor_username = 'root'`).Scan(&kept, &nullActor)
	if kept != n {
		t.Fatalf("audit rows lost on admin deletion: %d -> %d", n, kept)
	}
	if nullActor != kept {
		t.Fatalf("FK must SET NULL: %d of %d rows still reference the deleted admin", kept-nullActor, kept)
	}
}

// Audit JSON marshaling must never silently drop facts: an
// unmarshalable Before/After value aborts WithAuditTx, the mutation
// rolls back, and no audit row is written.
func TestAuditMarshalFailureRollsBackMutation(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	ctx := context.Background()

	err := WithAuditTx(ctx, dbPool, &AuditEntry{
		Action:  "probe.marshal_fail",
		Success: true,
		Before: map[string]interface{}{
			"unsupported": make(chan int), // json.Marshal cannot handle channels
		},
	}, func(ctx context.Context, tx pg.Querier) error {
		_, err := tx.Exec(ctx, `UPDATE tb_admins SET display_name='PWNED2' WHERE username='root'`)
		return err
	})
	if err == nil {
		t.Fatal("WithAuditTx must fail on unmarshalable audit facts")
	}
	ae, ok := errors.IsAppError(err)
	if !ok || ae.Code != "AUDIT_MARSHAL_FAILED" {
		t.Fatalf("expected AUDIT_MARSHAL_FAILED, got %v", err)
	}

	// mutation rolled back
	var dn string
	_ = dbPool.QueryRow(ctx, `SELECT display_name FROM tb_admins WHERE username='root'`).Scan(&dn)
	if dn == "PWNED2" {
		t.Fatal("mutation survived audit marshal failure")
	}
	// audit row = 0
	var n int
	_ = dbPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action='probe.marshal_fail'`).Scan(&n)
	if n != 0 {
		t.Fatalf("audit rows for failed marshal = %d, want 0", n)
	}
}

// Idle boundary: exactly 30 minutes since last_seen_at → expired
// (now >= last_seen_at + 30m). One second earlier → still valid.
func TestSessionIdleExactly30MinutesIsExpired(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	setLastSeen := func(offset time.Duration) {
		t.Helper()
		_, err := dbPool.Exec(context.Background(),
			`UPDATE tb_admin_sessions
			 SET last_seen_at = CURRENT_TIMESTAMP - $1::interval,
			     expires_at = CURRENT_TIMESTAMP + interval '11 hours'
			 WHERE id = $2`,
			fmt.Sprintf("%.0f seconds", offset.Seconds()), res.Session.ID)
		if err != nil {
			t.Fatalf("set last_seen: %v", err)
		}
	}

	// 29m59s → alive
	setLastSeen(29*time.Minute + 59*time.Second)
	if _, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil {
		t.Fatalf("29m59s idle must still resolve: %v", err)
	}
	// exactly 30m → expired
	setLastSeen(30 * time.Minute)
	if p, err := ResolveSession(context.Background(), dbPool, res.RawToken); err == nil || p != nil {
		t.Fatal("exactly 30m idle must be expired (>= boundary)")
	}
}

// Touch boundary: exactly 5 minutes since last_seen_at → touch fires
// (now >= last_seen_at + 5m). One second earlier → no write.
func TestSessionTouchAtExactly5Minutes(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	res := loginForTest(t, dbPool, "root", "password-123")

	lastSeen := func() time.Time {
		t.Helper()
		var ts time.Time
		if err := dbPool.QueryRow(context.Background(),
			`SELECT last_seen_at FROM tb_admin_sessions WHERE id = $1`, res.Session.ID).Scan(&ts); err != nil {
			t.Fatalf("read last_seen: %v", err)
		}
		return ts
	}
	setLastSeen := func(offset time.Duration) {
		t.Helper()
		_, err := dbPool.Exec(context.Background(),
			`UPDATE tb_admin_sessions
			 SET last_seen_at = CURRENT_TIMESTAMP - $1::interval
			 WHERE id = $2`,
			fmt.Sprintf("%.0f seconds", offset.Seconds()), res.Session.ID)
		if err != nil {
			t.Fatalf("set last_seen: %v", err)
		}
	}

	// 4m59s stale → resolve does NOT touch
	setLastSeen(4*time.Minute + 59*time.Second)
	before := lastSeen()
	if _, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if after := lastSeen(); !after.Equal(before) {
		t.Fatal("touch must not fire before the 5m boundary")
	}

	// exactly 5m stale → resolve MUST touch
	setLastSeen(5 * time.Minute)
	before = lastSeen()
	if _, err := ResolveSession(context.Background(), dbPool, res.RawToken); err != nil {
		t.Fatalf("resolve at boundary: %v", err)
	}
	if after := lastSeen(); !after.After(before) {
		t.Fatal("touch must fire at exactly 5m (>= boundary)")
	}
}
