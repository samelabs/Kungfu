package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
)

// B1.2 Admin Management HTTP surface. Discipline per work order:
// handler = parse HTTP → resolve principal → CSRF → permission →
// domain call → serialize. No handler SQL, no handler business rules.

// -- serialization allowlists (never json.Marshal a DB model) --

func adminUserDTO(a *model.Admin, roles []string) map[string]interface{} {
	if roles == nil {
		roles = []string{}
	}
	return map[string]interface{}{
		"id":                  a.ID,
		"username":            a.Username,
		"display_name":        a.DisplayName,
		"status":              a.Status,
		"roles":               roles,
		"last_login_at":       nullableTimeJSON(a.LastLoginAt),
		"password_changed_at": a.PasswordChangedAt,
		"created_at":          a.CreatedAt,
		"updated_at":          a.UpdatedAt,
	}
}

func adminRoleDTO(r *model.AdminRole, permissions []string) map[string]interface{} {
	if permissions == nil {
		permissions = []string{}
	}
	return map[string]interface{}{
		"id":          r.ID,
		"code":        r.Code,
		"name":        r.Name,
		"description": nullableStringJSON(r.Description),
		"is_system":   r.IsSystem,
		"status":      r.Status,
		"permissions": permissions,
		"created_at":  r.CreatedAt,
		"updated_at":  r.UpdatedAt,
	}
}

func adminSessionDTO(s *model.AdminSessionInfo) map[string]interface{} {
	return map[string]interface{}{
		"id":           s.ID,
		"admin_id":     s.AdminID,
		"username":     s.Username,
		"display_name": s.DisplayName,
		"ip_address":   nullableStringJSON(s.IPAddress),
		"user_agent":   nullableStringJSON(s.UserAgent),
		"created_at":   s.CreatedAt,
		"last_seen_at": s.LastSeenAt,
		"expires_at":   s.ExpiresAt,
		"revoked_at":   nullableTimeJSON(s.RevokedAt),
		"active":       s.RevokedAt == nil,
	}
}

func nullableStringJSON(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

func nullableTimeJSON(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}

// -- helper: resolve + CSRF + permission in one call --

func (s *Server) requireAdminMutation(r *http.Request, permission string) (*admin.Principal, error) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		return nil, err
	}
	if err := s.requireAdminCSRF(r); err != nil {
		return nil, err
	}
	if permission != "" && !principal.HasPermission(permission) {
		return nil, errAdminForbidden(permission)
	}
	return principal, nil
}

// pathID parses a positive {id} URL parameter.
func pathID(r *http.Request, name string) (int64, error) {
	v := r.PathValue(name)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New(404, "NOT_FOUND", "Resource not found")
	}
	return id, nil
}

// -- Admin users --

func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	users, err := admin.ListUsers(r.Context(), s.Pool, principal)
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		out = append(out, adminUserDTO(u.Admin, u.Roles))
	}
	SuccessResponse(w, map[string]interface{}{"users": out}, "")
}

func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	username, _ := input["username"].(string)
	displayName, _ := input["display_name"].(string)
	password, _ := input["password"].(string)

	principal, err := s.requireAdminMutation(r, "admin.users.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	created, err := admin.CreateAdmin(r.Context(), s.Pool, principal, admin.CreateAdminInput{
		Username: username, DisplayName: displayName, Password: password,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminUserDTO(created, []string{}), "Admin created")
}

func (s *Server) handleAdminUserGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	u, err := admin.GetUser(r.Context(), s.Pool, principal, adminID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminUserDTO(u.Admin, u.Roles), "")
}

func (s *Server) handleAdminUserPatch(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.users.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	displayName, ok := input["display_name"].(string)
	if !ok {
		MissingField(w, "display_name")
		return
	}
	updated, err := admin.UpdateAdminDisplayName(r.Context(), s.Pool, principal, adminID, displayName)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminUserDTO(updated, nil), "Admin updated")
}

func (s *Server) handleAdminUserDisable(w http.ResponseWriter, r *http.Request) {
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.users.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	if err := admin.DisableAdmin(r.Context(), s.Pool, principal, adminID); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": adminID, "status": "disabled"}, "Admin disabled")
}

func (s *Server) handleAdminUserEnable(w http.ResponseWriter, r *http.Request) {
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.users.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	if err := admin.EnableAdmin(r.Context(), s.Pool, principal, adminID); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": adminID, "status": "active"}, "Admin enabled")
}

func (s *Server) handleAdminUserRoles(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.roles.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	// Finding 3 (fail closed): role_ids must be PRESENT and a JSON
	// array; every element must be a positive integer. Empty [] is a
	// legal explicit clear. Malformed input = 400 with ZERO mutation.
	rawIDs, present := input["role_ids"].([]interface{})
	if !present {
		if _, exists := input["role_ids"]; !exists {
			MissingField(w, "role_ids")
			return
		}
		// present but not an array
		ErrorResponse(w, 400, "INVALID_ROLE_IDS", "role_ids must be a JSON array of positive integers", nil)
		return
	}
	roleIDs := make([]int64, 0, len(rawIDs))
	for _, v := range rawIDs {
		f, ok := v.(float64)
		if !ok || f <= 0 || f != float64(int64(f)) {
			ErrorResponse(w, 400, "INVALID_ROLE_IDS", "role_ids must be a JSON array of positive integers", nil)
			return
		}
		roleIDs = append(roleIDs, int64(f))
	}
	if err := admin.SetAdminRoles(r.Context(), s.Pool, principal, adminID, roleIDs); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": adminID, "role_ids": roleIDs}, "Roles updated")
}

func (s *Server) handleAdminUserPassword(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.users.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	password, _ := input["password"].(string)
	if password == "" {
		MissingField(w, "password")
		return
	}
	if err := admin.ResetAdminPassword(r.Context(), s.Pool, principal, adminID, password); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": adminID}, "Password reset")
}

func (s *Server) handleAdminUserForceLogout(w http.ResponseWriter, r *http.Request) {
	adminID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.sessions.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	if err := admin.ForceLogoutAdmin(r.Context(), s.Pool, principal, adminID); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": adminID}, "All sessions revoked")
}

// -- Self password change --

func (s *Server) handleAdminMePassword(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	// CSRF required; NO specific permission (any active admin).
	principal, err := s.requireAdminMutation(r, "")
	if err != nil {
		handleAppError(w, err)
		return
	}
	currentPassword, _ := input["current_password"].(string)
	newPassword, _ := input["new_password"].(string)
	if currentPassword == "" || newPassword == "" {
		MissingField(w, "current_password, new_password")
		return
	}
	if err := admin.ChangeOwnPassword(r.Context(), s.Pool, principal, currentPassword, newPassword); err != nil {
		handleAppError(w, err)
		return
	}
	// All sessions (including this one) are revoked: clear the local
	// cookie so the browser drops its login state.
	isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	admin.ClearAdminCookie(w, isHTTPS)
	SuccessResponse(w, map[string]interface{}{}, "Password changed — please log in again")
}

// -- Roles --

func (s *Server) handleAdminRolesList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	roles, err := admin.ListRoles(r.Context(), s.Pool, principal)
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(roles))
	for _, rv := range roles {
		out = append(out, adminRoleDTO(rv.Role, rv.Permissions))
	}
	SuccessResponse(w, map[string]interface{}{"roles": out}, "")
}

func (s *Server) handleAdminRolesCreate(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.roles.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	code, _ := input["code"].(string)
	name, _ := input["name"].(string)
	description, _ := input["description"].(string)
	created, err := admin.CreateRole(r.Context(), s.Pool, principal, admin.CreateRoleInput{
		Code: code, Name: name, Description: description,
	})
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRoleDTO(created, []string{}), "Role created")
}

func (s *Server) handleAdminRoleGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	roleID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	rv, err := admin.GetRole(r.Context(), s.Pool, principal, roleID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRoleDTO(rv.Role, rv.Permissions), "")
}

// handleAdminRolePatch: PARTIAL update. Omitted fields preserve their
// current values; at least one field must be present. Provided-but-
// wrong-type fields are 400s (fail closed).
func (s *Server) handleAdminRolePatch(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	roleID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.roles.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	patch := admin.RolePatch{}
	provided := 0
	if v, exists := input["name"]; exists {
		sv, ok := v.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_ROLE_NAME", "name must be a string", nil)
			return
		}
		patch.Name = &sv
		provided++
	}
	if v, exists := input["description"]; exists {
		sv, ok := v.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_ROLE_DESCRIPTION", "description must be a string", nil)
			return
		}
		patch.Description = &sv
		provided++
	}
	if v, exists := input["status"]; exists {
		sv, ok := v.(string)
		if !ok {
			ErrorResponse(w, 400, "INVALID_ROLE_STATUS", "status must be a string", nil)
			return
		}
		patch.Status = &sv
		provided++
	}
	if provided == 0 {
		ErrorResponse(w, 400, "EMPTY_PATCH", "PATCH must include at least one of name, description, status", nil)
		return
	}
	updated, err := admin.UpdateRole(r.Context(), s.Pool, principal, roleID, patch)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, adminRoleDTO(updated, nil), "Role updated")
}

// handleAdminRolePermissions: complete replacement. permission_codes
// must be PRESENT and a JSON array; every element must be a non-empty
// string (after trim). Empty [] is a legal explicit clear.
func (s *Server) handleAdminRolePermissions(w http.ResponseWriter, r *http.Request) {
	input, err := parseAdminJSONBody(r)
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	roleID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.roles.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	rawCodes, present := input["permission_codes"].([]interface{})
	if !present {
		if _, exists := input["permission_codes"]; !exists {
			MissingField(w, "permission_codes")
			return
		}
		ErrorResponse(w, 400, "INVALID_PERMISSION_CODES", "permission_codes must be a JSON array of strings", nil)
		return
	}
	codes := make([]string, 0, len(rawCodes))
	for _, v := range rawCodes {
		c, ok := v.(string)
		if !ok || strings.TrimSpace(c) == "" {
			ErrorResponse(w, 400, "INVALID_PERMISSION_CODES", "permission_codes must be a JSON array of strings", nil)
			return
		}
		codes = append(codes, c)
	}
	if err := admin.SetRolePermissions(r.Context(), s.Pool, principal, roleID, codes); err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{"id": roleID, "permission_codes": codes}, "Permissions updated")
}

// -- Permissions --

func (s *Server) handleAdminPermissionsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	perms, err := admin.ListPermissions(r.Context(), s.Pool, principal)
	if err != nil {
		handleAppError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(perms))
	for _, p := range perms {
		out = append(out, map[string]interface{}{
			"code":        p.Code,
			"description": nullableStringJSON(p.Description),
		})
	}
	SuccessResponse(w, map[string]interface{}{"permissions": out}, "")
}

// -- Sessions --

func (s *Server) handleAdminSessionsList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	sessions, err := admin.ListSessions(r.Context(), s.Pool, principal)
	if err != nil {
		handleAppError(w, err)
		return
	}
	adminIDFilter := strings.TrimSpace(r.URL.Query().Get("admin_id"))
	out := make([]map[string]interface{}, 0, len(sessions))
	for _, si := range sessions {
		if adminIDFilter != "" && strconv.FormatInt(si.AdminID, 10) != adminIDFilter {
			continue
		}
		out = append(out, adminSessionDTO(si))
	}
	SuccessResponse(w, map[string]interface{}{"sessions": out}, "")
}

func (s *Server) handleAdminSessionRevoke(w http.ResponseWriter, r *http.Request) {
	sessionID, err := pathID(r, "id")
	if err != nil {
		handleAppError(w, err)
		return
	}
	principal, err := s.requireAdminMutation(r, "admin.sessions.manage")
	if err != nil {
		handleAppError(w, err)
		return
	}
	revoked, err := admin.RevokeSessionByID(r.Context(), s.Pool, principal, sessionID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	// If the actor revoked their OWN current session, clear the local
	// cookie (the mutation itself already succeeded).
	if revoked.AdminID == principal.Admin.ID && revoked.ID == principal.Session.ID {
		isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
		admin.ClearAdminCookie(w, isHTTPS)
	}
	SuccessResponse(w, adminSessionDTO(revoked), "Session revoked")
}

// -- Audit explorer --

func (s *Server) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	q := r.URL.Query()
	page, pageSize := 1, 50
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(q.Get("page_size")); err == nil && v > 0 {
		pageSize = v
		if pageSize > 100 {
			pageSize = 100
		}
	}
	filter := admin.AuditFilter{
		ActorUsername: strings.TrimSpace(q.Get("actor_username")),
		Action:        strings.TrimSpace(q.Get("action")),
		TargetType:    strings.TrimSpace(q.Get("target_type")),
		TargetID:      strings.TrimSpace(q.Get("target_id")),
		Page:          page,
		PageSize:      pageSize,
	}
	if v := strings.TrimSpace(q.Get("actor_admin_id")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			filter.ActorAdminID = &id
		}
	}
	if v := strings.TrimSpace(q.Get("success")); v == "true" || v == "false" {
		b := v == "true"
		filter.Success = &b
	}

	result, err := admin.ExploreAudit(r.Context(), s.Pool, principal, filter)
	if err != nil {
		handleAppError(w, err)
		return
	}
	items := make([]map[string]interface{}, 0, len(result.Items))
	for _, l := range result.Items {
		items = append(items, adminAuditDTO(l))
	}
	SuccessResponse(w, map[string]interface{}{
		"items":     items,
		"page":      result.Page,
		"page_size": result.PageSize,
		"total":     result.Total,
	}, "")
}

// parseAdminJSONBody reuses the shared JSON body parser.
func parseAdminJSONBody(r *http.Request) (map[string]interface{}, error) {
	return parseJSONBodyRequired(r, true, "Request body must be valid JSON")
}

// -- serialization allowlists (never json.Marshal a DB model) --

func nullableInt64JSON(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func jsonBytesOrNull(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}

func adminAuditDTO(l *model.AdminAuditLog) map[string]interface{} {
	return map[string]interface{}{
		"id":             l.ID,
		"actor_admin_id": nullableInt64JSON(l.ActorAdminID),
		"actor_username": l.ActorUsername,
		"action":         l.Action,
		"target_type":    nullableStringJSON(l.TargetType),
		"target_id":      nullableStringJSON(l.TargetID),
		"success":        l.Success,
		"before_json":    jsonBytesOrNull(l.BeforeJSON),
		"after_json":     jsonBytesOrNull(l.AfterJSON),
		"metadata_json":  jsonBytesOrNull(l.MetadataJSON),
		"ip_address":     nullableStringJSON(l.IPAddress),
		"user_agent":     nullableStringJSON(l.UserAgent),
		"error_code":     nullableStringJSON(l.ErrorCode),
		"created_at":     l.CreatedAt,
	}
}
