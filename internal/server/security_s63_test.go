package server

// S6.3 architecture guard: Owner/Admin cookie lifecycle derives HTTPS
// ONLY through the canonical middleware.IsHTTPS authority; no
// alternate direct X-Forwarded-Proto trust path survives.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func s63ProdFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, rel := range []string{
		"internal/server/handlers.go",
		"internal/server/handler_admin.go",
		"internal/server/handler_admin_b12.go",
		"internal/auth/session.go",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		files[rel] = string(b)
	}
	return files
}

// All Owner/Admin cookie set/clear call sites use the authority.
func TestS63CookieLifecycleUsesCanonicalHTTPSAuthority(t *testing.T) {
	files := s63ProdFiles(t)

	// owner login + logout (handlers.go)
	if !strings.Contains(files["internal/server/handlers.go"],
		"setOwnerCookie(w, result.BotID, s.Config.SessionSecret, middleware.IsHTTPS(r, s.TrustedProxies))") {
		t.Fatal("owner login does not use middleware.IsHTTPS")
	}
	if !strings.Contains(files["internal/server/handlers.go"],
		"clearOwnerCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))") {
		t.Fatal("owner logout does not use middleware.IsHTTPS")
	}

	// admin login + logout (handler_admin.go)
	if !strings.Contains(files["internal/server/handler_admin.go"],
		"admin.SetAdminCookie(w, result.RawToken, middleware.IsHTTPS(r, s.TrustedProxies))") {
		t.Fatal("admin login does not use middleware.IsHTTPS")
	}
	if !strings.Contains(files["internal/server/handler_admin.go"],
		"admin.ClearAdminCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))") {
		t.Fatal("admin logout does not use middleware.IsHTTPS")
	}

	// admin self password-change + self session-revoke cookie clears
	n := strings.Count(files["internal/server/handler_admin_b12.go"],
		"admin.ClearAdminCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))")
	if n != 2 {
		t.Fatalf("handler_admin_b12.go canonical authority sites = %d, want 2", n)
	}
}

// No alternate X-Forwarded-Proto trust path in production
// Owner/Admin/auth code, and auth.IsHTTPS is gone.
func TestS63NoAlternateForwardedProtoPath(t *testing.T) {
	files := s63ProdFiles(t)
	for rel, src := range files {
		if strings.Contains(src, `r.Header.Get("X-Forwarded-Proto")`) {
			t.Fatalf("%s still reads X-Forwarded-Proto directly", rel)
		}
		if strings.Contains(src, "func IsHTTPS(") {
			t.Fatalf("%s defines a second HTTPS helper", rel)
		}
	}

	// the obsolete auth.IsHTTPS export is gone
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "auth", "session.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "IsHTTPS") {
		t.Fatal("auth.IsHTTPS still exists")
	}
}

// The middleware package owns the single production XFP interpretation.
func TestS63MiddlewareOwnsForwardedProto(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "middleware", "client_ip.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "func IsHTTPS(") {
		t.Fatal("middleware.IsHTTPS missing")
	}
	// GetClientIP and IsHTTPS share the direct-peer trust predicate
	if !strings.Contains(string(b), "isTrustedProxy(") {
		t.Fatal("shared trust predicate missing")
	}
}
