package server

// Owner UI production-defect tests: shell + section lifecycle
// (loading/empty/unavailable/error distinct), structured API error
// propagation, balance-first-paint, and the static-asset cache
// invalidation contract (no manual ?v bumps; SW network-first for
// code; server no-cache for JS/CSS).

import (
	"kungfu.md/web"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

// NoOwnerShellManualVersionBust: the Owner shell carries no hand-bumped
// cache-bust query — the only ?v= values allowed are the automatic
// content fingerprints (web.AssetURL).
func TestNoOwnerShellManualVersionBust(t *testing.T) {
	body := renderOwnerShellForTest(t, "en", "overview")
	for _, m := range regexp.MustCompile(`(/assets/[^"?]+)\?v=([^"&]+)`).FindAllStringSubmatch(body, -1) {
		if web.AssetURL(m[1]) != m[1]+"?v="+m[2] {
			t.Fatalf("asset %s carries a non-fingerprint version %q", m[1], m[2])
		}
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
	m := regexp.MustCompile(`const SW_VERSION = 'kungfu-pwa-v(\d+)'`).FindStringSubmatch(sw)
	if m == nil || len(m[1]) == 0 || (len(m[1]) == 1 && m[1] < "4") {
		t.Fatal("SW cache version must be v4 or later to migrate old v3 caches")
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
	// /api/account must be awaited before the authed shell reveal in
	// the restore flow (balance first paint, independent of
	// Store/Credits). Scoped to restoreSession's own body.
	restoreFn := regexp.MustCompile(`async function restoreSession\(\) \{[\s\S]*?\n\}`).FindString(initSrc)
	if restoreFn == "" {
		t.Fatal("restoreSession not found")
	}
	acctIdx := strings.Index(restoreFn, "await requestJson('/api/account'")
	shellIdx := strings.Index(restoreFn, "shellAuthed();")
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

// ── Repair locks ────────────────────────────────────────────────────

// CreditsEmptyIsUnavailable: packages=[] must map to the REAL
// unavailable state (emptyState option), not an empty-state render
// relabeled with unavailable text.
func TestCreditsEmptyMapsToUnavailable(t *testing.T) {
	initSrc := ownerAsset(t, "init.js")
	// The credits runSection call must select unavailable as its
	// success-empty target state.
	start := strings.Index(initSrc, "runSection('owner_credits'")
	if start < 0 {
		t.Fatal("credits runSection call not found")
	}
	credBlock := initSrc[start : start+400]
	if !strings.Contains(credBlock, "emptyState: 'unavailable'") {
		t.Fatalf("credits packages=[] must select emptyState unavailable, got: %s", credBlock)
	}
	if strings.Contains(credBlock, "emptyKey: 'credits.unavailable'") {
		t.Fatal("unavailable must not be faked through an empty-state text key")
	}
	// Store/Tasks/Logs keep plain empty.
	block := func(section string) string {
		i := strings.Index(initSrc, "runSection('"+section+"'")
		if i < 0 {
			return ""
		}
		return initSrc[i : i+400]
	}
	storeBlock := block("store")
	tasksBlock := block("tasks")
	logsBlock := block("logs")
	for name, block := range map[string]string{"store": storeBlock, "tasks": tasksBlock, "logs": logsBlock} {
		if block == "" {
			t.Fatalf("%s runSection call not found", name)
		}
		if strings.Contains(block, "emptyState") {
			t.Fatalf("%s success-empty must stay plain empty", name)
		}
	}
}

// LogsReadsUseSharedLifecycle: every page-level logs read (type
// change, task filter, pagination) goes through the shared
// runSection/sectionBox lifecycle with persistent Retry; no direct
// loadLogs+renderLogs path and no toast-only error path remains.
func TestLogsReadsUseSharedLifecycle(t *testing.T) {
	logsSrc := ownerAsset(t, "logs.js")
	if strings.Contains(logsSrc, "showToast") {
		t.Fatal("logs.js still has a toast-only error path")
	}
	// No handler calls loadLogs()/renderLogs() directly anymore.
	if regexp.MustCompile(`await loadLogs\(\)`).MatchString(logsSrc) {
		t.Fatal("logs.js still awaits loadLogs() directly instead of the shared lifecycle")
	}
	if !strings.Contains(logsSrc, "runSection('logs'") {
		t.Fatal("logs.js must route reads through runSection")
	}
	// All three handlers use logsReload.
	for _, handler := range []string{"bindLogsTypeButtons", "bindLogsTaskFilter", "bindLogsPagination"} {
		block := regexp.MustCompile(regexp.QuoteMeta(handler) + `\(\) \{[\s\S]*?\n\}`).FindString(logsSrc)
		if block == "" {
			t.Fatalf("logs handler %s not found", handler)
		}
		if !strings.Contains(block, "logsReload(") {
			t.Fatalf("logs handler %s must use logsReload (shared lifecycle)", handler)
		}
	}
	// Pagination failure rolls the page back — no new-page+old-data.
	if !strings.Contains(logsSrc, "previous.page") {
		t.Fatal("logs pagination/filter failure must roll state back to the previous page")
	}
}

// ActivateSessionAccountFailureSemantics: explicit login/registration
// account hydration must match restoreSession — guest on auth
// failure, persistent shellError with Retry otherwise, reveal only
// after account success. No toast-only bubbling.
func TestActivateSessionAccountFailureSemantics(t *testing.T) {
	initSrc := ownerAsset(t, "init.js")
	fn := regexp.MustCompile(`async function activateSession\(\) \{[\s\S]*?\n\}`).FindString(initSrc)
	if fn == "" {
		t.Fatal("activateSession not found")
	}
	if !strings.Contains(fn, "isOwnerLoginRequired(error)") {
		t.Fatal("activateSession must map auth failures to guest")
	}
	if !strings.Contains(fn, "shellGuest()") {
		t.Fatal("activateSession auth failure must call shellGuest")
	}
	if !strings.Contains(fn, "shellError(t('js.account_load_failed'), activateSession)") {
		t.Fatal("activateSession non-auth failure must render persistent shellError with itself as Retry")
	}
	if strings.Contains(fn, "showToast") {
		t.Fatal("activateSession must not bubble account failure as a toast")
	}
	// Reveal only after account success: loadAccount before
	// shellAuthed.
	acctIdx := strings.Index(fn, "await loadAccount()")
	authedIdx := strings.Index(fn, "shellAuthed()")
	if acctIdx < 0 || authedIdx < 0 || acctIdx > authedIdx {
		t.Fatal("activateSession must succeed /api/account before revealing the authed shell")
	}
}

// ── Logs rollback control-flow regression ───────────────────────────
//
// The locks below EXECUTE the real lifecycle sources (init.js +
// logs.js + lifecycle.js) under node with instrumented DOM/fetch —
// they verify actual control flow, not string presence.

const logsRollbackHarness = `
// minimal DOM/fetch/i18n shims sufficient for the real sources
const elements = {};
function makeEl(id) {
    return elements[id] || (elements[id] = {
        id, innerHTML: '', value: '', hidden: false, className: '',
        _listeners: {},
        addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
        querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) { return this.innerHTML.includes(sel.replace('.', '')) ? [makeEl(sel)] : []; },
        classList: { toggle() {} }
    });
}
const document = {
    body: makeEl('body'),
    querySelector: (sel) => makeEl(sel),
    querySelectorAll: () => []
};
const qs = (sel) => makeEl(sel);
const qsa = () => [];
// stub binders / helpers from sources not under test (core.js etc.)
function bindAuthHandlers() {}
function bindTaskHandlers() {}
function bindStoreHandlers() {}
function bindCreditsEvents() {}
function isOwnerLoginRequired(error) {
    const code = (error && error.code) || '';
    return code === 'OWNER_LOGIN_REQUIRED' || (error && error.httpStatus === 401);
}
const window = { location: { href: '' }, fetch: null };
function escapeHtml(x) { return String(x); }
function t(key, params) { return key; }
function noticeText(e) { return String((e && e.message) || e); }
function showToast() { throw new Error('showToast must not be used for page-level logs errors'); }
const SECTION = 'logs';
const state = {
    logs: { type: 'task', page: 1, pageSize: 20, taskCode: '', items: [{}], total: 40, totalPages: 2, balance: '0', tasks: [] }
};

// instrumentation
let loadCalls = 0;
let failNext = false;
async function loadLogs() {
    loadCalls++;
    if (failNext) {
        failNext = false;
        throw apiError('INTERNAL', 'boom', 500);
    }
    state.logs.items = [{}];
}
function renderLogs() { renderCalls++; }
let renderCalls = 0;
`

// execLifecycleSources loads the real web assets into a sandbox and
// returns the harness globals for assertions.
func execLifecycleSources(t *testing.T, extra string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "..", "..")
	script := logsRollbackHarness +
		"// real sources under test\n" +
		readFileOrFatal(t, filepath.Join(root, "web", "assets", "owner", "lifecycle.js")) + "\n" +
		readFileOrFatal(t, filepath.Join(root, "web", "assets", "owner", "init.js")) + "\n" +
		readFileOrFatal(t, filepath.Join(root, "web", "assets", "owner", "logs.js")) + "\n" +
		extra + "\n"
	out, err := execNode(t, script)
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	return out
}

func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// neutralize the top-level restoreSession()/decorateRenderPage()/
	// bindOwnerPage() boot calls by renaming them; tests call them
	// explicitly.
	return string(data)
}

func execNode(t *testing.T, script string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "harness.mjs")
	// The real sources auto-boot (restoreSession etc.) which needs
	// fetch; provide a stub via a prelude executed as CommonJS.
	full := "const __origFetch = globalThis.fetch;\n" +
		"globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => JSON.stringify({success:true,data:{}}) });\n" +
		script
	if err := os.WriteFile(file, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", file)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// LogsRollbackOnLoadFailure: with the real sources, a failing
// loadLogs through logsReload must (a) leave the persistent error
// state rendered, (b) roll page/type/taskCode back to previous, and
// (c) Retry must re-read the ROLLED-BACK state.
func TestLogsRollbackOnLoadFailure(t *testing.T) {
	out := execLifecycleSources(t, `
;(async () => {
    const prev = {type: 'task', page: 1, taskCode: ''};
    state.logs.page = 2; // user clicked "next"
    failNext = true;
    const outcome = await logsReload(prev);
    console.log('OUTCOME=' + JSON.stringify(outcome));
    console.log('PAGE_AFTER=' + state.logs.page);
    console.log('BOX_STATE=' + (qs('#logsTableWrap').innerHTML.includes('state-error') ? 'error' : 'other'));
    // Retry: fetch restored (failNext already consumed), reads page 1
    failNext = false;
    loadCalls = 0;
    await logsReload(prev);
    console.log('RETRY_LOADS=' + loadCalls);
    console.log('PAGE_FINAL=' + state.logs.page);
})().catch(e => { console.error('HARNESS_FAIL', e); process.exit(1); });
`)
	for _, want := range []string{
		"OUTCOME={\"ok\":false}",
		"PAGE_AFTER=1",    // rolled back from 2
		"BOX_STATE=error", // persistent error rendered
		"RETRY_LOADS=1",   // retry re-executed the loader
		"PAGE_FINAL=1",    // retry read the restored state
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in harness output, got:\n%s", want, out)
		}
	}
}

// LogsTaskFilterSelectionConsistency: after a filter-change load
// failure the DOM selection is restored together with state.
func TestLogsTaskFilterSelectionConsistency(t *testing.T) {
	out := execLifecycleSources(t, `
;(async () => {
    const prev = {type: 'task', page: 1, taskCode: ''};
    state.logs.taskCode = 'TASK-X'; // user picked a filter
    qs('#logTaskFilter').value = 'TASK-X';
    failNext = true;
    await logsReload(prev);
    console.log('STATE_CODE=' + (state.logs.taskCode === '' ? 'restored' : 'stale'));
    console.log('DOM_CODE=' + (qs('#logTaskFilter').value === '' ? 'restored' : 'stale'));
    console.log('CONSISTENT=' + (state.logs.taskCode === qs('#logTaskFilter').value));
})().catch(e => { console.error('HARNESS_FAIL', e); process.exit(1); });
`)
	for _, want := range []string{
		"STATE_CODE=restored",
		"DOM_CODE=restored",
		"CONSISTENT=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in harness output, got:\n%s", want, out)
		}
	}
}

// RunSectionOutcomeContract: runSection resolves (never rejects) with
// an explicit {ok} outcome on both paths — this is the mechanism the
// rollback branch depends on.
func TestRunSectionOutcomeContract(t *testing.T) {
	out := execLifecycleSources(t, `
;(async () => {
    let okPath = null, failPath = null;
    okPath = await runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {isEmpty: () => !state.logs.items.length, emptyKey: 'js.state_logs_empty'});
    failNext = true;
    failPath = await runSection('logs', loadLogs, '#logsTableWrap', () => renderLogs(), {isEmpty: () => !state.logs.items.length, emptyKey: 'js.state_logs_empty'});
    console.log('OK_PATH=' + JSON.stringify(okPath));
    console.log('FAIL_PATH=' + JSON.stringify(failPath));
})().catch(e => { console.error('REJECTED', e); process.exit(1); });
`)
	for _, want := range []string{
		"OK_PATH={\"ok\":true}",
		"FAIL_PATH={\"ok\":false}",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in harness output, got:\n%s", want, out)
		}
	}
}
