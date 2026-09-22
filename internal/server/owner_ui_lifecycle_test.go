package server

// Owner UI production-defect tests: shell + section lifecycle
// (loading/empty/unavailable/error distinct), structured API error
// propagation, balance-first-paint, and the static-asset cache
// invalidation contract (no manual ?v bumps; SW network-first for
// code; server no-cache for JS/CSS).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"kungfu.md/internal/i18n"
)

// renderOwnerShellForTest renders the owner SPA shell HTML through the
// same test-server construction used by the existing Owner UI tests.
func renderOwnerShellForTest(t *testing.T, locale, section string) string {
	t.Helper()
	s := storeTestServer(t)
	router := s.buildRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner", nil)
	if locale != "en" {
		req.URL.RawQuery = "lang=" + locale
	}
	router.ServeHTTP(rec, req)
	return rec.Body.String()
}

func ownerAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "owner", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func swSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "sw.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ── Asset cache invalidation ────────────────────────────────────────

// NoOwnerShellManualVersionBust: the Owner shell must not carry a
// hand-bumped ?v=N cache-bust query on any asset URL.
func TestNoOwnerShellManualVersionBust(t *testing.T) {
	body := renderOwnerShellForTest(t, "en", "overview")
	if regexp.MustCompile(`\?v=\d+`).MatchString(body) {
		t.Fatal("owner shell still contains a manual ?v=N cache-bust query")
	}
	for _, asset := range []string{"core.js", "lifecycle.js", "api.js", "init.js"} {
		if !strings.Contains(body, "/assets/owner/"+asset) {
			t.Fatalf("expected owner shell asset %s missing", asset)
		}
	}
	if !strings.Contains(body, "/assets/owner.css") {
		t.Fatal("expected owner shell stylesheet /assets/owner.css missing")
	}
}

// CodeAssetsRevalidate: /assets JS/CSS responses must not be served
// with a long fresh window; images/fonts keep theirs (policies are
// deliberately separated).
func TestCodeAssetsRevalidate(t *testing.T) {
	handler := serveAssets()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/owner/core.js", nil)
	handler(rec, req)
	cc := rec.Header().Get("Cache-Control")
	if strings.Contains(cc, "max-age=300") {
		t.Fatalf("JS served with stale-fresh window: %q", cc)
	}
	if !strings.Contains(cc, "no-cache") {
		t.Fatalf("JS must revalidate, got Cache-Control %q", cc)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/assets/icons/app-icon.svg", nil)
	handler(rec2, req2)
	cc2 := rec2.Header().Get("Cache-Control")
	if !strings.Contains(cc2, "max-age=300") {
		t.Fatalf("image assets keep their fresh window, got %q", cc2)
	}
}

// SWNetworkFirstForCode: the service worker must fetch scripts/styles
// network-first (no cached-code-first), keep images/fonts cache-first,
// and migrate away from the old v3 caches.
func TestSWNetworkFirstForCode(t *testing.T) {
	sw := swSource(t)
	if !strings.Contains(sw, "kungfu-pwa-v4") {
		t.Fatal("SW must bump cache version to migrate old v3 caches")
	}
	if strings.Contains(sw, "kungfu-pwa-v3'") {
		t.Fatal("SW must not reference the old v3 cache as active version")
	}

	scriptBlock := regexp.MustCompile(`dest === 'script' \|\| dest === 'style'([\s\S]*?)return;`).FindString(sw)
	if scriptBlock == "" {
		t.Fatal("SW has no dedicated script/style fetch branch")
	}
	// network-first: fetch attempted BEFORE any cache.match fallback
	// in that branch, and cached response only on network failure.
	netIdx := strings.Index(scriptBlock, "const network = await fetch")
	cacheIdx := strings.Index(scriptBlock, "cache.match(event.request)")
	if netIdx < 0 || cacheIdx < 0 || netIdx > cacheIdx {
		t.Fatal("SW script/style branch must be network-first (fetch before cache fallback)")
	}
	if !strings.Contains(scriptBlock, "catch") {
		t.Fatal("SW script/style branch must fall back to cache only on network failure")
	}

	imageBlock := regexp.MustCompile(`dest === 'image' \|\| dest === 'font'([\s\S]*?)\}\)\(\)`).FindString(sw)
	if imageBlock == "" || !strings.Contains(imageBlock, "cached ||") {
		t.Fatal("SW image/font branch must remain cache-first")
	}
}

// ── Lifecycle: four distinct states ─────────────────────────────────

// LifecycleHelperPresent: the shared lifecycle helper exists and the
// sections use it instead of hand-rolled state markup.
func TestLifecycleHelperPresent(t *testing.T) {
	lc := ownerAsset(t, "lifecycle.js")
	for _, token := range []string{
		"function sectionBox",
		"function sectionStateView",
		"'loading'", "'empty'", "'unavailable'", "'error'",
		"PAYMENT_NOT_CONFIGURED",
		"TASKS_NOT_READY",
		"LOGS_NOT_READY",
	} {
		if !strings.Contains(lc, token) {
			t.Fatalf("lifecycle.js missing %q", token)
		}
	}
	// Retry re-runs the loader through loading again.
	if !strings.Contains(lc, "retryLoader") {
		t.Fatal("lifecycle retry must re-execute the section loader")
	}
}

// SectionsUseSharedLifecycle: section renderers must not hand-roll
// loading/empty/error markup anymore — the shared sectionBox owns it.
func TestSectionsUseSharedLifecycle(t *testing.T) {
	for name, forbidden := range map[string][]string{
		"render-store.js":   {`t('store.loading')`, `t('store.empty')`},
		"render-credits.js": {`t('credits.loading')`, `t('credits.unavailable')`},
		"render-tasks.js":   {`t('tasks.empty')`},
		"render-logs.js":    {`t('logs.empty')`},
	} {
		src := ownerAsset(t, name)
		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Fatalf("%s still hand-rolls state text %s — must go through the shared lifecycle", name, f)
			}
		}
	}
	initSrc := ownerAsset(t, "init.js")
	for _, token := range []string{"runSection", "sectionBox", "mapErrorToSectionState"} {
		if !strings.Contains(initSrc, token) {
			t.Fatalf("init.js missing lifecycle wiring %q", token)
		}
	}
	// Store empty and load error must not share a state: the empty
	// key and the error path are distinct branches in runSection.
	if !strings.Contains(initSrc, "isEmpty") || !strings.Contains(initSrc, "emptyKey") {
		t.Fatal("runSection must decide empty vs ready explicitly")
	}
}

// StructuredErrorsPropagate: loaders must throw structured ApiErrors
// carrying the server code — no string-parsing for state mapping.
func TestStructuredErrorsPropagate(t *testing.T) {
	api := ownerAsset(t, "api.js")
	for _, token := range []string{"function apiErrorFrom", "err.code", "_httpStatus"} {
		if !strings.Contains(api, token) {
			t.Fatalf("api.js missing structured error %q", token)
		}
	}
	// Legacy string-only errors in loaders are gone.
	if strings.Contains(api, "throw new Error(noticeText(") {
		t.Fatal("api.js loaders still flatten server errors into plain strings")
	}
}

// BalanceServerFactContract: balance display continues to come from
// state.account (server fact) verbatim as a decimal string; account
// hydration happens before any section load.
func TestBalanceServerFactContract(t *testing.T) {
	initSrc := ownerAsset(t, "init.js")
	// /api/account must be awaited before showApp/renderPage in the
	// restore flow (balance first paint, independent of Store/Credits).
	acctIdx := strings.Index(initSrc, "await requestJson('/api/account'")
	shellIdx := strings.Index(initSrc, "shellAuthed();\n    shellClearError();")
	if acctIdx < 0 || shellIdx < 0 || acctIdx > shellIdx {
		t.Fatal("restoreSession must load /api/account before revealing the authed shell")
	}
	rs := ownerAsset(t, "render-store.js")
	rc := ownerAsset(t, "render-credits.js")
	for _, src := range []string{rs, rc} {
		if !strings.Contains(src, "state.account.balance") {
			t.Fatal("balance DOM must render state.account.balance (server fact)")
		}
		// Guard actual conversion CALLS on economic facts (comments
		// explaining the contract legitimately mention Number()).
		if strings.Contains(src, "Number(state") || strings.Contains(src, "parseFloat(") {
			t.Fatal("economic facts must stay canonical decimal strings — no Number()/parseFloat()")
		}
	}
}

// ── i18n parity for the new states ──────────────────────────────────

func TestOwnerStateI18nParity(t *testing.T) {
	keys := []string{
		"js.state_loading", "js.state_empty", "js.state_logs_empty",
		"js.state_unavailable", "js.state_error", "js.state_retry",
	}
	for _, loc := range i18n.SupportedLocales() {
		for _, key := range keys {
			if i18n.T(loc, "owner."+key) == "owner."+key {
				t.Fatalf("locale %s missing translation for %s (would render the raw key)", loc, key)
			}
		}
	}
}

// ── Shell lifecycle contract ────────────────────────────────────────

// ShellErrorIsPersistentNotGuest: unknown session/account failures
// must surface a persistent #shellStatus error with Retry — never a
// guest flip and never only a toast.
func TestShellErrorIsPersistentNotGuest(t *testing.T) {
	initSrc := ownerAsset(t, "init.js")
	lc := ownerAsset(t, "lifecycle.js")
	if !strings.Contains(initSrc, "shellError(t('js.owner_session_failed')") ||
		!strings.Contains(initSrc, "shellError(t('js.account_load_failed')") {
		t.Fatal("session/account non-auth failures must render the persistent shell error")
	}
	if !strings.Contains(lc, "#shellStatus") || !strings.Contains(lc, "role=\"alert\"") {
		t.Fatal("shell error host must exist with alert semantics")
	}
	// shellError must NOT flip to guest.
	if regexp.MustCompile(`shellError[\s\S]{0,200}shellGuest`).MatchString(lc + initSrc) {
		t.Fatal("shell error path must not fall back to guest")
	}
	// #shellStatus host exists in the template.
	body := renderOwnerShellForTest(t, "en", "overview")
	if !strings.Contains(body, `id="shellStatus"`) {
		t.Fatal("owner shell template missing #shellStatus host")
	}
}
