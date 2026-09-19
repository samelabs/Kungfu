package server

// C1 closure tests: the historical .notice presentation mechanism is
// fully removed, Owner JS i18n keys stay inside the owner scope, and
// the balance/stat contracts use the single formatter + existing stat
// DOM.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// C1-1: no .notice CSS authority remains.
func TestC1NoticeCSSAuthorityRemoved(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "owner.css"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, sel := range []string{".notice", ".notice.error", ".notice.ok", ".notice.pending", ".overview-notice"} {
		if strings.Contains(s, sel) {
			t.Fatalf("notice CSS authority %q still present", sel)
		}
	}
}

// C1-2: Owner templates contain no historical notice containers.
func TestC1NoticeContainersRemovedFromTemplates(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	for _, path := range []string{"/owner", "/owner/tasks", "/owner/logs", "/owner/credits", "/owner/store", "/owner/key", "/owner/account", "/owner/login"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, `class="notice`) || strings.Contains(body, "overview-notice") {
			t.Fatalf("%s: historical notice container/class still rendered", path)
		}
		for _, nid := range []string{"creditsNotice", "passwordNotice", "resetNotice", "taskCreateNotice", "logsNotice", "overviewNotice", "registerNotice", "loginNotice", "storeNotice"} {
			if strings.Contains(body, `id="`+nid+`"`) {
				t.Fatalf("%s: notice container %s still rendered", path, nid)
			}
		}
	}
}

// C1-3: Owner JS contains no setNotice( — the feedback entry point is
// gone from call sites; toast is invoked directly.
func TestC1OwnerJSHasNoSetNoticeCalls(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "web", "assets", "owner", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatal("owner js files not found")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "setNotice(") {
			t.Fatalf("%s still calls setNotice(", f)
		}
	}
}

// C1-4: Owner JS contains no t('owner.…') out-of-scope keys.
func TestC1OwnerJSI18nScope(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "web", "assets", "owner", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatal("owner js files not found")
	}
	re := regexp.MustCompile(`t\(\s*(['"])owner\.`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.FindAllString(string(src), -1); len(m) > 0 {
			t.Fatalf("%s has %d out-of-scope t('owner.…') call(s): %v", f, len(m), m[:min(len(m), 3)])
		}
	}
}

// C1-5: Store and Credits balance use the existing stat DOM contract
// (numeric value in <b id=...Balance>, label in <span>); no parallel
// stat-value system.
func TestC1BalanceStatDOMContract(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	cases := map[string]string{
		"/owner/store":   "storeBalance",
		"/owner/credits": "creditsBalance",
	}
	for path, id := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		body := rec.Body.String()
		want := `<b id="` + id + `"`
		if !strings.Contains(body, want) {
			t.Fatalf("%s: stat value element %q not rendered as <b>", path, want)
		}
		if strings.Contains(body, "stat-value") || strings.Contains(body, "stat-label") {
			t.Fatalf("%s: parallel stat-value/stat-label system present", path)
		}
	}
}

// C1-6: no unapproved balance format policy — formatCredits/toFixed(4)
// product rule introduced by C1 must not exist; per-page pre-C1
// display behavior is restored (only the <b> stat structure changed).
func TestC1NoBalanceFormatPolicy(t *testing.T) {
	jsDir := filepath.Join("..", "..", "web", "assets", "owner")
	core, err := os.ReadFile(filepath.Join(jsDir, "core.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(core), "formatCredits") {
		t.Fatal("formatCredits authority must not exist")
	}
	for _, f := range []string{"render-overview.js", "render-credits.js", "render-store.js"} {
		src, err := os.ReadFile(filepath.Join(jsDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "formatCredits(") {
			t.Fatalf("%s still uses formatCredits", f)
		}
	}
	// selectTask (read/navigation) must not produce a success toast.
	rt, err := os.ReadFile(filepath.Join(jsDir, "render-tasks.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rt), "t('tasks.created')") {
		t.Fatal("tasks.created key must not be referenced")
	}
}

// C1-6b: no historical .notice containers in DYNAMIC Owner JS markup,
// and no taskModalNotice.
func TestC1NoNoticeInDynamicOwnerJSMarkup(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "web", "assets", "owner", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatal("owner js files not found")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "taskModalNotice") {
			t.Fatalf("%s still references taskModalNotice", f)
		}
		if strings.Contains(string(src), `class="notice`) || strings.Contains(string(src), `class=\'notice`) {
			t.Fatalf("%s still generates class=\"notice\" markup", f)
		}
	}
}

// C1-7: the task modal renders translated text, never raw translation
// keys.
func TestC1TaskModalRendersTranslatedText(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/owner/tasks", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The JS source embedded in the page must not carry owner.-scoped
	// task_new keys (the production i18n regression).
	if strings.Contains(body, "t('owner.task_new") || strings.Contains(body, `t("owner.task_new`) {
		t.Fatal("task modal JS uses out-of-scope owner.task_new keys")
	}
	// The scope's task_new keys must exist in the embedded i18n payload
	// delivered to the client.
	if !strings.Contains(body, `"task_new"`) {
		t.Fatal("task_new i18n scope missing from OWNER_I18N payload")
	}
	for _, raw := range []string{">owner.task_new.heading<", ">OWNER.TASK_NEW.TITLE<"} {
		if strings.Contains(body, raw) {
			t.Fatalf("raw translation key leaked into page: %s", raw)
		}
	}
}

// C1-8: the global toast remains the transient feedback surface.
func TestC1GlobalToastStillWired(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "owner", "core.js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{"function showToast(", "#globalToast"} {
		if !strings.Contains(s, want) {
			t.Fatalf("global toast wiring missing: %s", want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
