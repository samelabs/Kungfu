package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Architecture guards (B1.1). These tests walk the SOURCE TREE of the
// repository and pin the structural invariants of the Admin plane.

func repoRoot(t *testing.T) string {
	t.Helper()
	// test runs in internal/admin → root is ../../
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found: %v", err)
	}
	return root
}

// walkSources yields every non-test .go file under dir.
func walkSources(t *testing.T, dir string, fn func(path, src string)) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fn(path, string(src))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Guard: the Admin domain may depend ONLY on auth (password utility),
// pg, repository, model, errors, and security utilities. No store,
// payment, credits, task, storage, service, or server dependency.
func TestAdminDomainDependencyAllowlist(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		"kungfu.md/internal/auth":       true,
		"kungfu.md/internal/pg":         true,
		"kungfu.md/internal/repository": true,
		"kungfu.md/internal/model":      true,
		"kungfu.md/internal/errors":     true,
		"kungfu.md/internal/security":   true,
	}
	banned := []string{
		"kungfu.md/internal/store", "kungfu.md/internal/payment",
		"kungfu.md/internal/credits", "kungfu.md/internal/task",
		"kungfu.md/internal/storage", "kungfu.md/internal/service",
		"kungfu.md/internal/server", "kungfu.md/internal/consumption",
		"kungfu.md/internal/delivery",
	}
	walkSources(t, filepath.Join(root, "internal", "admin"), func(path, src string) {
		for _, imp := range banned {
			if strings.Contains(src, `"`+imp) {
				t.Errorf("%s: admin domain must not import %s", path, imp)
			}
		}
		for line := range strings.SplitSeq(src, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "\"kungfu.md/") {
				continue
			}
			imp := strings.Trim(line, "\"")
			if !allowed[imp] {
				t.Errorf("%s: admin domain imports non-allowlisted package %s", path, imp)
			}
		}
	})
}

// codeLines strips comment lines from a Go source (comments may name
// the plane separation contract; only code matters for the guard).
func codeLines(src string) []string {
	var out []string
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Guard: Store must not import admin (Admin Foundation cannot be a
// Store dependency in either direction).
func TestStoreDoesNotImportAdmin(t *testing.T) {
	root := repoRoot(t)
	walkSources(t, filepath.Join(root, "internal"), func(path, src string) {
		if strings.Contains(path, "/admin/") {
			return
		}
		// server and cmd are the sanctioned wiring points.
		if !strings.HasPrefix(path, filepath.Join(root, "internal", "server")+string(filepath.Separator)) &&
			!strings.HasPrefix(path, filepath.Join(root, "internal", "admin")) &&
			!strings.Contains(path, string(filepath.Separator)+"cmd"+string(filepath.Separator)) {
			for _, line := range codeLines(src) {
				if strings.Contains(strings.TrimSpace(line), "\"kungfu.md/internal/admin\"") {
					t.Errorf("%s: non-admin production code imports internal/admin (only server/cmd may)", path)
				}
			}
		}
	})
	// server must wire the admin routes
	wired := false
	walkSources(t, filepath.Join(root, "internal", "server"), func(path, src string) {
		if strings.Contains(src, "\"kungfu.md/internal/admin\"") {
			wired = true
		}
	})
	if !wired {
		t.Error("internal/server must wire the admin routes (handler_admin.go)")
	}
}

// Guard: Owner session helper ≠ Admin authorization. The owner auth
// path must not resolve admin identity and vice versa: the owner
// cookie name and admin cookie name differ, and no production file
// may cross-check kf_owner inside admin code / kf_admin inside owner
// auth code.
func TestOwnerAndAdminPlanesAreSeparate(t *testing.T) {
	root := repoRoot(t)

	// 1) admin package never references the owner cookie or owner session (code lines only)
	walkSources(t, filepath.Join(root, "internal", "admin"), func(path, src string) {
		for _, line := range codeLines(src) {
			if strings.Contains(line, "kf_owner") || strings.Contains(line, "OwnerSession") {
				t.Errorf("%s: admin domain must not touch the Owner session: %s", path, strings.TrimSpace(line))
			}
		}
	})

	// 2) owner auth (internal/auth/session.go) never references admin cookie (code lines only)
	ownerSrc, err := os.ReadFile(filepath.Join(root, "internal", "auth", "session.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range codeLines(string(ownerSrc)) {
		if strings.Contains(line, "kf_admin") {
			t.Errorf("owner session code must not reference kf_admin: %s", strings.TrimSpace(line))
		}
	}

	// 3) requireAdminAuth reads ONLY the kf_admin cookie (code lines only)
	h, err := os.ReadFile(filepath.Join(root, "internal", "server", "handler_admin.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range codeLines(string(h)) {
		if strings.Contains(line, "OwnerCookieName") || strings.Contains(line, "kf_owner") {
			t.Errorf("admin handler must never consult the owner cookie: %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "X-Bot-Key") {
			t.Errorf("admin handler must never consult X-Bot-Key: %s", strings.TrimSpace(line))
		}
	}
	hs := string(h)
	if !strings.Contains(hs, "admin.AdminCookieName") {
		t.Error("requireAdminAuth must resolve via admin.AdminCookieName (kf_admin)")
	}
}

// Guard: tb_admin_audit_logs is append-only — no production UPDATE or
// DELETE against it anywhere outside migration files and this guard.
func TestAdminAuditLogsAppendOnlyInProductionCode(t *testing.T) {
	root := repoRoot(t)
	walkSources(t, filepath.Join(root, "internal"), func(path, src string) {
		if strings.Contains(path, "/admin/") && strings.HasSuffix(path, "architecture_guard_test.go") {
			return
		}
		for line := range strings.SplitSeq(src, "\n") {
			up := strings.Contains(strings.ToUpper(line), "UPDATE TB_ADMIN_AUDIT_LOGS")
			del := strings.Contains(strings.ToUpper(line), "DELETE FROM TB_ADMIN_AUDIT_LOGS")
			if up || del {
				t.Errorf("%s: production code must not UPDATE/DELETE tb_admin_audit_logs: %s", path, strings.TrimSpace(line))
			}
		}
	})
	// repository exposes no update/delete API for the audit table
	repoSrc, err := os.ReadFile(filepath.Join(root, "internal", "repository", "admin.go"))
	if err != nil {
		t.Fatal(err)
	}
	rs := string(repoSrc)
	if strings.Contains(strings.ToUpper(rs), "UPDATE TB_ADMIN_AUDIT_LOGS") ||
		strings.Contains(strings.ToUpper(rs), "DELETE FROM TB_ADMIN_AUDIT_LOGS") {
		t.Error("repository must not expose audit UPDATE/DELETE SQL")
	}
}

// Guard: the raw admin session token must never reach the DB or logs.
// Structural checks: no production admin/server code writes the raw
// token into any INSERT/UPDATE or log call; the model has no raw
// token field.
func TestRawAdminSessionTokenNeverPersistedOrLogged(t *testing.T) {
	root := repoRoot(t)

	// model must not have a raw token field
	modelSrc, err := os.ReadFile(filepath.Join(root, "internal", "model", "admin.go"))
	if err != nil {
		t.Fatal(err)
	}
	ms := string(modelSrc)
	if strings.Contains(ms, "RawToken") {
		t.Error("model.AdminSession must not carry a raw token field")
	}

	// repository session SQL: token_hash columns only; no token_plain
	repoSrc, err := os.ReadFile(filepath.Join(root, "internal", "repository", "admin.go"))
	if err != nil {
		t.Fatal(err)
	}
	rs := string(repoSrc)
	if strings.Contains(strings.ToLower(rs), "rawtoken") {
		t.Error("repository must not reference raw tokens")
	}

	// production code must not log the raw token
	for _, dir := range []string{filepath.Join(root, "internal", "admin"), filepath.Join(root, "internal", "server")} {
		walkSources(t, dir, func(path, src string) {
			for line := range strings.SplitSeq(src, "\n") {
				l := strings.ToLower(line)
				if (strings.Contains(l, "log.print") || strings.Contains(l, "log.fatal")) &&
					strings.Contains(l, "rawtoken") {
					t.Errorf("%s: raw token must not be logged: %s", path, strings.TrimSpace(line))
				}
			}
		})
	}
}

// Guard: migration 006 must not modify tables owned by 001–005 — only
// CREATE new objects plus seed inserts.
func TestMigration006IsAdditiveOnly(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "migrations", "006_admin_foundation.sql"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ToUpper(string(src))
	for _, banned := range []string{"DROP TABLE", "ALTER TABLE TB_BOTS", "ALTER TABLE TB_PAYMENTS",
		"ALTER TABLE TB_STORE", "ALTER TABLE TB_REDEMPTIONS", "ALTER TABLE TB_LOGS",
		"ALTER TABLE TB_TRANSACTIONS", "ALTER TABLE TB_TASK", "ALTER TABLE TB_KUNGFUS"} {
		if strings.Contains(s, banned) {
			t.Errorf("006 must not contain %s", banned)
		}
	}
}
