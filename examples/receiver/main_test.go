package main

// WO-9b unit tests: no database, no platform — the one external
// surface (the model API) is an httptest server. Rejection bodies are
// re-checked by a local validator: the platform hands them to the
// executor verbatim, so they must stay readable and within the first
// 4 000 bytes the platform records.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// startReceiver loads a config string as receiver.json, starts the
// handler on an httptest server and returns both.
func startReceiver(t *testing.T, cfgJSON string, env envConfig) (*receiver, *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receiver.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	r, err := loadConfig(path, env)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	ts := httptest.NewServer(r.handler())
	t.Cleanup(ts.Close)
	return r, ts
}

type result struct {
	status int
	body   string
}

// deliver posts one §7.1 delivery; extra headers append/override.
func deliver(t *testing.T, ts *httptest.Server, submissionID string, payload string, extra ...[2]string) result {
	t.Helper()
	body := fmt.Sprintf(`{"submission_id":%q,"task_code":"task01","version":1,"agent_ref":"ref","payload":%s}`,
		submissionID, payload)
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", submissionID)
	req.Header.Set("Kungfu-Task", "task01")
	req.Header.Set("Kungfu-Task-Version", "1")
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return result{status: resp.StatusCode, body: string(raw)}
}

// assertRejection checks a rejection body: JSON, not accepted, a
// 1–500-rune message, ≤ 20 problems each citing a configured criterion
// with an RFC 6901 pointer (or "") and a ≤ 200-rune message, and the
// whole body within the 4 000 bytes the platform records.
func assertRejection(t *testing.T, body string, declared map[string]bool) reply {
	t.Helper()
	var v reply
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("rejection body is not JSON: %v\n%s", err, body)
	}
	if v.Accepted {
		t.Fatalf("reply is accepting, want a rejection: %s", body)
	}
	if n := utf8.RuneCountInString(v.Message); n < 1 || n > 500 {
		t.Fatalf("message length = %d, want 1–500: %s", n, body)
	}
	if len(v.Problems) > 20 {
		t.Fatalf("problems = %d, want ≤ 20", len(v.Problems))
	}
	for i, p := range v.Problems {
		if p.Pointer != "" && !strings.HasPrefix(p.Pointer, "/") {
			t.Fatalf("problems[%d].pointer %q is not an RFC 6901 pointer", i, p.Pointer)
		}
		if !declared[p.Criterion] {
			t.Fatalf("problems[%d].criterion %q is not configured", i, p.Criterion)
		}
		if n := utf8.RuneCountInString(p.Message); n > 200 {
			t.Fatalf("problems[%d].message length = %d, want ≤ 200", i, n)
		}
	}
	if len(body) > 4000 {
		t.Fatalf("rejection body = %d bytes, want ≤ 4000", len(body))
	}
	return v
}

// failedCriteria lists the criteria a rejection's problems cite.
func failedCriteria(v reply) []string {
	out := make([]string, 0, len(v.Problems))
	for _, p := range v.Problems {
		out = append(out, p.Criterion)
	}
	return out
}

const goodPayload = `{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`

// syncCfg builds a three-rule config (required + pattern + schema).
func syncCfg() string {
	return `{
		"listen": "127.0.0.1:0",
		"criteria": [
			{"id": "C1", "required": ["/url", "/bullets"]},
			{"id": "C2", "pattern": {"pointer": "/url", "regex": "^https://[^\\s]+$"}},
			{"id": "C3", "schema": {
				"type": "object",
				"properties": {
					"url": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
				},
				"required": ["url", "bullets"]
			}}
		]
	}`
}

var syncDeclared = map[string]bool{"C1": true, "C2": true, "C3": true}

// §4 test delivery: a fixed {} that checks reachability and liveness —
// NOT the content. A payload that would fail judging still gets 2xx,
// and the key is never cached (a later real delivery decides anew).
func TestRequiredRule(t *testing.T) {
	cfg := `{"listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url","/bullets"]}]}`
	_, ts := startReceiver(t, cfg, envConfig{})

	r := deliver(t, ts, "req-pass-1", goodPayload)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"accepted":true`) {
		t.Fatalf("required pass = %d %s, want 200 accepted", r.status, r.body)
	}

	empty := `{"url":"https://example.com/a","bullets":[]}`
	r = deliver(t, ts, "req-fail-1", empty)
	if r.status != failStatus {
		t.Fatalf("required fail = %d %s, want %d", r.status, r.body, failStatus)
	}
	v := assertRejection(t, r.body, map[string]bool{"C1": true})
	if fc := failedCriteria(v); len(fc) != 1 || fc[0] != "C1" {
		t.Fatalf("failing criteria = %v, want [C1]: %s", failedCriteria(v), r.body)
	}
	if !strings.Contains(v.Message, "/bullets") {
		t.Fatalf("message does not name the empty pointer: %s", v.Message)
	}
}

// pattern: pass and fail.
func TestPatternRule(t *testing.T) {
	_, ts := startReceiver(t, syncCfg(), envConfig{})

	bad := `{"url":"ftp://example.com/a","bullets":["s1","s2","s3"]}`
	r := deliver(t, ts, "pat-fail-1", bad)
	if r.status != failStatus {
		t.Fatalf("pattern fail = %d %s", r.status, r.body)
	}
	v := assertRejection(t, r.body, syncDeclared)
	if fc := failedCriteria(v); len(fc) != 1 || fc[0] != "C2" {
		t.Fatalf("failing criteria = %v, want [C2]: %s", failedCriteria(v), r.body)
	}
	found := false
	for _, a := range v.Problems {
		if a.Criterion == "C2" && a.Pointer == "/url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no C2 problem at /url: %s", r.body)
	}

	r = deliver(t, ts, "pat-pass-1", goodPayload)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"accepted":true`) {
		t.Fatalf("pattern pass = %d %s", r.status, r.body)
	}
}

// schema: pass and fail.
func TestSchemaRule(t *testing.T) {
	_, ts := startReceiver(t, syncCfg(), envConfig{})

	tooFew := `{"url":"https://example.com/a","bullets":["only one"]}`
	r := deliver(t, ts, "sch-fail-1", tooFew)
	if r.status != failStatus {
		t.Fatalf("schema fail = %d %s", r.status, r.body)
	}
	v := assertRejection(t, r.body, syncDeclared)
	if fc := failedCriteria(v); len(fc) != 1 || fc[0] != "C3" {
		t.Fatalf("failing criteria = %v, want [C3]: %s", failedCriteria(v), r.body)
	}

	r = deliver(t, ts, "sch-pass-1", goodPayload)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"accepted":true`) {
		t.Fatalf("schema pass = %d %s", r.status, r.body)
	}
}

// §7.1: the same Idempotency-Key returns the FIRST outcome unchanged —
// even with a different payload.
func TestIdempotencySameKeyReturnsFirstOutcome(t *testing.T) {
	_, ts := startReceiver(t, syncCfg(), envConfig{})

	first := deliver(t, ts, "idem-1", goodPayload)
	if first.status != http.StatusOK {
		t.Fatalf("first = %d %s", first.status, first.body)
	}
	again := deliver(t, ts, "idem-1", `{"url":"ftp://x","bullets":[]}`)
	if again.status != first.status || again.body != first.body {
		t.Fatalf("repeat differs: %d/%q vs %d/%q", again.status, again.body, first.status, first.body)
	}

	reject := deliver(t, ts, "idem-2", `{"url":"ftp://x","bullets":[]}`)
	if reject.status != failStatus {
		t.Fatalf("first reject = %d", reject.status)
	}
	repeatReject := deliver(t, ts, "idem-2", goodPayload)
	if repeatReject.status != reject.status || repeatReject.body != reject.body {
		t.Fatalf("repeat of a rejection differs: %d/%q vs %d/%q",
			repeatReject.status, repeatReject.body, reject.status, reject.body)
	}
}

// Missing required §7.1 headers (or a key mismatch) → 400.
func TestMissingRequiredHeaders400(t *testing.T) {
	_, ts := startReceiver(t, syncCfg(), envConfig{})

	post := func(hdrs map[string]string) int {
		t.Helper()
		body := `{"submission_id":"hdr-1","task_code":"task01","version":1,"agent_ref":"r","payload":{}}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	full := map[string]string{"Idempotency-Key": "hdr-1", "Kungfu-Task": "task01", "Kungfu-Task-Version": "1"}

	noKey := cloneHeaders(full)
	delete(noKey, "Idempotency-Key")
	if code := post(noKey); code != http.StatusBadRequest {
		t.Fatalf("no Idempotency-Key = %d, want 400", code)
	}
	noTask := cloneHeaders(full)
	delete(noTask, "Kungfu-Task")
	if code := post(noTask); code != http.StatusBadRequest {
		t.Fatalf("no Kungfu-Task = %d, want 400", code)
	}
	noVersion := cloneHeaders(full)
	delete(noVersion, "Kungfu-Task-Version")
	if code := post(noVersion); code != http.StatusBadRequest {
		t.Fatalf("no extra header = %d, want 400", code)
	}
	mismatch := cloneHeaders(full)
	mismatch["Idempotency-Key"] = "other"
	if code := post(mismatch); code != http.StatusBadRequest {
		t.Fatalf("key mismatch = %d, want 400", code)
	}
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// chatServer serves one fixed OpenAI-compatible completion reply.
func chatServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
			t.Errorf("model path = %s", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer model-key-1" {
			t.Errorf("model Authorization = %q", req.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
}

func rubricCfg() string {
	return `{"listen":"127.0.0.1:0","criteria":[
		{"id":"C1","rubric":{"text":"Every bullet must be one complete factual sentence.","threshold":0.7}}]}`
}

// rubric: score above threshold accepts; below rejects with the model
// reason; a non-JSON model reply fails the criterion.
func TestRubricScoring(t *testing.T) {
	high := chatServer(t, `{"score": 0.9, "reason": "all bullets are complete sentences"}`)
	defer high.Close()
	_, ts := startReceiver(t, rubricCfg(), envConfig{
		modelBaseURL: high.URL, modelAPIKey: "model-key-1", modelName: "grader-1",
	})
	r := deliver(t, ts, "rub-high-1", goodPayload)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"accepted":true`) {
		t.Fatalf("rubric high score = %d %s, want 200 accepted", r.status, r.body)
	}

	low := chatServer(t, `{"score": 0.3, "reason": "second bullet is a fragment"}`)
	defer low.Close()
	_, ts2 := startReceiver(t, rubricCfg(), envConfig{
		modelBaseURL: low.URL, modelAPIKey: "model-key-1", modelName: "grader-1",
	})
	r = deliver(t, ts2, "rub-low-1", goodPayload)
	if r.status != failStatus {
		t.Fatalf("rubric low score = %d %s, want %d", r.status, r.body, failStatus)
	}
	v := assertRejection(t, r.body, map[string]bool{"C1": true})
	if fc := failedCriteria(v); len(fc) != 1 || fc[0] != "C1" {
		t.Fatalf("failing criteria = %v: %s", failedCriteria(v), r.body)
	}
	if !strings.Contains(v.Message, "below threshold") {
		t.Fatalf("message does not carry the model score: %s", v.Message)
	}

	junk := chatServer(t, "the payload looks fine, no JSON here")
	defer junk.Close()
	_, ts3 := startReceiver(t, rubricCfg(), envConfig{
		modelBaseURL: junk.URL, modelAPIKey: "model-key-1", modelName: "grader-1",
	})
	r = deliver(t, ts3, "rub-junk-1", goodPayload)
	if r.status != failStatus {
		t.Fatalf("non-JSON model reply = %d %s, want %d", r.status, r.body, failStatus)
	}
	v = assertRejection(t, r.body, map[string]bool{"C1": true})
	if !strings.Contains(v.Message, "model reply") {
		t.Fatalf("message does not explain the parse failure: %s", v.Message)
	}
}

// A rubric without MODEL_* configured is a startup error.
func TestConfigRubricWithoutModelFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receiver.json")
	if err := os.WriteFile(path, []byte(rubricCfg()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, envConfig{}); err == nil || !strings.Contains(err.Error(), "MODEL_") {
		t.Fatalf("rubric without MODEL_* = %v, want a MODEL_ startup error", err)
	}
}

// Extra startup guards exercised on the way (same validation path).
func TestConfigValidationBasics(t *testing.T) {
	write := func(cfg string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "receiver.json")
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := loadConfig(write(`{"criteria":[{"id":"C1","required":["/a"]}]}`), envConfig{}); err == nil {
		t.Fatal("missing listen accepted")
	}
	if _, err := loadConfig(write(`{"listen":":9","criteria":[]}`), envConfig{}); err == nil {
		t.Fatal("empty criteria accepted")
	}
	if _, err := loadConfig(write(`{"listen":":9","criteria":[{"id":"C1"}]}`), envConfig{}); err == nil {
		t.Fatal("criterion without a rule accepted")
	}
}
