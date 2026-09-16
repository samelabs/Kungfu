package admin

// B1.2 repair tests: global invariant serialization races, partial
// role PATCH, fail-closed replacement payloads, real audit facts.

import (
	"context"
	"encoding/json"
	"testing"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/repository"
)

// Two active superadmins concurrently DisableAdmin(SELF). The global
// invariant lock must let at most one succeed; >=1 active superadmin
// remains.
func TestRepairConcurrentSelfDisable(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, superadminRoleCode)

	second := b12CreateUserDirect(t, dbPool, "selfdis.super", "selfdis-pass-1", nil)
	if err := SetAdminRoles(context.Background(), dbPool, root, second.ID, []int64{super.ID}); err != nil {
		t.Fatalf("promote second: %v", err)
	}

	rootP := b12PrincipalOf(t, dbPool, "root", "password-123")
	secondP := b12PrincipalOf(t, dbPool, "selfdis.super", "selfdis-pass-1")

	outcomes := make(chan error, 2)
	start := make(chan struct{})
	go func() { <-start; outcomes <- DisableAdmin(context.Background(), dbPool, rootP, rootP.Admin.ID) }()
	go func() { <-start; outcomes <- DisableAdmin(context.Background(), dbPool, secondP, secondP.Admin.ID) }()
	close(start)

	successes, refusals, other := 0, 0, 0
	for i := 0; i < 2; i++ {
		err := <-outcomes
		switch {
		case err == nil:
			successes++
		default:
			if ae, ok := errors.IsAppError(err); ok && ae.Code == "LAST_SUPERADMIN_REQUIRED" {
				refusals++
			} else {
				other++
				t.Errorf("unexpected: %v", err)
			}
		}
	}
	if successes > 1 {
		t.Fatalf("both self-disables succeeded — invariant broken")
	}
	if successes+refusals+other != 2 {
		t.Fatalf("outcome mismatch: %d/%d/%d", successes, refusals, other)
	}
	n, _ := repository.CountActiveSuperadmins(context.Background(), dbPool)
	if n < 1 {
		t.Fatalf("active superadmins = %d, want >= 1", n)
	}
}

// Two active superadmins concurrently SetAdminRoles(SELF, empty).
func TestRepairConcurrentSelfDemote(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, superadminRoleCode)

	second := b12CreateUserDirect(t, dbPool, "selfdemo.super", "selfdemo-pass-1", nil)
	if err := SetAdminRoles(context.Background(), dbPool, root, second.ID, []int64{super.ID}); err != nil {
		t.Fatalf("promote second: %v", err)
	}

	rootP := b12PrincipalOf(t, dbPool, "root", "password-123")
	secondP := b12PrincipalOf(t, dbPool, "selfdemo.super", "selfdemo-pass-1")

	outcomes := make(chan error, 2)
	start := make(chan struct{})
	go func() { <-start; outcomes <- SetAdminRoles(context.Background(), dbPool, rootP, rootP.Admin.ID, nil) }()
	go func() {
		<-start
		outcomes <- SetAdminRoles(context.Background(), dbPool, secondP, secondP.Admin.ID, nil)
	}()
	close(start)

	successes, refusals := 0, 0
	for i := 0; i < 2; i++ {
		err := <-outcomes
		if err == nil {
			successes++
		} else if ae, ok := errors.IsAppError(err); ok && ae.Code == "LAST_SUPERADMIN_REQUIRED" {
			refusals++
		} else {
			t.Errorf("unexpected: %v", err)
		}
	}
	if successes > 1 {
		t.Fatalf("both self-demotions succeeded — invariant broken")
	}
	n, _ := repository.CountActiveSuperadmins(context.Background(), dbPool)
	if n < 1 {
		t.Fatalf("active superadmins = %d, want >= 1", n)
	}
}

// Partial PATCH: disable → PATCH name only → still disabled; a
// session held through a (re-enabled) role does not regain disabled-
// role permissions without re-resolution.
func TestRepairPartialPatchSemantics(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)

	role, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "part.role", Name: "Partial"})
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID,
		[]string{"admin.audit.read"}); err != nil {
		t.Fatalf("set perms: %v", err)
	}
	holder := b12CreateUserDirect(t, dbPool, "part.holder", "part-pass-123", []string{"part.role"})
	raw, _ := b12Login(t, dbPool, holder.Username, "part-pass-123")

	// holder currently has audit.read
	if p, _ := ResolveSession(context.Background(), dbPool, raw); !p.HasPermission("admin.audit.read") {
		t.Fatal("precondition: holder has audit.read")
	}

	// disable the role (status-only patch)
	st := "disabled"
	if _, err := UpdateRole(context.Background(), dbPool, root, role.ID, RolePatch{Status: &st}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// live RBAC must stop granting immediately, same session
	if p, _ := ResolveSession(context.Background(), dbPool, raw); p.HasPermission("admin.audit.read") {
		t.Fatal("disabled role must stop granting permissions immediately")
	}

	// PATCH name only → status must remain disabled
	newName := "Renamed Partial"
	updated, err := UpdateRole(context.Background(), dbPool, root, role.ID, RolePatch{Name: &newName})
	if err != nil {
		t.Fatalf("name-only patch: %v", err)
	}
	if updated.Status != "disabled" {
		t.Fatalf("name-only PATCH must preserve disabled status, got %s", updated.Status)
	}
	if updated.Name != "Renamed Partial" {
		t.Fatalf("name not applied: %s", updated.Name)
	}
	// description preserved too (never set → empty)
	// session still denied
	if p, _ := ResolveSession(context.Background(), dbPool, raw); p.HasPermission("admin.audit.read") {
		t.Fatal("permission must stay denied while role disabled")
	}

	// PATCH status only (re-enable) must succeed
	act := "active"
	if _, err := UpdateRole(context.Background(), dbPool, root, role.ID, RolePatch{Status: &act}); err != nil {
		t.Fatalf("status-only re-enable: %v", err)
	}
	if p, _ := ResolveSession(context.Background(), dbPool, raw); !p.HasPermission("admin.audit.read") {
		t.Fatal("re-enabled role must grant again immediately")
	}

	// empty patch → 400
	if _, err := UpdateRole(context.Background(), dbPool, root, role.ID, RolePatch{}); err == nil {
		t.Fatal("empty patch must 400")
	} else if ae, _ := errors.IsAppError(err); ae == nil || ae.Code != "EMPTY_PATCH" {
		t.Fatalf("expected EMPTY_PATCH, got %v", err)
	}
}

// Fail-closed payloads at the DOMAIN level: invalid entries reject
// the whole request with zero mutation.
func TestRepairFailClosedPayloadsDomain(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	role, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "fc.role", Name: "FC"})
	target := b12CreateUserDirect(t, dbPool, "fc.target", "fc-pass-123", nil)
	super, _ := repository.FindAdminRoleByCode(context.Background(), dbPool, superadminRoleCode)

	// baseline binding
	if err := SetAdminRoles(context.Background(), dbPool, root, target.ID, []int64{role.ID}); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// negative role id → whole request fails, binding unchanged
	if err := SetAdminRoles(context.Background(), dbPool, root, target.ID, []int64{role.ID, -5}); err == nil {
		t.Fatal("negative role id must fail")
	}
	after, _ := repository.ListAdminUserRoleIDs(context.Background(), dbPool, target.ID)
	if len(after) != 1 || after[0] != role.ID {
		t.Fatalf("binding changed by rejected request: %v", after)
	}

	// permission codes: blank entry → fail, bindings unchanged
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID,
		[]string{"admin.audit.read", "   "}); err == nil {
		t.Fatal("blank permission entry must fail")
	}
	perms, _ := repository.ListAdminPermissionCodesByRole(context.Background(), dbPool, role.ID)
	if len(perms) != 0 {
		t.Fatalf("permission binding changed by rejected request: %v", perms)
	}
	_ = super
}

// Real before/after audit facts for replacement operations.
func TestRepairAuditFactsAreRealState(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	// roles.set: before contains superadmin (bootstrap), after contains only viewer role
	role, _ := CreateRole(context.Background(), dbPool, root, CreateRoleInput{Code: "audit.role", Name: "A"})
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID, []string{"admin.users.read"}); err != nil {
		t.Fatalf("perm set 1: %v", err)
	}

	target := b12CreateUserDirect(t, dbPool, "audit.target", "audit-pass-1", []string{"superadmin", "audit.role"})
	if err := SetAdminRoles(context.Background(), dbPool, root, target.ID, []int64{role.ID}); err != nil {
		t.Fatalf("roles set: %v", err)
	}

	var beforeJSON, afterJSON []byte
	err := dbPool.QueryRow(context.Background(),
		`SELECT before_json, after_json FROM tb_admin_audit_logs
		 WHERE action='admin.user.roles.set' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&beforeJSON, &afterJSON)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var before, after map[string]interface{}
	_ = json.Unmarshal(beforeJSON, &before)
	_ = json.Unmarshal(afterJSON, &after)
	beforeIDs := idsOf(before["role_ids"])
	afterIDs := idsOf(after["role_ids"])
	if len(beforeIDs) != 2 || len(afterIDs) != 1 || afterIDs[0] != role.ID {
		t.Fatalf("audit facts not real state: before=%v after=%v", beforeIDs, afterIDs)
	}

	// permissions.set: before = [admin.users.read], after = [admin.roles.read]
	if err := SetRolePermissions(context.Background(), dbPool, root, role.ID, []string{"admin.roles.read"}); err != nil {
		t.Fatalf("perm set 2: %v", err)
	}
	err = dbPool.QueryRow(context.Background(),
		`SELECT before_json, after_json FROM tb_admin_audit_logs
		 WHERE action='admin.role.permissions.set' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&beforeJSON, &afterJSON)
	if err != nil {
		t.Fatalf("read perm audit: %v", err)
	}
	_ = json.Unmarshal(beforeJSON, &before)
	_ = json.Unmarshal(afterJSON, &after)
	bcodes, _ := before["permission_codes"].([]interface{})
	acodes, _ := after["permission_codes"].([]interface{})
	if len(bcodes) != 1 || bcodes[0] != "admin.users.read" {
		t.Fatalf("perm audit before not real: %v", before)
	}
	if len(acodes) != 1 || acodes[0] != "admin.roles.read" {
		t.Fatalf("perm audit after not real: %v", after)
	}

	// user.create: TargetID = new admin id, after carries full facts
	created, err := CreateAdmin(context.Background(), dbPool, root, CreateAdminInput{
		Username: "audit.new", DisplayName: "Audit New", Password: "audit-pass-99",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var targetID *string
	var afterCreate map[string]interface{}
	err = dbPool.QueryRow(context.Background(),
		`SELECT target_id, after_json FROM tb_admin_audit_logs
		 WHERE action='admin.user.create' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&targetID, &afterJSON)
	if err != nil || targetID == nil {
		t.Fatalf("create audit target: %v", err)
	}
	if *targetID != idToString(created.ID) {
		t.Fatalf("create audit TargetID = %s, want %d", *targetID, created.ID)
	}
	_ = json.Unmarshal(afterJSON, &afterCreate)
	if afterCreate["username"] != "audit.new" || afterCreate["status"] != "active" {
		t.Fatalf("create audit facts: %v", afterCreate)
	}

	// user.update: real before/after display names
	if _, err := UpdateAdminDisplayName(context.Background(), dbPool, root, created.ID, "Renamed Audit"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var b, a map[string]interface{}
	err = dbPool.QueryRow(context.Background(),
		`SELECT before_json, after_json FROM tb_admin_audit_logs
		 WHERE action='admin.user.update' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&beforeJSON, &afterJSON)
	if err != nil {
		t.Fatalf("update audit: %v", err)
	}
	_ = json.Unmarshal(beforeJSON, &b)
	_ = json.Unmarshal(afterJSON, &a)
	if b["display_name"] != "Audit New" || a["display_name"] != "Renamed Audit" {
		t.Fatalf("update audit facts: before=%v after=%v", b, a)
	}

	// disable: real before/after status
	if err := DisableAdmin(context.Background(), dbPool, root, created.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	err = dbPool.QueryRow(context.Background(),
		`SELECT before_json, after_json FROM tb_admin_audit_logs
		 WHERE action='admin.user.disable' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&beforeJSON, &afterJSON)
	if err != nil {
		t.Fatalf("disable audit: %v", err)
	}
	_ = json.Unmarshal(beforeJSON, &b)
	_ = json.Unmarshal(afterJSON, &a)
	if b["status"] != "active" || a["status"] != "disabled" {
		t.Fatalf("disable audit facts: before=%v after=%v", b, a)
	}

	// no placeholder may remain anywhere
	var n int
	_ = dbPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE after_json::text LIKE '%pre-replacement%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("placeholder audit facts remain: %d rows", n)
	}
}

func idsOf(v interface{}) []int64 {
	raw, _ := v.([]interface{})
	out := make([]int64, 0, len(raw))
	for _, x := range raw {
		if f, ok := x.(float64); ok {
			out = append(out, int64(f))
		}
	}
	return out
}
