package mcpserver

// WO-9a consistency tests: the public texts (llms.txt, kungfu_skill.md,
// task-guide.md, openai.json and the MCP bootstrap instructions) may
// only speak the real registry and the real error catalogue —
// (a) every tool-shaped identifier they mention exists in ToolNames();
// (b) every registered tool is documented in llms.txt;
// (c) every backtick-quoted ALL_CAPS code is in the code→status table,
//     the spec failure reasons, or the next_action/state vocabularies;
// (d) openai.json parses.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	apperrors "kungfu.md/internal/errors"
	"kungfu.md/web"
)

var (
	// toolIdent matches every tool-shaped identifier in prose.
	textToolIdent = regexp.MustCompile(`\b(?:work|task|account|memory)_[a-z_]+\b`)
	// backtickCode matches a backtick-quoted ALL_CAPS_UNDERSCORE token.
	backtickCode = regexp.MustCompile("`([A-Z][A-Z_]+[A-Z])`")
)

// specJSONFieldNames are the JSON keys of the spec's own example
// objects — the §5.3 submission request (code, request_key, payload,
// claim_id, revises), the §7.1 receiver request body (submission_id,
// task_code, agent_ref, payload) and the §8.2 envelope (ok,
// task_code, submission_id, state, paid, reply,
// next_action, retry_after, failure, error).
// task_code among them is tool-shaped but is a field, not a tool, so
// the tool check exempts these keys before judging.
var specJSONFieldNames = map[string]bool{
	"code": true, "request_key": true, "payload": true, "claim_id": true, "revises": true,
	"submission_id": true, "task_code": true, "version": true, "agent_ref": true,
	"ok": true, "state": true, "paid": true, "reply": true,
	"next_action": true, "retry_after": true,
	"failure": true, "error": true,
}

// specFailureReasons are §8.4's failure causes — carried by the failure
// field, not present in the code→status table.
var specFailureReasons = map[string]bool{
	"RECEIVER_UNREACHABLE": true,
	"RECEIVER_FAULT":       true,
	"RECEIVER_PROTOCOL":    true,
	"DELIVERY_UNRESOLVED":  true,
}

// textFixture is one scanned public text.
type textFixture struct {
	name string
	body string
}

// publicTexts loads every scanned surface: the four embedded files plus
// the MCP bootstrap instructions constant.
func publicTexts(t *testing.T) []textFixture {
	t.Helper()
	load := func(name string) string {
		t.Helper()
		b, err := web.StaticFile(name)
		if err != nil {
			t.Fatalf("static file %s: %v", name, err)
		}
		return string(b)
	}
	return []textFixture{
		{"llms.txt", load("llms.txt")},
		{"kungfu_skill.md", load("kungfu_skill.md")},
		{"task-guide.md", load("task-guide.md")},
		{"openai.json", load("openai.json")},
		{"instructions", mcpBootstrapInstructions},
	}
}

// (a) every tool-shaped identifier is a registered tool (the spec's
// own example-object field names are exempt first).
func TestPublicTextsMentionOnlyRealTools(t *testing.T) {
	registry := map[string]bool{}
	for _, name := range ToolNames() {
		registry[name] = true
	}
	for _, f := range publicTexts(t) {
		for _, ident := range textToolIdent.FindAllString(f.body, -1) {
			if specJSONFieldNames[ident] {
				continue // a spec example field, not a tool name
			}
			if !registry[ident] {
				t.Errorf("%s: %q is tool-shaped but not in the registry", f.name, ident)
			}
		}
	}
}

// (b) every registered tool is documented in llms.txt.
func TestLlmsTxtDocumentsEveryTool(t *testing.T) {
	raw, err := web.StaticFile("llms.txt")
	if err != nil {
		t.Fatalf("llms.txt: %v", err)
	}
	body := string(raw)
	// D2: thread tools are registered ahead of their public docs;
	// llms.txt documentation lands in D6 — drop this exemption there.
	d6Pending := map[string]bool{
		"thread_start": true, "thread_key": true, "thread_key_revoke": true,
		"thread_join": true, "thread_leave": true, "thread_remove": true,
		"thread_set_role": true, "thread_close": true, "thread_get": true,
		"thread_list": true, "thread_post": true, "thread_handle": true,
		"thread_retract": true, "assign_take": true, "assign_submit": true,
		"assign_judge": true, "assign_drop": true, "assign_void": true,
		"todo_list": true,
	}
	for _, name := range ToolNames() {
		if d6Pending[name] {
			continue
		}
		if !strings.Contains(body, name) {
			t.Errorf("llms.txt does not document %s", name)
		}
	}
}

// (c) every backtick-quoted ALL_CAPS code is known: in the code→HTTP
// status table, one of the spec failure reasons, or one of the
// next_action / submission-state / task-status vocabularies.
func TestPublicTextsCiteOnlyKnownCodes(t *testing.T) {
	known := map[string]bool{}
	for reason := range specFailureReasons {
		known[reason] = true
	}
	for _, v := range []string{"SUBMIT", "POLL", "DONE", "REVISE", "RETRY", "WAIT", "STOP"} { // next_action values
		known[v] = true
	}
	for _, f := range publicTexts(t) {
		for _, m := range backtickCode.FindAllStringSubmatch(f.body, -1) {
			code := m[1]
			if _, ok := apperrors.StatusFor(code); ok {
				continue
			}
			if !known[code] {
				t.Errorf("%s: cites unknown code %q (not in StatusFor, failure reasons, next_action or states)", f.name, code)
			}
		}
	}
}

// (d) openai.json is valid JSON.
func TestOpenAIManifestIsValidJSON(t *testing.T) {
	raw, err := web.StaticFile("openai.json")
	if err != nil {
		t.Fatalf("openai.json: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("openai.json is not valid JSON: %v", err)
	}
}

// The bootstrap instructions carry no stale protocol text (WO-9a
// deleted the "CODE: message" error shape with the v1 surface).
func TestBootstrapInstructionsDropStaleErrorText(t *testing.T) {
	if strings.Contains(mcpBootstrapInstructions, "CODE: message") {
		t.Fatal("instructions still describe the removed \"CODE: message\" text error shape")
	}
}
