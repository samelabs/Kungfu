package admin

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sort"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// Admin cookie and session lifecycle constants. The admin cookie is
// COMPLETELY independent of the owner cookie (kf_owner): different
// name, different mechanism (server-side session vs stateless HMAC).
const (
	AdminCookieName = "kf_admin"

	// SessionAbsoluteTTL bounds a session from creation regardless of
	// activity.
	SessionAbsoluteTTL = 12 * time.Hour

	// SessionIdleTimeout invalidates a session after inactivity.
	SessionIdleTimeout = 30 * time.Minute

	// TouchThrottle: last_seen_at is only rewritten when the previous
	// touch is at least this old — avoids a DB write per request.
	TouchThrottle = 5 * time.Minute
)

// timeNow is swappable in tests.
var timeNow = time.Now

func now() time.Time { return timeNow() }

// GenerateSessionToken mints a 32-byte crypto/random token.
// Returns (rawToken, sha256hex(rawToken)). The raw token only ever
// travels inside the HttpOnly cookie; only the hash is persisted.
func GenerateSessionToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return "", "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, toHex(sum[:]), nil
}

// HashSessionToken computes the stored form of a raw session token.
func HashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return toHex(sum[:])
}

func toHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}

// SetAdminCookie writes the raw session token into the kf_admin
// HttpOnly cookie.
func SetAdminCookie(w http.ResponseWriter, rawToken string, isHTTPS bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminCookieName,
		Value:    rawToken,
		Path:     "/",
		MaxAge:   int(SessionAbsoluteTTL.Seconds()),
		Secure:   isHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearAdminCookie clears kf_admin. Always safe — even with a stale
// cookie the client must be able to remove its local login state.
func ClearAdminCookie(w http.ResponseWriter, isHTTPS bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Now().Add(-time.Hour),
		Secure:   isHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// Principal is the resolved admin identity for a request: the admin,
// its active roles, and the live permission set. Permissions are
// resolved from the database on every request — never cached in the
// session — so role changes apply to existing sessions immediately.
type Principal struct {
	Admin       *model.Admin
	Roles       []string
	Permissions []string
	Session     *model.AdminSession
}

// HasPermission: "*" allows any permission; otherwise exact code match.
func (p *Principal) HasPermission(code string) bool {
	for _, perm := range p.Permissions {
		if perm == WildcardPermission || perm == code {
			return true
		}
	}
	return false
}

// ResolveSession validates the kf_admin cookie against the server-side
// session store on EVERY request:
//   - session exists for the token hash
//   - revoked_at IS NULL
//   - expires_at > now (absolute TTL)
//   - last_seen_at + idle timeout > now
//   - admin.status == active
//   - session.auth_version == admin.auth_version
//
// On success it throttled-touches last_seen_at (only when >= 5 min
// stale) and resolves roles + permissions live.
func ResolveSession(ctx context.Context, pool *pg.Pool, rawToken string) (*Principal, error) {
	if rawToken == "" {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	session, adminRec, err := repository.FindAdminSessionByTokenHash(ctx, pool, HashSessionToken(rawToken))
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if session == nil {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	n := now()
	if session.RevokedAt != nil {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	if !session.ExpiresAt.After(n) {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	// Idle expiry: expired when now >= last_seen_at + 30m (boundary
	// inclusive — exactly 30 minutes idle is expired).
	if !n.Before(session.LastSeenAt.Add(SessionIdleTimeout)) {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	if adminRec.Status != "active" {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	if session.AuthVersion != adminRec.AuthVersion {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}

	// Throttled touch: rewrite when now >= last_seen_at + 5m (boundary
	// inclusive — exactly 5 minutes stale triggers a touch).
	if !n.Before(session.LastSeenAt.Add(TouchThrottle)) {
		if err := repository.TouchAdminSession(ctx, pool, session.ID, n); err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		session.LastSeenAt = n
	}

	roles, err := repository.ListAdminRolesByAdminID(ctx, pool, adminRec.ID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	roleCodes := make([]string, 0, len(roles))
	for _, r := range roles {
		roleCodes = append(roleCodes, r.Code)
	}
	perms, err := repository.ListAdminPermissionCodesByAdminID(ctx, pool, adminRec.ID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}

	return &Principal{
		Admin:       adminRec,
		Roles:       roleCodes,
		Permissions: perms,
		Session:     session,
	}, nil
}

// RevokeSession revokes the current session and audits the logout.
// Revocation + logout audit share one transaction (privileged-ish
// mutation on the session plane).
func RevokeSession(ctx context.Context, pool *pg.Pool, principal *Principal) error {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer pg.Rollback(tx)

	if err := repository.RevokeAdminSessionByID(ctx, tx, principal.Session.ID); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.InsertAdminAuditLog(ctx, tx, &model.AdminAuditLog{
		ActorAdminID:  &principal.Admin.ID,
		ActorUsername: principal.Admin.Username,
		Action:        "admin.logout",
		TargetType:    strPtr("admin_session"),
		TargetID:      strPtr(idToString(principal.Session.ID)),
		Success:       true,
		IPAddress:     principal.Session.IPAddress,
		UserAgent:     principal.Session.UserAgent,
	}); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Failed to write logout audit")
	}
	return tx.Commit(ctx)
}

// bumpAuthVersionAndRevokeSessions is the tx-aware primitive B1.2's
// privileged operations compose inside WithAuditTx:
//
//	admin.WithAuditTx(ctx, pool, entry, func(ctx, tx) error {
//	    disable / reset password / force logout (business mutation)
//	    return admin.bumpAuthVersionAndRevokeSessions(ctx, tx, adminID)
//	})
//
// It deliberately does NOT begin/commit its own transaction and does
// NOT write audit — the caller's single transaction owns atomicity.
// There is intentionally no exported mutation API that could bypass
// the audit invariant.
func bumpAuthVersionAndRevokeSessions(ctx context.Context, q pg.Querier, adminID int64) error {
	if err := repository.IncrementAdminAuthVersion(ctx, q, adminID); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.RevokeAllAdminSessions(ctx, q, adminID); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return nil
}

// lockAdminRowsOrdered locks ALL admin rows the mutation touches
// (actor + targets) FOR UPDATE in ascending-id order — the single
// global lock order for every admin-management transaction, so
// cross-actor operations serialize instead of deadlocking (the audit
// INSERT's actor FK KEY SHARE lock is then always held by the same
// transaction first).
func lockAdminRowsOrdered(ctx context.Context, q pg.Querier, ids ...int64) error {
	seen := map[int64]bool{}
	ordered := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			ordered = append(ordered, id)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return repository.LockAdminRowsForUpdate(ctx, q, ordered)
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func strPtr(s string) *string { return &s }

func idToString(id int64) string {
	// small deterministic int→string without importing strconv at
	// several call sites
	if id == 0 {
		return "0"
	}
	neg := id < 0
	if neg {
		id = -id
	}
	var buf [24]byte
	i := len(buf)
	for id > 0 {
		i--
		buf[i] = byte('0' + id%10)
		id /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
