package admin

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// B1.2 Admin Management domain: accounts, roles, permissions
// visibility, assignments, sessions, passwords, audit explorer.
//
// Every privileged mutation runs inside ONE WithAuditTx transaction:
// business mutation + (bump/revoke where required) + audit INSERT.
// No username/admin-id special-casing anywhere; superadmin
// membership changes require the ACTOR to hold the wildcard.

var roleCodePattern = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

const superadminRoleCode = "superadmin"

// lastSuperadminError is the invariant failure.
func lastSuperadminError() error {
	return errors.New(409, "LAST_SUPERADMIN_REQUIRED",
		"Operation refused: the platform must retain at least one active superadmin")
}

// -- Admin accounts --

// CreateAdminInput carries account-creation facts.
type CreateAdminInput struct {
	Username    string
	DisplayName string
	Password    string
	Actor       *model.Admin
}

// CreateAdmin creates a new active admin (no implicit roles).
// Audit records the username/status; never the password material.
func CreateAdmin(ctx context.Context, pool *pg.Pool, principal *Principal, in CreateAdminInput) (*model.Admin, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.users.manage"); err != nil {
		return nil, err
	}
	username := NormalizeUsername(in.Username)
	if !ValidateUsername(username) {
		return nil, errors.New(400, "INVALID_USERNAME",
			"Username must be 3-64 chars of lowercase letters, digits, '.', '_' or '-'")
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" || len(displayName) > 128 {
		return nil, errors.New(400, "INVALID_DISPLAY_NAME", "Display name must be 1-128 characters")
	}
	if len(in.Password) < 8 {
		return nil, errors.New(400, "INVALID_PASSWORD", "Password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to hash password")
	}

	var created *model.Admin
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.user.create",
		TargetType: "admin",
		Success:    true,
	}
	err = WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		row, err := repository.InsertAdminWithRoles(ctx, tx, &model.Admin{
			Username:     username,
			DisplayName:  displayName,
			PasswordHash: hash,
			Status:       "active",
		})
		if err == repository.ErrAdminUsernameExists {
			return errors.New(409, "ADMIN_USERNAME_EXISTS", "Username already exists")
		}
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Failed to create admin")
		}
		created = row
		entry.TargetID = idToString(row.ID)
		entry.After = map[string]interface{}{
			"id": row.ID, "username": row.Username,
			"display_name": row.DisplayName, "status": row.Status,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	created.PasswordHash = "" // never serialize the hash upward
	return created, nil
}

// UpdateAdminDisplayName patches display_name (username immutable).
func UpdateAdminDisplayName(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64, displayName string) (*model.Admin, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.users.manage"); err != nil {
		return nil, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || len(displayName) > 128 {
		return nil, errors.New(400, "INVALID_DISPLAY_NAME", "Display name must be 1-128 characters")
	}

	var updated *model.Admin
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.user.update",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
	}
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		before, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || before == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		if err := repository.UpdateAdminDisplayName(ctx, tx, adminID, displayName); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		entry.Before = map[string]interface{}{"display_name": before.DisplayName}
		entry.After = map[string]interface{}{"display_name": displayName}
		updated = before
		updated.DisplayName = displayName
		updated.PasswordHash = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DisableAdmin: active → disabled + auth_version bump + revoke-all
// sessions + audit, one transaction. Idempotent no-op when already
// disabled. Never allowed to break the last-active-superadmin
// invariant.
func DisableAdmin(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64) error {
	if err := RequirePermission(ctx, pool, principal, "admin.users.manage"); err != nil {
		return err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.user.disable",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		// GLOBAL invariant serialization: lock the stable superadmin
		// system role row FIRST, then the admin rows ascending. Two
		// superadmins concurrently disabling themselves serialize on
		// the role row; the second re-reads state and sees the count
		// already at 1.
		if err := repository.LockSuperadminInvariant(ctx, tx); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		target, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || target == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		if target.Status == "disabled" {
			entry.Before = map[string]interface{}{"status": target.Status}
			entry.After = map[string]interface{}{"status": target.Status}
			return nil // idempotent no-op, still audited with real facts
		}
		// Last-active-superadmin invariant, re-read UNDER the locks.
		isSuper, err := repository.IsActiveSuperadmin(ctx, tx, adminID)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if isSuper {
			n, err := repository.CountActiveSuperadmins(ctx, tx)
			if err != nil {
				return errors.New(500, "INTERNAL_ERROR", "Database error")
			}
			if n <= 1 {
				return lastSuperadminError()
			}
		}
		if err := repository.SetAdminStatus(ctx, tx, adminID, "disabled"); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		entry.Before = map[string]interface{}{"status": target.Status}
		entry.After = map[string]interface{}{"status": "disabled"}
		return bumpAuthVersionAndRevokeSessions(ctx, tx, adminID)
	})
}

// EnableAdmin: disabled → active. Sessions stay revoked (no restore).
func EnableAdmin(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64) error {
	if err := RequirePermission(ctx, pool, principal, "admin.users.manage"); err != nil {
		return err
	}
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.user.enable",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		target, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || target == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		if target.Status == "active" {
			entry.Before = map[string]interface{}{"status": target.Status}
			entry.After = map[string]interface{}{"status": target.Status}
			return nil // idempotent no-op
		}
		if err := repository.SetAdminStatus(ctx, tx, adminID, "active"); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		entry.Before = map[string]interface{}{"status": target.Status}
		entry.After = map[string]interface{}{"status": "active"}
		return nil
	})
}

// -- Passwords --

// ChangeOwnPassword verifies the current password, sets the new one,
// bumps auth_version, revokes ALL sessions (including the current
// one), and audits — one transaction. Available to ANY authenticated
// active admin; no admin.users.manage required.
func ChangeOwnPassword(ctx context.Context, pool *pg.Pool, principal *Principal, currentPassword, newPassword string) error {
	if newPassword == currentPassword {
		return errors.New(400, "INVALID_PASSWORD", "New password must differ from the current password")
	}
	if len(newPassword) < 8 {
		return errors.New(400, "INVALID_PASSWORD", "Password must be at least 8 characters")
	}
	if !auth.VerifyPassword(currentPassword, principal.Admin.PasswordHash) {
		return errors.New(401, "INVALID_CREDENTIALS", "Current password is incorrect")
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Failed to hash password")
	}
	return WithAuditTx(ctx, pool, &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.password.change",
		TargetType: "admin",
		TargetID:   idToString(principal.Admin.ID),
		Success:    true,
		Metadata:   map[string]interface{}{"sessions_revoked": "all"},
	}, func(ctx context.Context, tx pg.Querier) error {
		if err := repository.UpdateAdminPassword(ctx, tx, principal.Admin.ID, hash); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		return bumpAuthVersionAndRevokeSessions(ctx, tx, principal.Admin.ID)
	})
}

// ResetAdminPassword sets a caller-supplied new password for another
// admin (admin.users.manage). No temporary password generation; the
// plaintext never reaches the audit row.
func ResetAdminPassword(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64, newPassword string) error {
	if err := RequirePermission(ctx, pool, principal, "admin.users.manage"); err != nil {
		return err
	}
	if len(newPassword) < 8 {
		return errors.New(400, "INVALID_PASSWORD", "Password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Failed to hash password")
	}
	return WithAuditTx(ctx, pool, &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.password.reset",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
		Metadata:   map[string]interface{}{"sessions_revoked": "all"},
	}, func(ctx context.Context, tx pg.Querier) error {
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		target, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || target == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		if err := repository.UpdateAdminPassword(ctx, tx, adminID, hash); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		return bumpAuthVersionAndRevokeSessions(ctx, tx, adminID)
	})
}

// -- Roles --

// CreateRoleInput carries role-creation facts.
type CreateRoleInput struct {
	Code        string
	Name        string
	Description string
}

// normalizeRoleCode lowercases a custom role code.
func normalizeRoleCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// CreateRole creates a custom (non-system) role.
func CreateRole(ctx context.Context, pool *pg.Pool, principal *Principal, in CreateRoleInput) (*model.AdminRole, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.manage"); err != nil {
		return nil, err
	}
	code := normalizeRoleCode(in.Code)
	if !roleCodePattern.MatchString(code) {
		return nil, errors.New(400, "INVALID_ROLE_CODE",
			"Role code must be 3-64 chars of lowercase letters, digits, '.', '_' or '-'")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 128 {
		return nil, errors.New(400, "INVALID_ROLE_NAME", "Role name must be 1-128 characters")
	}
	var created *model.AdminRole
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.role.create",
		TargetType: "admin_role",
		Success:    true,
	}
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		row, err := repository.InsertAdminRole(ctx, tx, code, name, in.Description)
		if err == repository.ErrAdminRoleCodeExists {
			return errors.New(409, "ADMIN_ROLE_CODE_EXISTS", "Role code already exists")
		}
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Failed to create role")
		}
		created = row
		entry.TargetID = idToString(row.ID)
		afterFacts := map[string]interface{}{
			"id": row.ID, "code": row.Code, "name": row.Name, "status": row.Status,
		}
		if row.Description != nil {
			afterFacts["description"] = *row.Description
		}
		entry.After = afterFacts
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// RolePatch carries OPTIONAL fields for a partial PATCH. nil means
// "field not provided" (preserve the existing value); a non-nil
// pointer means "set to this value" (including an explicit empty
// description). Provided-vs-omitted is structurally distinguishable.
type RolePatch struct {
	Name        *string
	Description *string
	Status      *string
}

// UpdateRole applies a PARTIAL patch to a CUSTOM role: omitted fields
// keep their current values. At least one field must be provided.
// System roles are structurally immutable (repository guard).
func UpdateRole(ctx context.Context, pool *pg.Pool, principal *Principal, roleID int64, patch RolePatch) (*model.AdminRole, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.manage"); err != nil {
		return nil, err
	}
	if patch.Name == nil && patch.Description == nil && patch.Status == nil {
		return nil, errors.New(400, "EMPTY_PATCH", "PATCH must include at least one of name, description, status")
	}
	if patch.Name != nil {
		trimmed := strings.TrimSpace(*patch.Name)
		if trimmed == "" || len(trimmed) > 128 {
			return nil, errors.New(400, "INVALID_ROLE_NAME", "Role name must be 1-128 characters")
		}
		patch.Name = &trimmed
	}
	if patch.Description != nil && len(*patch.Description) > 500 {
		return nil, errors.New(400, "INVALID_ROLE_DESCRIPTION", "Role description must be at most 500 characters")
	}
	if patch.Status != nil && *patch.Status != "active" && *patch.Status != "disabled" {
		return nil, errors.New(400, "INVALID_ROLE_STATUS", "Role status must be active or disabled")
	}

	var updated *model.AdminRole
	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.role.update",
		TargetType: "admin_role",
		TargetID:   idToString(roleID),
		Success:    true,
	}
	err := WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		if err := repository.LockAdminRoleRowForUpdate(ctx, tx, roleID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		role, err := repository.FindAdminRoleByID(ctx, tx, roleID)
		if err != nil || role == nil {
			return errors.New(404, "ROLE_NOT_FOUND", "Role not found")
		}
		if role.IsSystem {
			return errors.New(409, "ROLE_IS_SYSTEM", "System roles are immutable")
		}
		// merge patch over current values (omitted = preserved)
		nextName := role.Name
		if patch.Name != nil {
			nextName = *patch.Name
		}
		nextDesc := ""
		if role.Description != nil {
			nextDesc = *role.Description
		}
		if patch.Description != nil {
			nextDesc = *patch.Description
		}
		nextStatus := role.Status
		if patch.Status != nil {
			nextStatus = *patch.Status
		}
		beforeFacts := map[string]interface{}{
			"name": role.Name, "status": role.Status,
		}
		if role.Description != nil {
			beforeFacts["description"] = *role.Description
		}
		if err := repository.UpdateAdminRole(ctx, tx, roleID, nextName, nextDesc, nextStatus); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		role.Name = nextName
		role.Description = &nextDesc
		role.Status = nextStatus
		updated = role
		afterFacts := map[string]interface{}{"name": nextName, "status": nextStatus}
		if patch.Description != nil {
			afterFacts["description"] = nextDesc
		}
		entry.Before = beforeFacts
		entry.After = afterFacts
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// SetRolePermissions replaces the role's permission bindings
// (complete replacement). System roles and wildcard grants to custom
// roles are refused.
func SetRolePermissions(ctx context.Context, pool *pg.Pool, principal *Principal, roleID int64, permissionCodes []string) error {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.manage"); err != nil {
		return err
	}
	// validate EVERY element; blank-after-trim entries fail the whole
	// request with zero mutation (fail closed).
	seen := map[string]bool{}
	normalized := make([]string, 0, len(permissionCodes))
	for _, c := range permissionCodes {
		c = strings.TrimSpace(c)
		if c == "" {
			return errors.New(400, "INVALID_PERMISSION_CODES",
				"permission_codes entries must be non-empty strings")
		}
		if seen[c] {
			continue // duplicates are collapsed, not an error
		}
		seen[c] = true
		normalized = append(normalized, c)
	}
	sort.Strings(normalized)

	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.role.permissions.set",
		TargetType: "admin_role",
		TargetID:   idToString(roleID),
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		if err := repository.LockAdminRoleRowForUpdate(ctx, tx, roleID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		role, err := repository.FindAdminRoleByID(ctx, tx, roleID)
		if err != nil || role == nil {
			return errors.New(404, "ROLE_NOT_FOUND", "Role not found")
		}
		if role.IsSystem {
			return errors.New(409, "ROLE_IS_SYSTEM", "System role permissions are immutable")
		}
		for _, c := range normalized {
			if c == WildcardPermission {
				return errors.New(403, "WILDCARD_NOT_ASSIGNABLE",
					"The wildcard permission cannot be granted to a custom role")
			}
		}
		existing, err := repository.PermissionCodesExist(ctx, tx, normalized)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if len(existing) != len(normalized) {
			missing := diffStrings(normalized, existing)
			return errors.New(400, "PERMISSION_NOT_FOUND",
				"Unknown permission codes: "+strings.Join(missing, ", "))
		}
		beforePerms, err := repository.ListAdminPermissionCodesByRole(ctx, tx, roleID)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if err := repository.ReplaceAdminRolePermissions(ctx, tx, roleID, normalized); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		entry.Before = map[string]interface{}{"permission_codes": beforePerms}
		entry.After = map[string]interface{}{"permission_codes": normalized}
		return nil
	})
}

func diffStrings(want, have []string) []string {
	hs := map[string]bool{}
	for _, h := range have {
		hs[h] = true
	}
	var out []string
	for _, w := range want {
		if !hs[w] {
			out = append(out, w)
		}
	}
	return out
}

// -- Admin ↔ role assignment --

// SetAdminRoles replaces the target admin's role bindings (complete
// replacement). Superadmin membership changes (adding OR removing the
// superadmin system role) require the ACTOR to hold the wildcard.
// The last-active-superadmin invariant is enforced under a fixed-order
// lock (target admin first, then all affected admins ascending).
func SetAdminRoles(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64, roleIDs []int64) error {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.manage"); err != nil {
		return err
	}
	// validate EVERY element; any invalid entry fails the whole
	// request with zero mutation (fail closed).
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(roleIDs))
	for _, id := range roleIDs {
		if id <= 0 {
			return errors.New(400, "INVALID_ROLE_IDS",
				"role_ids entries must be positive integers")
		}
		if seen[id] {
			continue // duplicates are collapsed, not an error
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.user.roles.set",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		// GLOBAL invariant serialization FIRST: this operation can
		// drop superadmin membership (an active-superadmin count
		// reducer), so it must hold the superadmin role-row lock
		// before reading state.
		if err := repository.LockSuperadminInvariant(ctx, tx); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		target, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || target == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		existing, err := repository.RoleIDsExist(ctx, tx, ids)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if len(existing) != len(ids) {
			return errors.New(400, "ROLE_NOT_FOUND", "One or more role ids do not exist")
		}
		// resolve which roles are the superadmin system role
		superRole, err := repository.FindAdminRoleByCode(ctx, tx, superadminRoleCode)
		if err != nil || superRole == nil {
			return errors.New(500, "INTERNAL_ERROR", "superadmin system role missing")
		}

		beforeIDs, err := repository.ListAdminUserRoleIDs(ctx, tx, adminID)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		beforeHasSuper := containsInt64(beforeIDs, superRole.ID)
		afterHasSuper := containsInt64(ids, superRole.ID)

		// superadmin membership change requires actor wildcard
		if beforeHasSuper != afterHasSuper && !principal.HasPermission(WildcardPermission) {
			return errors.New(403, "SUPERADMIN_MEMBERSHIP_FORBIDDEN",
				"Changing superadmin membership requires the wildcard permission")
		}

		if err := repository.ReplaceAdminUserRoles(ctx, tx, adminID, ids); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}

		// last-active-superadmin invariant, re-read UNDER the locks
		// (the replacement already happened in this tx; the count
		// reflects the post-replacement state, so >=1 must remain).
		if beforeHasSuper && !afterHasSuper && target.Status == "active" {
			n, err := repository.CountActiveSuperadmins(ctx, tx)
			if err != nil {
				return errors.New(500, "INTERNAL_ERROR", "Database error")
			}
			if n < 1 {
				return lastSuperadminError()
			}
		}
		entry.Before = map[string]interface{}{"role_ids": beforeIDs}
		entry.After = map[string]interface{}{"role_ids": ids}
		return nil
	})
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// -- Sessions --

// RevokeSessionByID revokes ONE session (no auth_version bump for the
// target admin). Returns the session info for the handler (which
// clears the actor's own cookie when revoking the current session).
func RevokeSessionByID(ctx context.Context, pool *pg.Pool, principal *Principal, sessionID int64) (*model.AdminSessionInfo, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.sessions.manage"); err != nil {
		return nil, err
	}
	var revoked *model.AdminSessionInfo
	err := WithAuditTx(ctx, pool, &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.session.revoke",
		TargetType: "admin_session",
		TargetID:   idToString(sessionID),
		Success:    true,
	}, func(ctx context.Context, tx pg.Querier) error {
		info, err := repository.FindAdminSessionByID(ctx, tx, sessionID)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if info == nil {
			return errors.New(404, "SESSION_NOT_FOUND", "Session not found")
		}
		if err := repository.RevokeAdminSessionByIDTx(ctx, tx, sessionID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		revoked = info
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// ForceLogoutAdmin is an account-wide security event: auth_version
// bump + revoke all active sessions + audit, one transaction.
func ForceLogoutAdmin(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64) error {
	if err := RequirePermission(ctx, pool, principal, "admin.sessions.manage"); err != nil {
		return err
	}
	return WithAuditTx(ctx, pool, &AuditEntry{
		Actor:      principal.Admin,
		Action:     "admin.sessions.force_logout",
		TargetType: "admin",
		TargetID:   idToString(adminID),
		Success:    true,
		Metadata:   map[string]interface{}{"sessions_revoked": "all"},
	}, func(ctx context.Context, tx pg.Querier) error {
		if err := lockAdminRowsOrdered(ctx, tx, principal.Admin.ID, adminID); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		target, err := repository.FindAdminByID(ctx, tx, adminID)
		if err != nil || target == nil {
			return errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
		}
		return bumpAuthVersionAndRevokeSessions(ctx, tx, adminID)
	})
}
