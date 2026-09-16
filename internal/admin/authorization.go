package admin

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// RequirePermission authorizes a resolved principal against an exact
// permission code. "*" (held by superadmin) allows any code. There is
// deliberately NO username or admin-id special-casing anywhere.
func RequirePermission(ctx context.Context, pool *pg.Pool, principal *Principal, permission string) error {
	if principal == nil || principal.Admin == nil {
		return errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	if principal.HasPermission(permission) {
		return nil
	}
	return errors.New(403, "FORBIDDEN", "Admin does not have permission: "+permission)
}
