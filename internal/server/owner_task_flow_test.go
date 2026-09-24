package server

// Task publishing flow alignment regression.
// Locks:
//   - /owner/tasks/new reuses the canonical task UI (no second form)
//   - JS action matrix: pending=Edit+Open, open=Close+AddBudget (no
//     Edit), closed=Edit+Open+Refund eligibility
//   - five-locale Guide critical facts (Idempotency-Key, skill
//     optional, no fixed ?v=)
//   - Markdown guide matches the HTML critical contract
//   - Guide CTA points at the canonical create flow
//   - Terms/Privacy routes + footer links
//
// JS behavior is verified by EXECUTING the real render-tasks.js
// under node with a DOM stub (not string greps).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func flowRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "..", "..")
}

func flowAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(flowRepoRoot(t), "web", "assets", "owner", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func flowGuide(t *testing.T, loc string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(flowRepoRoot(t), "web", "task_guide_"+loc+".html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestTaskNewReusesCanonicalUI: the task_new section renders the SAME
// tasks UI and no second create form exists anywhere.
func TestTaskNewReusesCanonicalUI(t *testing.T) {
	tplSrc, err := os.ReadFile(filepath.Join(flowRepoRoot(t), "internal", "server", "templates.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(tplSrc)
	if strings.Contains(src, "ownerTaskNewHTML") {
		t.Fatal("second task create form implementation (ownerTaskNewHTML) still exists")
	}
	if !strings.Contains(src, `case "tasks", "task_new":`) {
		t.Fatal("task_new must share the canonical tasks section renderer")
	}
	if strings.Contains(src, "task-create-form") {
		t.Fatal("second create form CSS class remains")
	}
	// init.js routes task_new through the same section loader and
	// opens the canonical modal.
	initSrc := flowAsset(t, "init.js")
	if !strings.Contains(initSrc, `SECTION === 'task_new'`) {
		t.Fatal("task_new must flow through the shared tasks section")
	}
	if !strings.Contains(initSrc, `openTaskModal('create')`) {
		t.Fatal("task_new must auto-open the canonical create modal")
	}
	// The only create form markup lives in the modal renderer.
	renderSrc := flowAsset(t, "render-tasks.js")
	if c := strings.Count(renderSrc, `id="taskForm"`); c != 1 {
		t.Fatalf("exactly one create form implementation expected, got %d", c)
	}
}

// TestTaskActionMatrixJS: EXECUTES render-tasks.js renderTaskDetail
// under node for pending/open/closed tasks and asserts the action
// bar buttons — real behavior, not source greps.
func TestTaskActionMatrixJS(t *testing.T) {
	script := `
// DOM stub sufficient for renderTaskDetail
const els = {};
function makeEl() {
    return {
        innerHTML: '', _listeners: {},
        addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
        classList: { add() {}, remove() {}, toggle() {} },
        dataset: {},
        value: '', hidden: false, disabled: false,
        querySelector() { return null; }, querySelectorAll() { return []; }
    };
}
const document = { querySelector: () => makeEl(), querySelectorAll: () => [] };
let qs = () => makeEl();
const qsa = () => [];
function escapeHtml(x) { return String(x); }
function t(key) { return key; }
function noticeText(e) { return String(e); }
function showToast() {}
async function requestJson() { return {success: true, data: {}}; }
const state = {tasks: [], selectedTask: null};
function humanTaskStatus(s) { return s; }
` + flowAsset(t, "render-tasks.js") + `
// capture the rendered action bar per status
const out = {};
for (const status of ['pending', 'open', 'closed']) {
    const task = {code: 'X', title: 'T', status, budget: '1000', price: '5', success_count: 0,
                  requirements: 'r', postapi: 'https://e.com/h', closed_at: '2020-01-01T00:00:00Z'};
    const host = makeEl();
    const origQS = qs;
    // renderTaskDetail writes into qs('#taskDetail')
    let captured = null;
    qs = (sel) => { const el = makeEl(); if (sel === '#taskDetail') captured = el; return el; };
    renderTaskDetail(task);
    qs = origQS;
    out[status] = (captured && captured.innerHTML) || '';
}
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	file := filepath.Join(dir, "matrix.mjs")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", file).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	var matrix map[string]string
	if err := json.Unmarshal(out, &matrix); err != nil {
		t.Fatalf("bad harness output: %v\n%s", err, out)
	}
	check := func(status string, want, notWant []string) {
		t.Helper()
		htmlOut := matrix[status]
		if htmlOut == "" {
			t.Fatalf("no captured HTML for status %s", status)
		}
		for _, w := range want {
			if !strings.Contains(htmlOut, `data-act="`+w+`"`) {
				t.Fatalf("%s must offer action %q", status, w)
			}
		}
		for _, n := range notWant {
			if strings.Contains(htmlOut, `data-act="`+n+`"`) {
				t.Fatalf("%s must NOT offer action %q", status, n)
			}
		}
	}
	check("pending", []string{"edit", "open"}, nil)
	check("open", []string{"close", "budget"}, []string{"edit"})
	check("closed", []string{"edit", "open", "refund"}, nil)
}

// TestGuideCriticalFactsFiveLocales: every locale guide carries the
// real test contract and does not present skill as required.
func TestGuideCriticalFactsFiveLocales(t *testing.T) {
	for _, loc := range []string{"en", "zh", "ja", "ko", "es"} {
		s := flowGuide(t, loc)
		if !strings.Contains(s, "Idempotency-Key") {
			t.Fatalf("%s guide missing Idempotency-Key contract", loc)
		}
		if !strings.Contains(s, "POST /api/testtask/{code}") {
			t.Fatalf("%s guide missing testtask endpoint", loc)
		}
		if !strings.Contains(s, "earn_task") {
			t.Fatalf("%s guide missing no-earn_task semantics", loc)
		}
		// skill must be marked optional
		opt := regexp.MustCompile(`(?i)optional|可选|opcional|推奨項目|권장`).FindString(s)
		if opt == "" {
			t.Fatalf("%s guide does not mark skill as optional", loc)
		}
		// no fixed asset version
		if regexp.MustCompile(`site\.css\?v=\d+`).MatchString(s) {
			t.Fatalf("%s guide still pins site.css ?v=", loc)
		}
		// shared assets referenced
		if !strings.Contains(s, "/assets/task-guide.css") {
			t.Fatalf("%s guide missing shared task-guide.css", loc)
		}
		// CTA into the canonical create flow
		if !strings.Contains(s, "/owner/tasks/new") {
			t.Fatalf("%s guide CTA must point at the canonical create flow", loc)
		}
		// no inline style blocks remain
		if strings.Contains(s, "<style>") {
			t.Fatalf("%s guide still has inline styles", loc)
		}
		// footer legal links
		if !strings.Contains(s, "/terms?lang="+loc) || !strings.Contains(s, "/privacy?lang="+loc) {
			t.Fatalf("%s guide missing footer legal links", loc)
		}
	}
}

// TestGuideMarkdownMatchesHTMLContract: the markdown guide carries
// the same critical contract as the HTML guides.
func TestGuideMarkdownMatchesHTMLContract(t *testing.T) {
	md, err := os.ReadFile(filepath.Join(flowRepoRoot(t), "web", "owner_task_guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(md)
	for _, want := range []string{
		"Idempotency-Key",
		"POST /api/testtask/{code}",
		"earn_task",
		"1-128 ASCII",
		"does not open the task",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("markdown guide missing critical fact: %q", want)
		}
	}
	if regexp.MustCompile(`prepare three things`).MatchString(s) {
		t.Fatal("markdown guide still claims three mandatory things including skill")
	}
	if !regexp.MustCompile(`(?i)recommended, not required`).MatchString(s) {
		t.Fatal("markdown guide must present skill as optional")
	}
}

// TestLegalRoutesAndServe: /terms and /privacy render with content
// and footer links in every locale, without untranslated keys.
func TestLegalRoutesAndServe(t *testing.T) {
	router := storeTestServer(t).buildRouter()
	for _, path := range []string{"/terms", "/privacy"} {
		for _, loc := range []string{"en", "zh", "ja", "ko", "es"} {
			req := httptest.NewRequest(http.MethodGet, path+"?lang="+loc, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s?lang=%s: status %d", path, loc, rec.Code)
			}
			body := rec.Body.String()
			if len(body) < 500 {
				t.Fatalf("%s?lang=%s: suspiciously short body", path, loc)
			}
			if strings.Contains(body, "terms.s0_h") || strings.Contains(body, "privacy.s0_h") {
				t.Fatalf("%s?lang=%s: untranslated i18n key leaked", path, loc)
			}
			if !strings.Contains(body, "/terms") || !strings.Contains(body, "/privacy") {
				t.Fatalf("%s?lang=%s: footer legal links missing", path, loc)
			}
		}
	}
}
