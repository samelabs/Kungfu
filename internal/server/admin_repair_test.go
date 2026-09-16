package server

// B1.2 repair HTTP tests: fail-closed replacement payloads, partial
// role PATCH through the real router, UI static contract.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mutateJSON: authenticated mutation with CSRF.
func (e *b12Env) mutateJSON(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.cookie)
	req.Header.Set("X-CSRF-Token", e.csrf)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestRepairPutRolesFailClosed(t *testing.T) {
	e := newB12Env(t)

	// seed a target admin with a known binding
	target := fmt.Sprintf("fcuser_%d", time.Now().UnixNano())
	rec := e.mutateJSON(t, "POST", "/api/admin/users",
		fmt.Sprintf(`{"username":%q,"display_name":"FC","password":"fc-pass-123"}`, target))
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &created)
	targetID := created.Data.ID

	// find a role id to bind
	rolesRec := e.do(t, "GET", "/api/admin/roles", "", false)
	var rolesResp struct {
		Data struct {
			Roles []struct {
				ID   int64  `json:"id"`
				Code string `json:"code"`
			} `json:"roles"`
		} `json:"data"`
	}
	_ = jsonDecode(rolesRec.Body.String(), &rolesResp)
	var superID int64
	for _, r := range rolesResp.Data.Roles {
		if r.Code == "superadmin" {
			superID = r.ID
		}
	}

	// baseline binding: [superID]
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/users/%d/roles", targetID),
		fmt.Sprintf(`{"role_ids":[%d]}`, superID))
	if rec.Code != 200 {
		t.Fatalf("baseline bind: %d %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name string
		body string
	}{
		{"missing field", `{}`},
		{"wrong type", `{"role_ids":"nope"}`},
		{"mixed valid+invalid", `{"role_ids":[12345, -1]}`},
		{"non-integer entry", `{"role_ids":[1.5]}`},
		{"string entry", `{"role_ids":["superadmin"]}`},
	}
	for _, tc := range cases {
		rec := e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/users/%d/roles", targetID), tc.body)
		if rec.Code != 400 {
			t.Fatalf("%s: got %d %s, want 400", tc.name, rec.Code, rec.Body.String())
		}
		// binding unchanged
		bindings := e.bindingsOf(t, targetID)
		if len(bindings) != 1 || bindings[0] != superID {
			t.Fatalf("%s: bindings changed to %v", tc.name, bindings)
		}
	}

	// valid empty array = legal explicit clear
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/users/%d/roles", targetID), `{"role_ids":[]}`)
	if rec.Code != 200 {
		t.Fatalf("empty array clear: %d %s", rec.Code, rec.Body.String())
	}
	if b := e.bindingsOf(t, targetID); len(b) != 0 {
		t.Fatalf("empty array must clear bindings, got %v", b)
	}
}

func (e *b12Env) bindingsOf(t *testing.T, adminID int64) []int64 {
	t.Helper()
	rec := e.do(t, "GET", fmt.Sprintf("/api/admin/users/%d", adminID), "", false)
	if rec.Code != 200 {
		t.Fatalf("get user: %d", rec.Code)
	}
	// roles are codes; map via roles list to ids
	var userResp struct {
		Data struct {
			Roles []string `json:"roles"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &userResp)
	rolesRec := e.do(t, "GET", "/api/admin/roles", "", false)
	var rolesResp struct {
		Data struct {
			Roles []struct {
				ID   int64  `json:"id"`
				Code string `json:"code"`
			} `json:"roles"`
		} `json:"data"`
	}
	_ = jsonDecode(rolesRec.Body.String(), &rolesResp)
	codeToID := map[string]int64{}
	for _, r := range rolesResp.Data.Roles {
		codeToID[r.Code] = r.ID
	}
	var out []int64
	for _, c := range userResp.Data.Roles {
		if id, ok := codeToID[c]; ok {
			out = append(out, id)
		}
	}
	return out
}

func TestRepairPutPermissionsFailClosed(t *testing.T) {
	e := newB12Env(t)

	// create a custom role
	rec := e.mutateJSON(t, "POST", "/api/admin/roles",
		fmt.Sprintf(`{"code":"fcperm_%d","name":"FC Perm"}`, time.Now().UnixNano()%100000))
	if rec.Code != 200 {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &created)
	roleID := created.Data.ID

	// baseline permission binding
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/roles/%d/permissions", roleID),
		`{"permission_codes":["admin.users.read"]}`)
	if rec.Code != 200 {
		t.Fatalf("baseline: %d %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name string
		body string
	}{
		{"missing field", `{}`},
		{"wrong type", `{"permission_codes":42}`},
		{"mixed valid+invalid", `{"permission_codes":["admin.users.read","   "]}`},
		{"non-string entry", `{"permission_codes":["admin.users.read",7]}`},
	}
	for _, tc := range cases {
		rec := e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/roles/%d/permissions", roleID), tc.body)
		if rec.Code != 400 {
			t.Fatalf("%s: got %d %s, want 400", tc.name, rec.Code, rec.Body.String())
		}
		perms := e.permsOf(t, roleID)
		if len(perms) != 1 || perms[0] != "admin.users.read" {
			t.Fatalf("%s: bindings changed to %v", tc.name, perms)
		}
	}

	// valid empty array clears
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/roles/%d/permissions", roleID), `{"permission_codes":[]}`)
	if rec.Code != 200 {
		t.Fatalf("empty clear: %d %s", rec.Code, rec.Body.String())
	}
	if p := e.permsOf(t, roleID); len(p) != 0 {
		t.Fatalf("empty array must clear, got %v", p)
	}
}

func (e *b12Env) permsOf(t *testing.T, roleID int64) []string {
	t.Helper()
	rec := e.do(t, "GET", fmt.Sprintf("/api/admin/roles/%d", roleID), "", false)
	if rec.Code != 200 {
		t.Fatalf("get role: %d", rec.Code)
	}
	var resp struct {
		Data struct {
			Permissions []string `json:"permissions"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &resp)
	return resp.Data.Permissions
}

func TestRepairPartialRolePATCHViaRouter(t *testing.T) {
	e := newB12Env(t)

	// create + disable a role
	code := fmt.Sprintf("patchrole_%d", time.Now().UnixNano()%100000)
	rec := e.mutateJSON(t, "POST", "/api/admin/roles", fmt.Sprintf(`{"code":%q,"name":"Patch Me"}`, code))
	if rec.Code != 200 {
		t.Fatalf("create: %d", rec.Code)
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &created)
	roleID := created.Data.ID

	rec = e.mutateJSON(t, "PATCH", fmt.Sprintf("/api/admin/roles/%d", roleID), `{"status":"disabled"}`)
	if rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}

	// name-only PATCH → status stays disabled
	rec = e.mutateJSON(t, "PATCH", fmt.Sprintf("/api/admin/roles/%d", roleID), `{"name":"Patched Name"}`)
	if rec.Code != 200 {
		t.Fatalf("name patch: %d %s", rec.Code, rec.Body.String())
	}
	var role struct {
		Data struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	_ = jsonDecode(rec.Body.String(), &role)
	if role.Data.Name != "Patched Name" || role.Data.Status != "disabled" {
		t.Fatalf("partial PATCH broken: %+v", role.Data)
	}

	// description-only PATCH keeps name+status
	rec = e.mutateJSON(t, "PATCH", fmt.Sprintf("/api/admin/roles/%d", roleID), `{"description":"added later"}`)
	if rec.Code != 200 {
		t.Fatalf("desc patch: %d %s", rec.Code, rec.Body.String())
	}
	_ = jsonDecode(rec.Body.String(), &role)
	if role.Data.Name != "Patched Name" || role.Data.Status != "disabled" {
		t.Fatalf("desc patch disturbed other fields: %+v", role.Data)
	}

	// empty PATCH → 400
	rec = e.mutateJSON(t, "PATCH", fmt.Sprintf("/api/admin/roles/%d", roleID), `{}`)
	if rec.Code != 400 {
		t.Fatalf("empty patch: %d", rec.Code)
	}
	// wrong type → 400
	rec = e.mutateJSON(t, "PATCH", fmt.Sprintf("/api/admin/roles/%d", roleID), `{"name":123}`)
	if rec.Code != 400 {
		t.Fatalf("wrong type: %d", rec.Code)
	}
}

// UI static contract: the role picker must preselect current roles
// (no empty-array overlay call), and status toggles must not depend
// on data-name.
func TestRepairAdminUIStaticContract(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		b, err := osReadFile(filepath.Join("..", "..", "web", "assets", "admin", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	users := read("users.js")
	if strings.Contains(users, "rolePickerOverlay('Roles for ' + (target ? target.username : '#' + id),\n                    [],") {
		t.Fatal("role picker still passes an empty currentRoleIds array")
	}
	if !strings.Contains(users, "currentRoleIds") {
		t.Fatal("users.js must build currentRoleIds from the target's roles")
	}

	roles := read("roles.js")
	// status toggle must be status-only PATCH (no name field)
	if strings.Contains(roles, "name: btn.dataset.name") {
		t.Fatal("status toggle still sends name from data-name")
	}
	if !strings.Contains(roles, `{ status: act === 'disable' ? 'disabled' : 'active' }`) &&
		!strings.Contains(roles, "status: act === 'disable' ? 'disabled' : 'active'") {
		t.Fatal("status toggle must be a status-only PATCH")
	}
	// description editing must exist and be a description-only PATCH
	if !strings.Contains(roles, "editdesc") {
		t.Fatal("role description edit action missing")
	}
	if !strings.Contains(roles, "adminNewRoleDesc") {
		t.Fatal("role create form must support description")
	}
}

func jsonDecode(body string, v interface{}) error {
	return json.Unmarshal([]byte(body), v)
}

func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
