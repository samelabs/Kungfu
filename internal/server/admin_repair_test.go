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

// UI static contract (re-audit repair): these assertions lock the
// RENDERED action construction and the fail-closed picker shapes —
// not merely the presence of handler branches.
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

	// -- role picker fail closed --
	// 1) catalog availability is a distinct gate, not a try/catch
	//    fallback to empty preselection.
	if !strings.Contains(users, "rolesCatalogReady") {
		t.Fatal("users.js must track verified catalog availability (rolesCatalogReady)")
	}
	// the silent fallback comment/shape must be gone
	if strings.Contains(users, "falls back to empty preselection") {
		t.Fatal("users.js still documents a fallback-to-empty picker path")
	}
	// 2) catalog load failure must propagate (throw), not be swallowed
	if strings.Contains(users, "} catch (e) { /* picker falls back") {
		t.Fatal("catalog load failure is silently swallowed")
	}
	// 3) the Roles button is rendered ONLY behind the verified gate —
	//    the rendered action construction must carry the condition.
	if !strings.Contains(users, "if (canRoles && rolesCatalogReady)") {
		t.Fatal("Roles button render must be gated on rolesCatalogReady")
	}
	// 4) unresolvable current role codes abort — no silent filter
	//    dropping them into an empty/checked-less preselection.
	if strings.Contains(users, ".filter(rid => typeof rid === 'number')") {
		t.Fatal("unresolved role codes must abort, not be filtered away")
	}
	if !strings.Contains(users, "typeof rid !== 'number'") {
		t.Fatal("picker must fail closed on unresolvable current role codes")
	}
	// 5) no-roles.read actor gets NO Roles button at all (button gate
	//    requires rolesCatalogReady which only a successful catalog
	//    load sets).
	if !strings.Contains(users, "if (hasPermission('admin.roles.read'))") {
		t.Fatal("catalog load must be permission-gated before it can set rolesCatalogReady")
	}

	// -- editdesc actually rendered --
	roles := read("roles.js")
	// the RENDERED actions array must construct the editdesc button —
	// checking only the event branch would pass with a dead button.
	editdescButton := `data-act="editdesc" data-id="${r.id}">Description</button>`
	if !strings.Contains(roles, editdescButton) {
		t.Fatal("renderRoles must construct a data-act=editdesc Description button")
	}
	if !strings.Contains(roles, "actions.push") {
		t.Fatal("rendered actions must be constructed via actions.push")
	}
	// and the event branch handles it
	if !strings.Contains(roles, "act === 'editdesc'") {
		t.Fatal("editdesc event branch missing")
	}
	// status toggle stays status-only
	if strings.Contains(roles, "name: btn.dataset.name") {
		t.Fatal("status toggle still sends name from data-name")
	}
	// create form description input exists in the template
	tmpl := readTemplateSource(t)
	if !strings.Contains(tmpl, `id="adminNewRoleDesc"`) {
		t.Fatal("role create form must include the description input")
	}
}

// readTemplateSource reads the admin template Go source that renders
// the create form (the HTML lives in Go string literals).
func readTemplateSource(t *testing.T) string {
	t.Helper()
	b, err := osReadFile(filepath.Join("templates_admin.go"))
	if err != nil {
		t.Fatalf("read templates_admin.go: %v", err)
	}
	return string(b)
}

func jsonDecode(body string, v interface{}) error {
	return json.Unmarshal([]byte(body), v)
}

func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Picker normal path through the real API: with the catalog loaded
// and current roles resolved, submitting the SAME role set back (the
// "open picker, change nothing, Apply" case) must leave bindings
// completely unchanged.
func TestRepairPickerApplyUnchangedKeepsBindings(t *testing.T) {
	e := newB12Env(t)

	target := fmt.Sprintf("pick_%d", time.Now().UnixNano())
	rec := e.mutateJSON(t, "POST", "/api/admin/users",
		fmt.Sprintf(`{"username":%q,"display_name":"Pick","password":"pick-pass-123"}`, target))
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

	// bind superadmin (what the prechecked picker would show checked)
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
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/users/%d/roles", targetID),
		fmt.Sprintf(`{"role_ids":[%d]}`, superID))
	if rec.Code != 200 {
		t.Fatalf("bind: %d", rec.Code)
	}

	// "Apply without changes": submit the same set back
	rec = e.mutateJSON(t, "PUT", fmt.Sprintf("/api/admin/users/%d/roles", targetID),
		fmt.Sprintf(`{"role_ids":[%d]}`, superID))
	if rec.Code != 200 {
		t.Fatalf("re-apply: %d %s", rec.Code, rec.Body.String())
	}
	bindings := e.bindingsOf(t, targetID)
	if len(bindings) != 1 || bindings[0] != superID {
		t.Fatalf("unchanged Apply altered bindings: %v", bindings)
	}
}
