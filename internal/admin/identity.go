// Package admin is the Platform Admin plane (B1.1): a third identity
// surface, permanently separate from the Agent plane (X-Bot-Key →
// bot_id) and the Owner plane (kf_owner → bot_id). The Admin plane
// authenticates via a server-side session (kf_admin cookie → admin_id)
// and authorizes per-request against live RBAC data.
//
// Allowed dependencies ONLY: auth password utility, pg, repository,
// model, errors, and necessary security utilities. No store, payment,
// credits, task, storage, or service imports.
package admin

import (
	"context"
	"regexp"
	"strings"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// WildcardPermission is held by the superadmin role and grants every
// admin permission. No username or admin-id special-casing exists.
const WildcardPermission = "*"

// usernamePattern: lowercase [a-z0-9._-], 3–64 chars.
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

// NormalizeUsername lowercases and trims a username. Callers must run
// this BEFORE any validation or persistence so uniqueness is decided
// on the normalized form.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// ValidateUsername enforces the normalized format.
func ValidateUsername(username string) bool {
	return usernamePattern.MatchString(username)
}

// BootstrapResult reports what a bootstrap run did.
type BootstrapResult struct {
	AdminID  int64
	Username string
	Created  bool // false when the run was refused (admins already exist)
}

// Bootstrap creates the FIRST platform admin, assigns the superadmin
// system role, and writes the bootstrap audit record — all in ONE
// transaction. Fails closed when any admin already exists. Re-running
// never creates a second superadmin.
func Bootstrap(ctx context.Context, pool *pg.Pool, username, displayName, password string) (*BootstrapResult, error) {
	username = NormalizeUsername(username)
	if !ValidateUsername(username) {
		return nil, errors.New(400, "INVALID_USERNAME",
			"Username must be 3-64 chars of lowercase letters, digits, '.', '_' or '-'")
	}
	if strings.TrimSpace(displayName) == "" {
		return nil, errors.New(400, "INVALID_DISPLAY_NAME", "Display name must not be empty")
	}
	if len(password) < 8 {
		return nil, errors.New(400, "INVALID_PASSWORD", "Password must be at least 8 characters")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to hash password")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer pg.Rollback(tx)

	// Serialize concurrent first-bootstraps: the EXCLUSIVE lock makes
	// the second transaction's COUNT wait until the first commits, so
	// exactly one winner is possible regardless of goroutine ordering.
	if err := repository.LockAdminsTableExclusive(ctx, tx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}

	// Fail-closed gate inside the transaction (post-lock).
	count, err := repository.CountAdmins(ctx, tx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if count > 0 {
		return nil, errors.New(409, "BOOTSTRAP_REFUSED",
			"Bootstrap refused: an administrator already exists")
	}

	created, err := repository.InsertAdmin(ctx, tx, &model.Admin{
		Username:     username,
		DisplayName:  strings.TrimSpace(displayName),
		PasswordHash: hash,
		Status:       "active",
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to create admin")
	}

	role, err := repository.FindAdminRoleByCode(ctx, tx, "superadmin")
	if err != nil || role == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "superadmin system role missing")
	}
	if err := repository.AssignAdminRole(ctx, tx, created.ID, role.ID); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to assign superadmin role")
	}

	// Bootstrap audit record: same transaction as the mutation.
	if err := repository.InsertAdminAuditLog(ctx, tx, &model.AdminAuditLog{
		ActorAdminID:  &created.ID,
		ActorUsername: created.Username,
		Action:        "admin.bootstrap",
		TargetType:    strPtr("admin"),
		TargetID:      strPtr(idToString(created.ID)),
		Success:       true,
		AfterJSON:     []byte(`{"username":"` + created.Username + `","roles":["superadmin"]}`),
		MetadataJSON:  []byte(`{"source":"cli"}`),
	}); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to write bootstrap audit")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return &BootstrapResult{AdminID: created.ID, Username: created.Username, Created: true}, nil
}

// -- Login --

// dummyAdminBcryptHash is a valid cost-10 bcrypt hash of an unguessable
// random secret; verified when the username does not resolve, so
// unknown users burn identical bcrypt work (anti enumeration).
const dummyAdminBcryptHash = "$2a$10$Y32i7tXf1eM73f06uFMlMulohgIXrlbeYW.9HWWd4q5zbwJ957vKO"

// LoginInput carries login credentials plus request context for the
// session row.
type LoginInput struct {
	Username  string
	Password  string
	IPAddress string
	UserAgent string
}

// LoginResult is the outcome of a successful admin login. RawToken is
// the ONE place the raw session token exists: it goes straight into
// the HttpOnly cookie and is never persisted or logged.
type LoginResult struct {
	Admin    *model.Admin
	Session  *model.AdminSession
	RawToken string
	Roles    []string
}

// Login authenticates an admin and creates a server-side session.
// session INSERT + last_login_at + login audit share one transaction.
// Unknown user / disabled / wrong password are externally identical
// (401 INVALID_CREDENTIALS) and all run the bcrypt work factor.
func Login(ctx context.Context, pool *pg.Pool, in LoginInput) (*LoginResult, error) {
	username := NormalizeUsername(in.Username)
	password := in.Password

	invalid := errors.New(401, "INVALID_CREDENTIALS", "Username or password is incorrect")

	if username == "" || password == "" {
		_ = auth.VerifyPassword(password, dummyAdminBcryptHash)
		return nil, invalid
	}

	adminRec, err := repository.FindAdminByUsername(ctx, pool, username)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error during login")
	}
	if adminRec == nil || adminRec.Status != "active" {
		_ = auth.VerifyPassword(password, dummyAdminBcryptHash)
		return nil, invalid
	}
	if !auth.VerifyPassword(password, adminRec.PasswordHash) {
		return nil, invalid
	}

	rawToken, tokenHash, err := GenerateSessionToken()
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to create session")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer pg.Rollback(tx)

	session, err := repository.InsertAdminSession(ctx, tx, &model.AdminSession{
		AdminID:     adminRec.ID,
		TokenHash:   tokenHash,
		AuthVersion: adminRec.AuthVersion,
		IPAddress:   nullableString(in.IPAddress),
		UserAgent:   nullableString(in.UserAgent),
		ExpiresAt:   now().Add(SessionAbsoluteTTL),
	})
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to create session")
	}
	if err := repository.UpdateAdminLastLogin(ctx, tx, adminRec.ID, timeNow()); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.InsertAdminAuditLog(ctx, tx, &model.AdminAuditLog{
		ActorAdminID:  &adminRec.ID,
		ActorUsername: adminRec.Username,
		Action:        "admin.login",
		TargetType:    strPtr("admin_session"),
		TargetID:      strPtr(idToString(session.ID)),
		Success:       true,
		IPAddress:     nullableString(in.IPAddress),
		UserAgent:     nullableString(in.UserAgent),
	}); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Failed to write login audit")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}

	roles, err := repository.ListAdminRolesByAdminID(ctx, pool, adminRec.ID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	roleCodes := make([]string, 0, len(roles))
	for _, r := range roles {
		roleCodes = append(roleCodes, r.Code)
	}

	return &LoginResult{
		Admin:    adminRec,
		Session:  session,
		RawToken: rawToken,
		Roles:    roleCodes,
	}, nil
}

// RecordFailedLogin writes a best-effort audit entry for a failed
// login attempt (no privileged mutation happened, so best-effort is
// allowed by the audit invariant). Unknown usernames are recorded
// verbatim as historical snapshots.
func RecordFailedLogin(ctx context.Context, pool *pg.Pool, username, ip, userAgent, errorCode string) {
	username = NormalizeUsername(username)
	if username == "" || !ValidateUsername(username) {
		username = "(invalid)"
	}
	_ = repository.InsertAdminAuditLog(ctx, pool, &model.AdminAuditLog{
		ActorUsername: username,
		Action:        "admin.login",
		Success:       false,
		IPAddress:     nullableString(ip),
		UserAgent:     nullableString(userAgent),
		ErrorCode:     strPtr(errorCode),
	})
}
