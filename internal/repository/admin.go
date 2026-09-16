package repository

import (
	"context"
	"errors"
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
