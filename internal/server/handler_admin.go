package server

import (
	"net/http"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
)

// Admin HTTP surface (B1.1) — identity foundation ONLY:
//
//	POST   /api/admin/session   login (rate-limited; no CSRF — this
//	                            is the credential entry point)
//	GET    /api/admin/session   current principal + CSRF token
//	DELETE /api/admin/session   logout (CSRF-gated; stale cookie still
//	                            clears locally, privileged mutation
//	                            only runs for a valid session)
//
// The Admin plane is independent from Owner (kf_owner) and Agent
// (X-Bot-Key): kf_owner can never authenticate here, kf_admin can
// never authenticate owner endpoints, X-Bot-Key is never consulted.

func errAdminLoginRequired() error {
	return errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
}

func errAdminForbidden(permission string) error {
	return errors.New(403, "FORBIDDEN", "Admin does not have permission: "+permission)
}

func errAdminCSRF() error {
	return errors.New(403, "CSRF_INVALID", "Missing or invalid X-CSRF-Token header")
}

// requireAdminAuth resolves the kf_admin cookie into a live principal.
// It is the Admin-plane equivalent of requireOwnerAuth and shares
// nothing with it: a kf_owner cookie is never read here.
func (s *Server) requireAdminAuth(r *http.Request) (*admin.Principal, error) {
	cookie, err := r.Cookie(admin.AdminCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errAdminLoginRequired()
	}
	return admin.ResolveSession(r.Context(), s.Pool, cookie.Value)
}

// requireAdminPermission resolves the principal and enforces an exact
// permission code (wildcard * honored). No username/admin-id special
// cases.
func (s *Server) requireAdminPermission(r *http.Request, permission string) (*admin.Principal, error) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		return nil, err
	}
	if !principal.HasPermission(permission) {
		return nil, errAdminForbidden(permission)
	}
	return principal, nil
}

// requireAdminCSRF enforces the X-CSRF-Token mutation gate for the
// admin control plane.
func (s *Server) requireAdminCSRF(r *http.Request) error {
	cookie, err := r.Cookie(admin.AdminCookieName)
	if err != nil || cookie.Value == "" {
		return errAdminLoginRequired()
	}
	if !admin.VerifyCSRF(r, cookie.Value, s.Config.SessionSecret) {
		return errAdminCSRF()
	}
	return nil
}

func (s *Server) handleAdminSessionCreate(w http.ResponseWriter, r *http.Request) {
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	username, _ := input["username"].(string)
	password, _ := input["password"].(string)
	if username == "" || password == "" {
		MissingField(w, "username, password")
		return
	}

	ip := middleware.GetClientIP(r, s.TrustedProxies)
	rlResult := s.RateLimiter.CheckAdminLogin(ip)
	if !rlResult.Allowed {
		RateLimitResponse(w, rlResult.RetryAfter, rlResult.Limit, rlResult.Window)
		return
	}

	result, err := admin.Login(r.Context(), s.Pool, admin.LoginInput{
		Username:  username,
		Password:  password,
		IPAddress: ip,
		UserAgent: truncateAdminUA(r.UserAgent()),
	})
	if err != nil {
		// Best-effort failed-login audit (no privileged mutation
		// happened; the invariant allows best-effort here).
		if ae, ok := errors.IsAppError(err); ok && ae.Code == "INVALID_CREDENTIALS" {
			admin.RecordFailedLogin(r.Context(), s.Pool, username, ip,
				truncateAdminUA(r.UserAgent()), "INVALID_CREDENTIALS")
		}
		handleAppError(w, err)
		return
	}

	admin.SetAdminCookie(w, result.RawToken, middleware.IsHTTPS(r, s.TrustedProxies))

	SuccessResponse(w, map[string]interface{}{
		"id":           result.Admin.ID,
		"username":     result.Admin.Username,
		"display_name": result.Admin.DisplayName,
		"roles":        result.Roles,
		"csrf_token":   admin.CSRFToken(result.RawToken, s.Config.SessionSecret),
		"expires_at":   result.Session.ExpiresAt,
	}, "Admin login successful")
}

func (s *Server) handleAdminSessionGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	cookie, _ := r.Cookie(admin.AdminCookieName)
	rawToken := ""
	if cookie != nil {
		rawToken = cookie.Value
	}

	SuccessResponse(w, map[string]interface{}{
		"id":           principal.Admin.ID,
		"username":     principal.Admin.Username,
		"display_name": principal.Admin.DisplayName,
		"roles":        principal.Roles,
		"permissions":  principal.Permissions,
		"expires_at":   principal.Session.ExpiresAt,
		"csrf_token":   admin.CSRFToken(rawToken, s.Config.SessionSecret),
	}, "")
}

func (s *Server) handleAdminSessionDelete(w http.ResponseWriter, r *http.Request) {
	// A stale cookie must never block clearing the local login state.
	// The privileged revoke mutation runs ONLY for a valid session AND
	// a valid CSRF token.
	principal, authErr := s.requireAdminAuth(r)
	if authErr == nil {
		if csrfErr := s.requireAdminCSRF(r); csrfErr != nil {
			handleAppError(w, csrfErr)
			return
		}
		if err := admin.RevokeSession(r.Context(), s.Pool, principal); err != nil {
			handleAppError(w, err)
			return
		}
	}
	// Stale/absent session: still clear the cookie so the client can
	// always log out locally.
	admin.ClearAdminCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))
	SuccessResponse(w, map[string]interface{}{}, "Admin logout successful")
}

func truncateAdminUA(ua string) string {
	if len(ua) > 256 {
		return ua[:256]
	}
	return ua
}
