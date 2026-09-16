package admin

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// B1.2 read operations, wrapped in the Admin domain so handlers never
// import the repository (pipeline: server → admin → repository → PG).

// AdminUserView is one admin row + its role codes for list/detail.
type AdminUserView struct {
	Admin *model.Admin
	Roles []string
}

// ListUsers requires admin.users.read and returns all admins with
// their role codes.
func ListUsers(ctx context.Context, pool *pg.Pool, principal *Principal) ([]*AdminUserView, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.users.read"); err != nil {
		return nil, err
	}
	admins, rolesByAdmin, err := repository.ListAdmins(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	out := make([]*AdminUserView, 0, len(admins))
	for _, a := range admins {
		a.PasswordHash = ""
		roles := rolesByAdmin[a.ID]
		if roles == nil {
			roles = []string{}
		}
		out = append(out, &AdminUserView{Admin: a, Roles: roles})
	}
	return out, nil
}

// GetUser requires admin.users.read and returns one admin + roles.
func GetUser(ctx context.Context, pool *pg.Pool, principal *Principal, adminID int64) (*AdminUserView, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.users.read"); err != nil {
		return nil, err
	}
	a, err := repository.FindAdminByID(ctx, pool, adminID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if a == nil {
		return nil, errors.New(404, "ADMIN_NOT_FOUND", "Admin not found")
	}
	roles, err := repository.ListAdminRoleCodesByAdminID(ctx, pool, adminID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if roles == nil {
		roles = []string{}
	}
	a.PasswordHash = ""
	return &AdminUserView{Admin: a, Roles: roles}, nil
}

// AdminRoleView is one role + its permission codes.
type AdminRoleView struct {
	Role        *model.AdminRole
	Permissions []string
}

// ListRoles requires admin.roles.read and returns all roles with
// their permission bindings.
func ListRoles(ctx context.Context, pool *pg.Pool, principal *Principal) ([]*AdminRoleView, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.read"); err != nil {
		return nil, err
	}
	roles, permsByRole, err := repository.ListAdminRoles(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	out := make([]*AdminRoleView, 0, len(roles))
	for _, r := range roles {
		perms := permsByRole[r.ID]
		if perms == nil {
			perms = []string{}
		}
		out = append(out, &AdminRoleView{Role: r, Permissions: perms})
	}
	return out, nil
}

// GetRole requires admin.roles.read and returns one role + bindings.
func GetRole(ctx context.Context, pool *pg.Pool, principal *Principal, roleID int64) (*AdminRoleView, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.read"); err != nil {
		return nil, err
	}
	role, err := repository.FindAdminRoleByID(ctx, pool, roleID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if role == nil {
		return nil, errors.New(404, "ROLE_NOT_FOUND", "Role not found")
	}
	perms, err := repository.ListAdminPermissionCodesByRole(ctx, pool, roleID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if perms == nil {
		perms = []string{}
	}
	return &AdminRoleView{Role: role, Permissions: perms}, nil
}

// ListPermissions requires admin.roles.read (permission visibility)
// and returns every permission code.
func ListPermissions(ctx context.Context, pool *pg.Pool, principal *Principal) ([]*model.AdminPermission, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.roles.read"); err != nil {
		return nil, err
	}
	perms, err := repository.ListAdminPermissions(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return perms, nil
}

// ListSessions requires admin.sessions.manage and returns all
// sessions (identity-joined, no token material).
func ListSessions(ctx context.Context, pool *pg.Pool, principal *Principal) ([]*model.AdminSessionInfo, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.sessions.manage"); err != nil {
		return nil, err
	}
	sessions, err := repository.ListAdminSessions(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return sessions, nil
}

// AuditPage is one page of audit explorer results.
type AuditPage struct {
	Items    []*model.AdminAuditLog
	Page     int
	PageSize int
	Total    int64
}

// AuditFilter is the explorer's filter shape (mirrored here so the
// handler never imports the repository types).
type AuditFilter struct {
	ActorAdminID  *int64
	ActorUsername string
	Action        string
	TargetType    string
	TargetID      string
	Success       *bool
	Page          int
	PageSize      int
}

// ExploreAudit requires admin.audit.read and returns a filtered,
// paginated, stably-ordered audit page.
func ExploreAudit(ctx context.Context, pool *pg.Pool, principal *Principal, f AuditFilter) (*AuditPage, error) {
	if err := RequirePermission(ctx, pool, principal, "admin.audit.read"); err != nil {
		return nil, err
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 50
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	logs, total, err := repository.SelectAdminAuditLogs(ctx, pool, repository.AdminAuditFilter{
		ActorAdminID:  f.ActorAdminID,
		ActorUsername: f.ActorUsername,
		Action:        f.Action,
		TargetType:    f.TargetType,
		TargetID:      f.TargetID,
		Success:       f.Success,
		Page:          f.Page,
		PageSize:      f.PageSize,
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return &AuditPage{Items: logs, Page: f.Page, PageSize: f.PageSize, Total: total}, nil
}
