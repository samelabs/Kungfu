package main

// WO-9b unit tests: no database, no platform — every external surface
// (the model API and the kungfu verdict write-back) is an httptest
// server. Verdict bodies are re-checked against the §6.1 rules by a
// local validator (the reference receiver must not import internal/).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// assertRejectingVerdictValid re-checks a 4xx verdict body against the
// §6.1 rules (the local twin of the platform's parser).
func assertRejectingVerdictValid(t *testing.T, body string, declared map[string]bool) verdict {
	t.Helper()
	var v verdict
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("verdict body is not JSON: %v\n%s", err, body)
	}
	if v.Accepted {
		t.Fatalf("verdict is accepting, want a rejection: %s", body)
	}
	if len(v.Criteria) == 0 {
		t.Fatalf("a rejecting verdict must cite at least one criterion: %s", body)
	}
	for _, id := range v.Criteria {
		if !declared[id] {
			t.Fatalf("criterion %q is not declared: %s", id, body)
		}
	}
	if n := utf8.RuneCountInString(v.Reason); n < 1 || n > 500 {
		t.Fatalf("reason length = %d, want 1–500: %s", n, body)
	}
	if !v.Retryable {
		t.Fatalf("the receiver always marks rejections retryable: %s", body)
	}
	if len(v.Annotations) > 50 {
		t.Fatalf("annotations = %d, want ≤ 50", len(v.Annotations))
	}
	for i, a := range v.Annotations {
		if a.Pointer != "" && !strings.HasPrefix(a.Pointer, "/") {
			t.Fatalf("annotations[%d].pointer %q is not an RFC 6901 pointer", i, a.Pointer)
		}
		if !declared[a.Criterion] {
			t.Fatalf("annotations[%d].criterion %q is not declared", i, a.Criterion)
		}
		if n := utf8.RuneCountInString(a.Message); n > 300 {
			t.Fatalf("annotations[%d].message length = %d, want ≤ 300", i, n)
		}
	}
	return v
}

const goodPayload = `{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`

// syncCfg builds a three-rule sync config (required + pattern + schema).
func syncCfg() string {
	return `{
		"mode": "sync",
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

// §5.4 test delivery: acknowledged, never judged.
func TestTestDeliveryNotJudged(t *testing.T) {
	_, syncTS := startReceiver(t, syncCfg(), envConfig{})
	r := deliver(t, syncTS, "test-task01-1", goodPayload, [2]string{"Kungfu-Test", "1"})
	if r.status != http.StatusOK {
		t.Fatalf("sync test delivery = %d %s, want 200", r.status, r.body)
	}
	if strings.Contains(r.body, `"accepted"`) {
		t.Fatalf("test delivery produced a verdict: %s", r.body)
	}

	// async: 202, and no verdict write-back happens
	kungfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Error("test delivery must not produce a verdict write-back")
	}))
	defer kungfu.Close()
	asyncCfg := `{"mode":"async","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url"]}]}`
	_, asyncTS := startReceiver(t, asyncCfg, envConfig{kungfuBaseURL: kungfu.URL, agentKey: "pub-key"})
	r = deliver(t, asyncTS, "test-task01-1", goodPayload, [2]string{"Kungfu-Test", "1"})
	if r.status != http.StatusAccepted {
		t.Fatalf("async test delivery = %d %s, want 202", r.status, r.body)
	}
	if strings.Contains(r.body, `"accepted"`) {
		t.Fatalf("test delivery produced a verdict: %s", r.body)
	}
	time.Sleep(50 * time.Millisecond) // no write-back above within the grace window
}

// required: pass and fail; the failing verdict passes the §6.1 checks
// (own single-rule config so the empty array cannot also trip a schema).
func TestRequiredRule(t *testing.T) {
	cfg := `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url","/bullets"]}]}`
	_, ts := startReceiver(t, cfg, envConfig{})

	r := deliver(t, ts, "req-pass-1", goodPayload)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"accepted":true`) {
		t.Fatalf("required pass = %d %s, want 200 accepted", r.status, r.body)
	}

	empty := `{"url":"https://example.com/a","bullets":[]}`
	r = deliver(t, ts, "req-fail-1", empty)
	if r.status != syncFailStatus {
		t.Fatalf("required fail = %d %s, want %d", r.status, r.body, syncFailStatus)
	}
	v := assertRejectingVerdictValid(t, r.body, map[string]bool{"C1": true})
	if len(v.Criteria) != 1 || v.Criteria[0] != "C1" {
		t.Fatalf("failing criteria = %v, want [C1]: %s", v.Criteria, r.body)
	}
	if !strings.Contains(v.Reason, "/bullets") {
		t.Fatalf("reason does not name the empty pointer: %s", v.Reason)
	}
}

// pattern: pass and fail.
func TestPatternRule(t *testing.T) {
	_, ts := startReceiver(t, syncCfg(), envConfig{})

	bad := `{"url":"ftp://example.com/a","bullets":["s1","s2","s3"]}`
	r := deliver(t, ts, "pat-fail-1", bad)
	if r.status != syncFailStatus {
		t.Fatalf("pattern fail = %d %s", r.status, r.body)
	}
	v := assertRejectingVerdictValid(t, r.body, syncDeclared)
	if len(v.Criteria) != 1 || v.Criteria[0] != "C2" {
		t.Fatalf("failing criteria = %v, want [C2]: %s", v.Criteria, r.body)
	}
	found := false
	for _, a := range v.Annotations {
		if a.Criterion == "C2" && a.Pointer == "/url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no C2 annotation at /url: %s", r.body)
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
	if r.status != syncFailStatus {
		t.Fatalf("schema fail = %d %s", r.status, r.body)
	}
	v := assertRejectingVerdictValid(t, r.body, syncDeclared)
	if len(v.Criteria) != 1 || v.Criteria[0] != "C3" {
		t.Fatalf("failing criteria = %v, want [C3]: %s", v.Criteria, r.body)
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
	if reject.status != syncFailStatus {
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
		t.Fatalf("no Kungfu-Task-Version = %d, want 400", code)
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

// async: 202 immediately, then the verdict reaches
// /api/v1/task_verdict with the publisher Bearer key and the exact
// {submission_id, verdict} body.
func TestAsyncVerdictWriteBack(t *testing.T) {
	type capture struct {
		auth string
		body string
	}
	got := make(chan capture, 1)
	kungfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/task_verdict" {
			t.Errorf("write-back path = %s", req.URL.Path)
		}
		raw, _ := io.ReadAll(req.Body)
		got <- capture{auth: req.Header.Get("Authorization"), body: string(raw)}
		w.WriteHeader(http.StatusOK)
	}))
	defer kungfu.Close()

	cfg := `{"mode":"async","listen":"127.0.0.1:0","criteria":[{"id":"C1","required":["/url"]}]}`
	_, ts := startReceiver(t, cfg, envConfig{kungfuBaseURL: kungfu.URL, agentKey: "publisher-key-1"})

	r := deliver(t, ts, "async-1", goodPayload)
	if r.status != http.StatusAccepted {
		t.Fatalf("async reply = %d %s, want 202", r.status, r.body)
	}

	select {
	case c := <-got:
		if c.auth != "Bearer publisher-key-1" {
			t.Fatalf("Authorization = %q, want the publisher Bearer key", c.auth)
		}
		var body struct {
			SubmissionID string  `json:"submission_id"`
			Verdict      verdict `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatalf("write-back body: %v (%s)", err, c.body)
		}
		if body.SubmissionID != "async-1" || !body.Verdict.Accepted || body.Verdict.Retryable != true {
			t.Fatalf("write-back body = %s", c.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verdict write-back never arrived")
	}
}

// The write-back retries: two 500s then a 200 succeeds on the third
// attempt.
func TestWriteBackRetriesUntilSuccess(t *testing.T) {
	old := verdictRetryBackoff
	verdictRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { verdictRetryBackoff = old })

	var mu sync.Mutex
	hits := 0
	var lastBody string
	kungfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		hits++
		count := hits
		mu.Unlock()
		raw, _ := io.ReadAll(req.Body)
		lastBody = string(raw)
		if count < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer kungfu.Close()

	r := newReceiver(Config{}, envConfig{kungfuBaseURL: kungfu.URL, agentKey: "k"})
	err := r.writeBack("retry-1", verdict{Accepted: true, Retryable: true})
	if err != nil {
		t.Fatalf("writeBack after retries: %v", err)
	}
	if hits != 3 {
		t.Fatalf("hits = %d, want 3", hits)
	}
	if !strings.Contains(lastBody, `"submission_id":"retry-1"`) || !strings.Contains(lastBody, `"accepted":true`) {
		t.Fatalf("write-back body = %s", lastBody)
	}
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
	return `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
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
	if r.status != syncFailStatus {
		t.Fatalf("rubric low score = %d %s, want %d", r.status, r.body, syncFailStatus)
	}
	v := assertRejectingVerdictValid(t, r.body, map[string]bool{"C1": true})
	if len(v.Criteria) != 1 || v.Criteria[0] != "C1" {
		t.Fatalf("failing criteria = %v: %s", v.Criteria, r.body)
	}
	if !strings.Contains(v.Reason, "below threshold") {
		t.Fatalf("reason does not carry the model score: %s", v.Reason)
	}

	junk := chatServer(t, "the payload looks fine, no JSON here")
	defer junk.Close()
	_, ts3 := startReceiver(t, rubricCfg(), envConfig{
		modelBaseURL: junk.URL, modelAPIKey: "model-key-1", modelName: "grader-1",
	})
	r = deliver(t, ts3, "rub-junk-1", goodPayload)
	if r.status != syncFailStatus {
		t.Fatalf("non-JSON model reply = %d %s, want %d", r.status, r.body, syncFailStatus)
	}
	v = assertRejectingVerdictValid(t, r.body, map[string]bool{"C1": true})
	if !strings.Contains(v.Reason, "model reply") {
		t.Fatalf("reason does not explain the parse failure: %s", v.Reason)
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
	if _, err := loadConfig(write(`{"mode":"batch","listen":":9","criteria":[{"id":"C1","required":["/a"]}]}`), envConfig{}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := loadConfig(write(`{"mode":"sync","listen":":9","criteria":[]}`), envConfig{}); err == nil {
		t.Fatal("empty criteria accepted")
	}
	if _, err := loadConfig(write(`{"mode":"sync","listen":":9","criteria":[{"id":"C1"}]}`), envConfig{}); err == nil {
		t.Fatal("criterion without a rule accepted")
	}
	if _, err := loadConfig(write(`{"mode":"async","listen":":9","criteria":[{"id":"C1","required":["/a"]}]}`), envConfig{}); err == nil {
		t.Fatal("async without KUNGFU_* accepted")
	}
}
