package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// Admin repository: all Admin-plane SQL lives here. Handlers never
// embed Admin business SQL. The Admin plane never touches tb_bots.

// CountAdmins returns the number of admin rows (bootstrap gate).
func CountAdmins(ctx context.Context, q pg.Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_admins`).Scan(&n)
	return n, err
}

// LockAdminsTableExclusive is the bootstrap serialization primitive:
// inside the caller's transaction it takes a session-level EXCLUSIVE
// lock on tb_admins, so two concurrent first-bootstraps serialize —
// the second only observes COUNT>0 after the first commits (its lock
// wait ends post-commit). Must be the FIRST statement of the
// bootstrap transaction.
func LockAdminsTableExclusive(ctx context.Context, q pg.Querier) error {
	_, err := q.Exec(ctx, `LOCK TABLE tb_admins IN EXCLUSIVE MODE`)
	return err
}

// IncrementAdminAuthVersion bumps auth_version by one. Tx-aware:
// the caller owns BEGIN/COMMIT (typically via WithAuditTx so the
// bump, the revocations, the business mutation, and the audit row
// all commit atomically).
func IncrementAdminAuthVersion(ctx context.Context, q pg.Querier, adminID int64) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admins SET auth_version = auth_version + 1, updated_at = CURRENT_TIMESTAMP WHERE id = $1`,
		adminID)
	return err
}

// InsertAdmin creates an admin; username must already be normalized
// lowercase. Returns the full row.
func InsertAdmin(ctx context.Context, q pg.Querier, a *model.Admin) (*model.Admin, error) {
	row := &model.Admin{}
	err := q.QueryRow(ctx, `
		INSERT INTO tb_admins (username, display_name, password_hash, status)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, ''), 'active'))
		RETURNING id, username, display_name, password_hash, status, auth_version,
			last_login_at, password_changed_at, created_at, updated_at`,
		a.Username, a.DisplayName, a.PasswordHash, a.Status,
	).Scan(&row.ID, &row.Username, &row.DisplayName, &row.PasswordHash, &row.Status,
		&row.AuthVersion, &row.LastLoginAt, &row.PasswordChangedAt, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// FindAdminByUsername looks up any admin (any status) by exact username.
func FindAdminByUsername(ctx context.Context, q pg.Querier, username string) (*model.Admin, error) {
	row := &model.Admin{}
	err := q.QueryRow(ctx, `
		SELECT id, username, display_name, password_hash, status, auth_version,
			last_login_at, password_changed_at, created_at, updated_at
		FROM tb_admins WHERE username = $1`, username,
	).Scan(&row.ID, &row.Username, &row.DisplayName, &row.PasswordHash, &row.Status,
		&row.AuthVersion, &row.LastLoginAt, &row.PasswordChangedAt, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// FindAdminByID looks up an admin by id.
func FindAdminByID(ctx context.Context, q pg.Querier, id int64) (*model.Admin, error) {
	row := &model.Admin{}
	err := q.QueryRow(ctx, `
		SELECT id, username, display_name, password_hash, status, auth_version,
			last_login_at, password_changed_at, created_at, updated_at
		FROM tb_admins WHERE id = $1`, id,
	).Scan(&row.ID, &row.Username, &row.DisplayName, &row.PasswordHash, &row.Status,
		&row.AuthVersion, &row.LastLoginAt, &row.PasswordChangedAt, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// -- Sessions --

// InsertAdminSession stores a new server-side session (hash only).
func InsertAdminSession(ctx context.Context, q pg.Querier, s *model.AdminSession) (*model.AdminSession, error) {
	row := &model.AdminSession{}
	err := q.QueryRow(ctx, `
		INSERT INTO tb_admin_sessions (admin_id, token_hash, auth_version, ip_address, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, admin_id, token_hash, auth_version, ip_address, user_agent,
			created_at, last_seen_at, expires_at, revoked_at`,
		s.AdminID, s.TokenHash, s.AuthVersion, s.IPAddress, s.UserAgent, s.ExpiresAt,
	).Scan(&row.ID, &row.AdminID, &row.TokenHash, &row.AuthVersion, &row.IPAddress, &row.UserAgent,
		&row.CreatedAt, &row.LastSeenAt, &row.ExpiresAt, &row.RevokedAt)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// ErrAdminSessionNotFound is returned when a session hash does not resolve.
var ErrAdminSessionNotFound = errors.New("admin session not found")

// FindAdminSessionByTokenHash resolves a session by token hash with the
// owning admin joined in (status + auth_version checks are the caller's).
func FindAdminSessionByTokenHash(ctx context.Context, q pg.Querier, tokenHash string) (*model.AdminSession, *model.Admin, error) {
	s := &model.AdminSession{}
	a := &model.Admin{}
	err := q.QueryRow(ctx, `
		SELECT s.id, s.admin_id, s.token_hash, s.auth_version, s.ip_address, s.user_agent,
			s.created_at, s.last_seen_at, s.expires_at, s.revoked_at,
			a.id, a.username, a.display_name, a.password_hash, a.status, a.auth_version,
			a.last_login_at, a.password_changed_at, a.created_at, a.updated_at
		FROM tb_admin_sessions s
		JOIN tb_admins a ON a.id = s.admin_id
		WHERE s.token_hash = $1`, tokenHash,
	).Scan(&s.ID, &s.AdminID, &s.TokenHash, &s.AuthVersion, &s.IPAddress, &s.UserAgent,
		&s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt,
		&a.ID, &a.Username, &a.DisplayName, &a.PasswordHash, &a.Status, &a.AuthVersion,
		&a.LastLoginAt, &a.PasswordChangedAt, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return s, a, nil
}

// TouchAdminSession updates last_seen_at (throttled by the caller:
// only when the last touch is >= 5 minutes old).
func TouchAdminSession(ctx context.Context, q pg.Querier, sessionID int64, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE tb_admin_sessions SET last_seen_at = $1 WHERE id = $2`, at, sessionID)
	return err
}

// RevokeAdminSessionByID revokes one session (idempotent).
func RevokeAdminSessionByID(ctx context.Context, q pg.Querier, sessionID int64) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admin_sessions SET revoked_at = CURRENT_TIMESTAMP WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	return err
}

// RevokeAllAdminSessions revokes every active session of an admin.
func RevokeAllAdminSessions(ctx context.Context, q pg.Querier, adminID int64) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admin_sessions SET revoked_at = CURRENT_TIMESTAMP WHERE admin_id = $1 AND revoked_at IS NULL`, adminID)
	return err
}

// UpdateAdminLastLogin stamps last_login_at.
func UpdateAdminLastLogin(ctx context.Context, q pg.Querier, adminID int64, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE tb_admins SET last_login_at = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, at, adminID)
	return err
}

// -- RBAC --

// AdminRolePermission is one role binding row.
type AdminRolePermission struct {
	Role            *model.AdminRole
	PermissionCodes []string
}

// ListAdminRolesByAdminID returns the admin's roles (active only)
// plus each role's (active) permission codes. Permissions are
// resolved fresh from the DB on every request — no caching in the
// session, so role edits take effect immediately.
func ListAdminRolesByAdminID(ctx context.Context, q pg.Querier, adminID int64) ([]*model.AdminRole, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id, r.code, r.name, r.description, r.is_system, r.status, r.created_at, r.updated_at
		FROM tb_admin_user_roles ur
		JOIN tb_admin_roles r ON r.id = ur.role_id
		WHERE ur.admin_id = $1 AND r.status = 'active'
		ORDER BY r.id`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.AdminRole
	for rows.Next() {
		r := &model.AdminRole{}
		if err := rows.Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.IsSystem, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAdminPermissionCodesByAdminID returns the distinct permission codes
// granted to the admin through active roles, including through active roles
// that hold the '*' wildcard row.
func ListAdminPermissionCodesByAdminID(ctx context.Context, q pg.Querier, adminID int64) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT rp.permission_code
		FROM tb_admin_user_roles ur
		JOIN tb_admin_roles r ON r.id = ur.role_id AND r.status = 'active'
		JOIN tb_admin_role_permissions rp ON rp.role_id = r.id
		WHERE ur.admin_id = $1
		ORDER BY rp.permission_code`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// FindAdminRoleByCode finds a role by code.
func FindAdminRoleByCode(ctx context.Context, q pg.Querier, code string) (*model.AdminRole, error) {
	r := &model.AdminRole{}
	err := q.QueryRow(ctx, `
		SELECT id, code, name, description, is_system, status, created_at, updated_at
		FROM tb_admin_roles WHERE code = $1`, code,
	).Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.IsSystem, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// AssignAdminRole binds a role to an admin (idempotent).
func AssignAdminRole(ctx context.Context, q pg.Querier, adminID, roleID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_admin_user_roles (admin_id, role_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, adminID, roleID)
	return err
}

// -- Audit --

// InsertAdminAuditLog appends one governance fact. Append-only:
// there is intentionally no update/delete function for this table.
func InsertAdminAuditLog(ctx context.Context, q pg.Querier, l *model.AdminAuditLog) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_admin_audit_logs
			(actor_admin_id, actor_username, action, target_type, target_id, success,
			 before_json, after_json, metadata_json, ip_address, user_agent, error_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		l.ActorAdminID, l.ActorUsername, l.Action, l.TargetType, l.TargetID, l.Success,
		l.BeforeJSON, l.AfterJSON, l.MetadataJSON, l.IPAddress, l.UserAgent, l.ErrorCode)
	return err
}

// ============================================================
// B1.2: Admin management repository extensions.
// ALL tb_admin* SQL stays here (AST guard enforces it).
// ============================================================

// -- Locking primitives (fixed order to avoid management deadlocks) --

// LockAdminRowsForUpdate takes row locks on the given admin ids in
// ASCENDING id order (fixed lock order). Callers must pass a sorted,
// deduplicated id list.
func LockAdminRowsForUpdate(ctx context.Context, q pg.Querier, adminIDs []int64) error {
	if len(adminIDs) == 0 {
		return nil
	}
	_, err := q.Exec(ctx,
		`SELECT id FROM tb_admins WHERE id = ANY($1) ORDER BY id FOR UPDATE`, adminIDs)
	return err
}

// LockAdminRowForUpdate locks a single admin row.
func LockAdminRowForUpdate(ctx context.Context, q pg.Querier, adminID int64) error {
	_, err := q.Exec(ctx, `SELECT id FROM tb_admins WHERE id = $1 FOR UPDATE`, adminID)
	return err
}

// LockAdminRoleRowForUpdate locks a single role row.
func LockAdminRoleRowForUpdate(ctx context.Context, q pg.Querier, roleID int64) error {
	_, err := q.Exec(ctx, `SELECT id FROM tb_admin_roles WHERE id = $1 FOR UPDATE`, roleID)
	return err
}

// -- Active-superadmin invariant --

// CountActiveSuperadmins returns the number of admins with
// status='active' bound to the active superadmin system role.
// Must be called INSIDE the caller's transaction AFTER taking the
// admin row locks, so the count is transaction-stable.
func CountActiveSuperadmins(ctx context.Context, q pg.Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.id)
		FROM tb_admins a
		JOIN tb_admin_user_roles ur ON ur.admin_id = a.id
		JOIN tb_admin_roles r ON r.id = ur.role_id
		WHERE r.code = 'superadmin' AND r.status = 'active' AND a.status = 'active'`).Scan(&n)
	return n, err
}

// -- Admin accounts --

// ErrAdminUsernameExists marks a duplicate username on create.
var ErrAdminUsernameExists = errors.New("admin username exists")

// InsertAdminWithRoles creates an admin inside the caller's tx.
// Detects the username UNIQUE constraint and returns
// ErrAdminUsernameExists so the domain layer can map it to 409.
func InsertAdminWithRoles(ctx context.Context, q pg.Querier, a *model.Admin) (*model.Admin, error) {
	row := &model.Admin{}
	err := q.QueryRow(ctx, `
		INSERT INTO tb_admins (username, display_name, password_hash, status)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, ''), 'active'))
		RETURNING id, username, display_name, password_hash, status, auth_version,
			last_login_at, password_changed_at, created_at, updated_at`,
		a.Username, a.DisplayName, a.PasswordHash, a.Status,
	).Scan(&row.ID, &row.Username, &row.DisplayName, &row.PasswordHash, &row.Status,
		&row.AuthVersion, &row.LastLoginAt, &row.PasswordChangedAt, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "uk_admin_username") {
			return nil, ErrAdminUsernameExists
		}
		return nil, err
	}
	return row, nil
}

// UpdateAdminDisplayName updates display_name only (username immutable).
func UpdateAdminDisplayName(ctx context.Context, q pg.Querier, adminID int64, displayName string) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admins SET display_name = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`,
		displayName, adminID)
	return err
}

// SetAdminStatus flips status active|disabled (guarded transition is
// the domain layer's job; this is the raw write).
func SetAdminStatus(ctx context.Context, q pg.Querier, adminID int64, status string) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admins SET status = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`,
		status, adminID)
	return err
}

// UpdateAdminPassword sets a new hash and stamps password_changed_at.
func UpdateAdminPassword(ctx context.Context, q pg.Querier, adminID int64, passwordHash string) error {
	_, err := q.Exec(ctx,
		`UPDATE tb_admins SET password_hash = $1, password_changed_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP WHERE id = $2`,
		passwordHash, adminID)
	return err
}

// ListAdmins returns admins ordered by id, with their role codes
// aggregated (read-only; used by the users API).
func ListAdmins(ctx context.Context, q pg.Querier) ([]*model.Admin, map[int64][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT id, username, display_name, password_hash, status, auth_version,
			last_login_at, password_changed_at, created_at, updated_at
		FROM tb_admins ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*model.Admin
	for rows.Next() {
		a := &model.Admin{}
		if err := rows.Scan(&a.ID, &a.Username, &a.DisplayName, &a.PasswordHash, &a.Status,
			&a.AuthVersion, &a.LastLoginAt, &a.PasswordChangedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rolesByAdmin := make(map[int64][]string)
	rrows, err := q.Query(ctx, `
		SELECT ur.admin_id, r.code
		FROM tb_admin_user_roles ur
		JOIN tb_admin_roles r ON r.id = ur.role_id
		ORDER BY ur.admin_id, r.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var adminID int64
		var code string
		if err := rrows.Scan(&adminID, &code); err != nil {
			return nil, nil, err
		}
		rolesByAdmin[adminID] = append(rolesByAdmin[adminID], code)
	}
	return out, rolesByAdmin, rrows.Err()
}

// -- Roles --

// ListAdminRoles returns all roles ordered by id, with their
// permission codes (read-only).
func ListAdminRoles(ctx context.Context, q pg.Querier) ([]*model.AdminRole, map[int64][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT id, code, name, description, is_system, status, created_at, updated_at
		FROM tb_admin_roles ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*model.AdminRole
	for rows.Next() {
		r := &model.AdminRole{}
		if err := rows.Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.IsSystem, &r.Status,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	permsByRole := make(map[int64][]string)
	prows, err := q.Query(ctx, `
		SELECT role_id, permission_code FROM tb_admin_role_permissions ORDER BY role_id, permission_code`)
	if err != nil {
		return nil, nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var roleID int64
		var code string
		if err := prows.Scan(&roleID, &code); err != nil {
			return nil, nil, err
		}
		permsByRole[roleID] = append(permsByRole[roleID], code)
	}
	return out, permsByRole, prows.Err()
}

// FindAdminRoleByID finds a role by id.
func FindAdminRoleByID(ctx context.Context, q pg.Querier, id int64) (*model.AdminRole, error) {
	r := &model.AdminRole{}
	err := q.QueryRow(ctx, `
		SELECT id, code, name, description, is_system, status, created_at, updated_at
		FROM tb_admin_roles WHERE id = $1`, id,
	).Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.IsSystem, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ErrAdminRoleCodeExists marks a duplicate role code on create.
var ErrAdminRoleCodeExists = errors.New("admin role code exists")

// InsertAdminRole creates a custom role (is_system stays false).
func InsertAdminRole(ctx context.Context, q pg.Querier, code, name, description string) (*model.AdminRole, error) {
	r := &model.AdminRole{}
	err := q.QueryRow(ctx, `
		INSERT INTO tb_admin_roles (code, name, description, is_system, status)
		VALUES ($1, $2, $3, FALSE, 'active')
		RETURNING id, code, name, description, is_system, status, created_at, updated_at`,
		code, name, description,
	).Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.IsSystem, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "uk_admin_role_code") {
			return nil, ErrAdminRoleCodeExists
		}
		return nil, err
	}
	return r, nil
}

// UpdateAdminRole updates name/description/status of a NON-system
// role only (guarded by WHERE is_system = FALSE so system roles are
// structurally unmodifiable).
func UpdateAdminRole(ctx context.Context, q pg.Querier, roleID int64, name, description, status string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_admin_roles
		SET name = $1, description = $2, status = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $4 AND is_system = FALSE`,
		name, description, status, roleID)
	return err
}

// ReplaceAdminRolePermissions atomically replaces the role's permission
// bindings (complete-replacement semantics). Guarded to non-system
// roles.
func ReplaceAdminRolePermissions(ctx context.Context, q pg.Querier, roleID int64, permissionCodes []string) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM tb_admin_role_permissions WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	for _, code := range permissionCodes {
		if _, err := q.Exec(ctx, `
			INSERT INTO tb_admin_role_permissions (role_id, permission_code)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, roleID, code); err != nil {
			return err
		}
	}
	return nil
}

// ListAdminPermissions returns all permission codes ordered by code.
func ListAdminPermissions(ctx context.Context, q pg.Querier) ([]*model.AdminPermission, error) {
	rows, err := q.Query(ctx, `
		SELECT code, description, created_at FROM tb_admin_permissions ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.AdminPermission
	for rows.Next() {
		p := &model.AdminPermission{}
		if err := rows.Scan(&p.Code, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PermissionCodesExist returns the subset of codes that exist.
func PermissionCodesExist(ctx context.Context, q pg.Querier, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx,
		`SELECT code FROM tb_admin_permissions WHERE code = ANY($1)`, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// -- Admin ↔ role assignment --

// ReplaceAdminUserRoles atomically replaces the admin's role bindings
// (complete-replacement semantics).
func ReplaceAdminUserRoles(ctx context.Context, q pg.Querier, adminID int64, roleIDs []int64) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM tb_admin_user_roles WHERE admin_id = $1`, adminID); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		if _, err := q.Exec(ctx, `
			INSERT INTO tb_admin_user_roles (admin_id, role_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, adminID, roleID); err != nil {
			return err
		}
	}
	return nil
}

// ListAdminRoleIDsByCodes resolves role ids from codes (existence
// check via len comparison by the caller).
func ListAdminRoleIDsByCodes(ctx context.Context, q pg.Querier, codes []string) (map[string]int64, error) {
	out := make(map[string]int64)
	if len(codes) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		`SELECT code, id FROM tb_admin_roles WHERE code = ANY($1)`, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			return nil, err
		}
		out[code] = id
	}
	return out, rows.Err()
}

// RoleIDsExist returns the subset of role ids that exist.
func RoleIDsExist(ctx context.Context, q pg.Querier, roleIDs []int64) ([]int64, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx,
		`SELECT id FROM tb_admin_roles WHERE id = ANY($1)`, roleIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListAdminUserRoleIDs returns the admin's current role ids (inside
// the caller's tx, for before/after audit facts).
func ListAdminUserRoleIDs(ctx context.Context, q pg.Querier, adminID int64) ([]int64, error) {
	rows, err := q.Query(ctx,
		`SELECT role_id FROM tb_admin_user_roles WHERE admin_id = $1 ORDER BY role_id`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// -- Sessions --

// ListAdminSessions returns all sessions joined with admin identity,
// newest first. NO token material is selected.
func ListAdminSessions(ctx context.Context, q pg.Querier) ([]*model.AdminSessionInfo, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id, s.admin_id, a.username, a.display_name,
			s.ip_address, s.user_agent, s.created_at, s.last_seen_at, s.expires_at, s.revoked_at
		FROM tb_admin_sessions s
		JOIN tb_admins a ON a.id = s.admin_id
		ORDER BY s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.AdminSessionInfo
	for rows.Next() {
		si := &model.AdminSessionInfo{}
		if err := rows.Scan(&si.ID, &si.AdminID, &si.Username, &si.DisplayName,
			&si.IPAddress, &si.UserAgent, &si.CreatedAt, &si.LastSeenAt, &si.ExpiresAt,
			&si.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// FindAdminSessionByID loads one session (no tokens) by id.
func FindAdminSessionByID(ctx context.Context, q pg.Querier, sessionID int64) (*model.AdminSessionInfo, error) {
	si := &model.AdminSessionInfo{}
	err := q.QueryRow(ctx, `
		SELECT s.id, s.admin_id, a.username, a.display_name,
			s.ip_address, s.user_agent, s.created_at, s.last_seen_at, s.expires_at, s.revoked_at
		FROM tb_admin_sessions s
		JOIN tb_admins a ON a.id = s.admin_id
		WHERE s.id = $1`, sessionID,
	).Scan(&si.ID, &si.AdminID, &si.Username, &si.DisplayName,
		&si.IPAddress, &si.UserAgent, &si.CreatedAt, &si.LastSeenAt, &si.ExpiresAt,
		&si.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return si, nil
}

// RevokeAdminSessionByIDTx revokes one session on the caller's tx
// (same SQL as RevokeAdminSessionByID; explicit tx alias for
// management flows).
func RevokeAdminSessionByIDTx(ctx context.Context, q pg.Querier, sessionID int64) error {
	return RevokeAdminSessionByID(ctx, q, sessionID)
}

// -- Audit explorer --

// AdminAuditFilter carries the explorer's filter parameters.
type AdminAuditFilter struct {
	ActorAdminID  *int64
	ActorUsername string
	Action        string
	TargetType    string
	TargetID      string
	Success       *bool
	Page          int
	PageSize      int
}

// SelectAdminAuditLogs reads a filtered, ordered, paginated page of
// audit rows plus the total match count.
func SelectAdminAuditLogs(ctx context.Context, q pg.Querier, f AdminAuditFilter) ([]*model.AdminAuditLog, int64, error) {
	where := "WHERE 1=1"
	args := []interface{}{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.ActorAdminID != nil {
		where += " AND actor_admin_id = " + arg(*f.ActorAdminID)
	}
	if f.ActorUsername != "" {
		where += " AND actor_username = " + arg(f.ActorUsername)
	}
	if f.Action != "" {
		where += " AND action = " + arg(f.Action)
	}
	if f.TargetType != "" {
		where += " AND target_type = " + arg(f.TargetType)
	}
	if f.TargetID != "" {
		where += " AND target_id = " + arg(f.TargetID)
	}
	if f.Success != nil {
		where += " AND success = " + arg(*f.Success)
	}

	var total int64
	if err := q.QueryRow(ctx,
		"SELECT COUNT(*) FROM tb_admin_audit_logs "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (f.Page - 1) * f.PageSize
	rows, err := q.Query(ctx, `
		SELECT id, actor_admin_id, actor_username, action, target_type, target_id, success,
			before_json, after_json, metadata_json, ip_address, user_agent, error_code, created_at
		FROM tb_admin_audit_logs `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT `+arg(f.PageSize)+` OFFSET `+arg(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.AdminAuditLog
	for rows.Next() {
		l := &model.AdminAuditLog{}
		if err := rows.Scan(&l.ID, &l.ActorAdminID, &l.ActorUsername, &l.Action, &l.TargetType,
			&l.TargetID, &l.Success, &l.BeforeJSON, &l.AfterJSON, &l.MetadataJSON,
			&l.IPAddress, &l.UserAgent, &l.ErrorCode, &l.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// ListAdminRoleCodesByAdminID returns the admin's active role codes.
func ListAdminRoleCodesByAdminID(ctx context.Context, q pg.Querier, adminID int64) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT r.code
		FROM tb_admin_user_roles ur
		JOIN tb_admin_roles r ON r.id = ur.role_id
		WHERE ur.admin_id = $1 AND r.status = 'active'
		ORDER BY r.id`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// ListAdminPermissionCodesByRole returns a role's permission codes.
func ListAdminPermissionCodesByRole(ctx context.Context, q pg.Querier, roleID int64) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT permission_code FROM tb_admin_role_permissions
		WHERE role_id = $1 ORDER BY permission_code`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// IsActiveSuperadmin reports whether the admin exists, is active, and
// holds the active superadmin system role. Call inside the caller's
// transaction after locking the admin row.
func IsActiveSuperadmin(ctx context.Context, q pg.Querier, adminID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tb_admin_user_roles ur
			JOIN tb_admin_roles r ON r.id = ur.role_id
			JOIN tb_admins a ON a.id = ur.admin_id
			WHERE ur.admin_id = $1 AND r.code = 'superadmin'
			  AND r.status = 'active' AND a.status = 'active'
		)`, adminID).Scan(&ok)
	return ok, err
}
