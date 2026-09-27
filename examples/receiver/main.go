// Command receiver is the Kungfu reference acceptance receiver
// (WO-9b): a publisher-copyable judging service implementing the
// receiver protocol of docs/task-spec-1.0.md §7 and producing §6.1
// verdicts. It depends on nothing from internal/ — the platform is
// reached only over HTTP.
//
// Modes:
//
//	sync  — judge inside the delivery call: pass → HTTP 200, fail →
//	        HTTP 422 + the rejecting verdict body (§7.2).
//	async — answer HTTP 202 immediately, judge in the background and
//	        write the verdict back through
//	        POST {KUNGFU_BASE_URL}/api/v1/task_verdict.
//
// Rules per criterion (receiver.json): required (JSON pointers that
// must exist and be non-empty), pattern (pointer + regex), schema (a
// JSON Schema for the whole payload) and rubric (model scoring with a
// threshold; needs MODEL_BASE_URL / MODEL_API_KEY / MODEL_NAME).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// -- configuration ------------------------------------------------------

// PatternRule matches the string at Pointer against Regex.
type PatternRule struct {
	Pointer string `json:"pointer"`
	Regex   string `json:"regex"`

	re *regexp.Regexp
}

// Rubric is the model-scoring rule: Text is the grading instruction,
// Threshold (0–1) the acceptance cut-off.
type Rubric struct {
	Text      string  `json:"text"`
	Threshold float64 `json:"threshold"`
}

// Criterion is one judging rule; at least one rule kind must be set.
type Criterion struct {
	ID       string          `json:"id"`
	Required []string        `json:"required,omitempty"`
	Pattern  *PatternRule    `json:"pattern,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Rubric   *Rubric         `json:"rubric,omitempty"`

	compiled *jsonschema.Schema
}

// Config is receiver.json.
type Config struct {
	Mode     string      `json:"mode"`   // "sync" | "async"
	Listen   string      `json:"listen"` // e.g. ":8080"
	Criteria []Criterion `json:"criteria"`
}

// Environment for the model API and the platform write-back.
type envConfig struct {
	kungfuBaseURL string // KUNGFU_BASE_URL
	agentKey      string // KUNGFU_AGENT_KEY
	modelBaseURL  string // MODEL_BASE_URL
	modelAPIKey   string // MODEL_API_KEY
	modelName     string // MODEL_NAME
}

func envFromOS() envConfig {
	return envConfig{
		kungfuBaseURL: os.Getenv("KUNGFU_BASE_URL"),
		agentKey:      os.Getenv("KUNGFU_AGENT_KEY"),
		modelBaseURL:  os.Getenv("MODEL_BASE_URL"),
		modelAPIKey:   os.Getenv("MODEL_API_KEY"),
		modelName:     os.Getenv("MODEL_NAME"),
	}
}

// loadConfig reads, validates and compiles receiver.json. A criterion
// using a rubric without MODEL_* configured, an async receiver without
// the platform write-back credentials, or any malformed rule is a
// startup error; a sync receiver using a rubric only gets a warning
// (model latency risks the §11 10-second response budget).
func loadConfig(path string, env envConfig) (*receiver, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Mode != "sync" && cfg.Mode != "async" {
		return nil, fmt.Errorf("mode must be \"sync\" or \"async\", got %q", cfg.Mode)
	}
	if cfg.Listen == "" {
		return nil, errors.New("listen is required")
	}
	if len(cfg.Criteria) == 0 {
		return nil, errors.New("at least one criterion is required")
	}
	hasRubric := false
	seen := map[string]bool{}
	for i := range cfg.Criteria {
		c := &cfg.Criteria[i]
		if c.ID == "" {
			return nil, fmt.Errorf("criteria[%d].id is required", i)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("criteria[%d].id %q is duplicated", i, c.ID)
		}
		seen[c.ID] = true
		rules := 0
		if len(c.Required) > 0 {
			rules++
		}
		if c.Pattern != nil {
			rules++
			re, err := regexp.Compile(c.Pattern.Regex)
			if err != nil {
				return nil, fmt.Errorf("criteria %s pattern.regex: %w", c.ID, err)
			}
			c.Pattern.re = re
		}
		if len(c.Schema) > 0 {
			rules++
			sch, err := compileSchema(c.Schema)
			if err != nil {
				return nil, fmt.Errorf("criteria %s schema: %w", c.ID, err)
			}
			c.compiled = sch
		}
		if c.Rubric != nil {
			rules++
			hasRubric = true
			if c.Rubric.Threshold < 0 || c.Rubric.Threshold > 1 {
				return nil, fmt.Errorf("criteria %s rubric.threshold must be within 0–1", c.ID)
			}
			if c.Rubric.Text == "" {
				return nil, fmt.Errorf("criteria %s rubric.text is required", c.ID)
			}
		}
		if rules == 0 {
			return nil, fmt.Errorf("criteria %s has no rule (one of required, pattern, schema, rubric)", c.ID)
		}
	}
	if hasRubric && (env.modelBaseURL == "" || env.modelAPIKey == "" || env.modelName == "") {
		return nil, errors.New("a rubric criterion requires MODEL_BASE_URL, MODEL_API_KEY and MODEL_NAME")
	}
	if hasRubric && cfg.Mode == "sync" {
		fmt.Fprintln(os.Stderr, "warning: mode sync with a rubric — model latency may exceed the 10-second response budget; consider async")
	}
	if cfg.Mode == "async" {
		if env.kungfuBaseURL == "" {
			return nil, errors.New("mode async requires KUNGFU_BASE_URL")
		}
		if env.agentKey == "" {
			return nil, errors.New("mode async requires KUNGFU_AGENT_KEY")
		}
	}
	return newReceiver(cfg, env), nil
}

// compileSchema compiles a JSON Schema under draft 2020-12.
func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("criterion:schema", doc); err != nil {
		return nil, err
	}
	return compiler.Compile("criterion:schema")
}

// -- verdict (§6.1 shape; the platform sets source, never the body) ----

type annotation struct {
	Pointer   string `json:"pointer"`
	Criterion string `json:"criterion"`
	Message   string `json:"message"`
}

type verdict struct {
	Accepted    bool         `json:"accepted"`
	Criteria    []string     `json:"criteria,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Retryable   bool         `json:"retryable"`
	Annotations []annotation `json:"annotations,omitempty"`
}

// §6.1 bounds enforced while building the verdict.
const (
	maxReasonRunes      = 500
	maxAnnotations      = 50
	maxAnnotationRunes  = 300
	syncFailStatus      = http.StatusUnprocessableEntity
	requestBodyLimit    = 2 << 20 // 512 KB payload cap + envelope headroom
	modelCallTimeout    = 8 * time.Second
	platformCallTimeout = 10 * time.Second
)

// -- receiver -----------------------------------------------------------

type cachedResult struct {
	status int
	body   []byte
}

type receiver struct {
	cfg Config
	env envConfig

	mu      sync.Mutex
	results map[string]cachedResult // Idempotency-Key -> first outcome

	client *http.Client // shared, non-judging calls
}

// verdictRetryBackoff spaces the task_verdict write-back retries after
// a failed attempt (package var so tests can shorten it).
var verdictRetryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

func newReceiver(cfg Config, env envConfig) *receiver {
	return &receiver{
		cfg:    cfg,
		env:    env,
		client: &http.Client{Timeout: platformCallTimeout},
	}
}

// deliveryRequest is the §7.1 body.
type deliveryRequest struct {
	SubmissionID string          `json:"submission_id"`
	TaskCode     string          `json:"task_code"`
	Version      int             `json:"version"`
	AgentRef     string          `json:"agent_ref"`
	Payload      json.RawMessage `json:"payload"`
}

func (r *receiver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, requestBodyLimit+1))
		if err != nil || len(body) > requestBodyLimit {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		key := req.Header.Get("Idempotency-Key")
		task := req.Header.Get("Kungfu-Task")
		version := req.Header.Get("Kungfu-Task-Version")
		if key == "" || task == "" || version == "" {
			http.Error(w, "Idempotency-Key, Kungfu-Task and Kungfu-Task-Version headers are required", http.StatusBadRequest)
			return
		}
		var d deliveryRequest
		if err := json.Unmarshal(body, &d); err != nil {
			http.Error(w, "body must be the §7.1 JSON object", http.StatusBadRequest)
			return
		}
		if d.SubmissionID == "" || key != d.SubmissionID {
			http.Error(w, "Idempotency-Key must equal submission_id", http.StatusBadRequest)
			return
		}

		// §7.1: the same key redelivered returns the FIRST outcome
		// unchanged. In-memory only — a production deployment
		// persists this map (durable idempotency across restarts).
		r.mu.Lock()
		if r.results == nil {
			r.results = map[string]cachedResult{}
		}
		if first, ok := r.results[key]; ok {
			r.mu.Unlock()
			writeResult(w, first)
			return
		}
		r.mu.Unlock()

		if req.Header.Get("Kungfu-Test") == "1" {
			// open-time test delivery (§5.4): acknowledge, never judge
			status := http.StatusOK
			if r.cfg.Mode == "async" {
				status = http.StatusAccepted
			}
			out, _ := json.Marshal(map[string]string{"status": "test"})
			res := cachedResult{status: status, body: out}
			r.remember(key, res)
			writeResult(w, res)
			return
		}

		if r.cfg.Mode == "async" {
			out, _ := json.Marshal(map[string]string{"status": "reviewing"})
			res := cachedResult{status: http.StatusAccepted, body: out}
			r.remember(key, res)
			writeResult(w, res)
			go func() {
				v := r.judge(d.Payload)
				if err := r.writeBack(d.SubmissionID, v); err != nil {
					log.Printf("verdict write-back for %s gave up: %v (the platform accepts by timeout past the review window)", d.SubmissionID, err)
				}
			}()
			return
		}

		// sync: judge inside the call, within the §11 response budget
		v := r.judge(d.Payload)
		out, _ := json.Marshal(v)
		status := http.StatusOK
		if !v.Accepted {
			status = syncFailStatus
		}
		res := cachedResult{status: status, body: out}
		r.remember(key, res)
		writeResult(w, res)
	})
}

func (r *receiver) remember(key string, res cachedResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = map[string]cachedResult{}
	}
	if _, ok := r.results[key]; !ok {
		r.results[key] = res
	}
}

func writeResult(w http.ResponseWriter, res cachedResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.status)
	_, _ = w.Write(res.body)
}

// judge evaluates every criterion against the payload and builds a
// §6.1 verdict: all pass → accepted; otherwise the failing criteria,
// a ≤500-rune summed reason and ≤50 annotations (message ≤300 runes).
func (r *receiver) judge(payload []byte) verdict {
	v := verdict{Accepted: true, Retryable: true}
	if len(payload) == 0 {
		v.Accepted = false
		v.Criteria = r.criterionIDs()
		v.Reason = truncateRunes("payload is empty", maxReasonRunes)
		return v
	}
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		v.Accepted = false
		v.Criteria = r.criterionIDs()
		v.Reason = truncateRunes("payload is not valid JSON: "+err.Error(), maxReasonRunes)
		return v
	}

	var reasons []string
	for i := range r.cfg.Criteria {
		c := &r.cfg.Criteria[i]
		ptr, msg, ok := r.checkCriterion(c, doc, payload)
		if ok {
			continue
		}
		v.Accepted = false
		v.Criteria = append(v.Criteria, c.ID)
		reasons = append(reasons, c.ID+": "+msg)
		if len(v.Annotations) < maxAnnotations {
			v.Annotations = append(v.Annotations, annotation{
				Pointer:   ptr,
				Criterion: c.ID,
				Message:   truncateRunes(msg, maxAnnotationRunes),
			})
		}
	}
	if !v.Accepted {
		v.Reason = truncateRunes(strings.Join(reasons, "; "), maxReasonRunes)
	}
	return v
}

func (r *receiver) criterionIDs() []string {
	ids := make([]string, 0, len(r.cfg.Criteria))
	for i := range r.cfg.Criteria {
		ids = append(ids, r.cfg.Criteria[i].ID)
	}
	return ids
}

// checkCriterion runs one criterion. It returns the annotation pointer
// ("" when none applies), a message and whether the criterion passed.
func (r *receiver) checkCriterion(c *Criterion, doc any, payload []byte) (string, string, bool) {
	for _, ptr := range c.Required {
		val, ok := atPointer(doc, ptr)
		if !ok {
			return ptr, fmt.Sprintf("required value at %s is missing", ptr), false
		}
		if isEmpty(val) {
			return ptr, fmt.Sprintf("required value at %s is empty", ptr), false
		}
	}
	if c.Pattern != nil {
		val, ok := atPointer(doc, c.Pattern.Pointer)
		if !ok {
			return c.Pattern.Pointer, fmt.Sprintf("value at %s is missing", c.Pattern.Pointer), false
		}
		s, isStr := val.(string)
		if !isStr {
			return c.Pattern.Pointer, fmt.Sprintf("value at %s is not a string", c.Pattern.Pointer), false
		}
		if !c.Pattern.re.MatchString(s) {
			return c.Pattern.Pointer, fmt.Sprintf("value at %s does not match %s", c.Pattern.Pointer, c.Pattern.Regex), false
		}
	}
	if c.compiled != nil {
		if err := c.compiled.Validate(doc); err != nil {
			return "", fmt.Sprintf("payload fails the schema: %v", err), false
		}
	}
	if c.Rubric != nil {
		if msg, ok := r.scoreRubric(c.Rubric, payload); !ok {
			return "", msg, false
		}
	}
	return "", "", true
}

// scoreRubric asks an OpenAI-compatible /chat/completions endpoint to
// grade the payload against the rubric text and compares the returned
// 0–1 score with the threshold. Any parse or transport failure fails
// the criterion with the reason in the message.
func (r *receiver) scoreRubric(rb *Rubric, payload []byte) (string, bool) {
	prompt := fmt.Sprintf("%s\n\nPayload:\n%s\n\nReply with ONLY a JSON object of the form {\"score\": <number 0-1>, \"reason\": \"<short text>\"}.",
		rb.Text, payload)
	reqBody, _ := json.Marshal(map[string]any{
		"model":    r.env.modelName,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(r.env.modelBaseURL, "/")+"/chat/completions",
		bytes.NewReader(reqBody))
	if err != nil {
		return "cannot build the model request: " + err.Error(), false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.env.modelAPIKey)
	client := &http.Client{Timeout: modelCallTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "model call failed: " + err.Error(), false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, requestBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("model call returned HTTP %d", resp.StatusCode), false
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil || len(completion.Choices) == 0 {
		return "model reply was not a chat completion", false
	}
	score, reason, err := parseScore(completion.Choices[0].Message.Content)
	if err != nil {
		return "model reply was not valid JSON {\"score\",\"reason\"}: " + err.Error(), false
	}
	if score < rb.Threshold {
		return fmt.Sprintf("model score %.2f below threshold %.2f: %s", score, rb.Threshold, reason), false
	}
	return "", true
}

// parseScore reads {"score": 0–1, "reason": "..."} from the model
// reply, falling back to the first braced object in the text.
func parseScore(content string) (float64, string, error) {
	candidate := strings.TrimSpace(content)
	if !strings.HasPrefix(candidate, "{") {
		if i := strings.IndexByte(candidate, '{'); i >= 0 {
			if j := strings.LastIndexByte(candidate, '}'); j > i {
				candidate = candidate[i : j+1]
			}
		}
	}
	var out struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(candidate), &out); err != nil {
		return 0, "", err
	}
	return out.Score, out.Reason, nil
}

// writeBack posts the verdict to the platform: retry three times with
// the backoff schedule, then give up (the platform accepts by timeout
// past the review window, §6.2).
func (r *receiver) writeBack(submissionID string, v verdict) error {
	body, _ := json.Marshal(map[string]any{"submission_id": submissionID, "verdict": v})
	url := strings.TrimRight(r.env.kungfuBaseURL, "/") + "/api/v1/task_verdict"
	var lastErr error
	for attempt := 0; attempt <= len(verdictRetryBackoff); attempt++ {
		if attempt > 0 {
			time.Sleep(verdictRetryBackoff[attempt-1])
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+r.env.agentKey)
		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return lastErr
}

// -- JSON Pointer (RFC 6901) ---------------------------------------------

// atPointer resolves an RFC 6901 pointer ("/a/b", "" = the root
// document) against a decoded JSON value.
func atPointer(doc any, pointer string) (any, bool) {
	if pointer == "" {
		return doc, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	cur := doc
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			idx := -1
			if _, err := fmt.Sscanf(token, "%d", &idx); err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// isEmpty reports the §"非空" reading: empty string, empty array or
// empty object; every other present value counts as non-empty.
func isEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case nil:
		return true
	}
	return false
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func main() {
	configPath := flag.String("config", "receiver.json", "path to receiver.json")
	flag.Parse()
	r, err := loadConfig(*configPath, envFromOS())
	if err != nil {
		fmt.Fprintln(os.Stderr, "receiver:", err)
		os.Exit(1)
	}
	log.Printf("kungfu reference receiver listening on %s (mode %s, %d criteria)",
		r.cfg.Listen, r.cfg.Mode, len(r.cfg.Criteria))
	log.Fatal(http.ListenAndServe(r.cfg.Listen, r.handler()))
}
