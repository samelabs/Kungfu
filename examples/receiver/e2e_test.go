package main

// WO-10 end-to-end executor journeys. The reference receiver serves
// deliveries over a real TLS httptest server; the platform runs
// in-process (internal/server router on an httptest server, real test
// PostgreSQL); every executor move goes through HTTP POST /api/v1 and
// the driver below decides ONLY by the returned next_action. Time
// advances by calling the platform's periodic workers with a moved
// `now` (ExpireClaims / RecoverSubmissions / ExpireReviews) — never by
// sleeping, and never by asserting global pass counts.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kungfu.md/internal/config"
	"kungfu.md/internal/delivery"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/server"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

// e2eTLSCert makes the self-signed receiver URL both pass contract
// validation and verify against the hardened delivery client; the
// delivery client is made to trust it via delivery.TrustRootsForTest (same
// technique as internal/mcpserver's TestMain).
var e2eTLSCert tls.Certificate

func TestMain(m *testing.M) {
	e2eTLSCert = mustE2ECert()
	restore := delivery.AllowLoopbackForTest()
	// the write-back retry schedule is seconds in production; the e2e
	// journeys only need the retry SHAPE, not the spacing
	verdictRetryBackoff = []time.Duration{
		50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond}
	code := m.Run()
	restore()
	os.Exit(code)
}

func mustE2ECert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, perr := x509.ParseCertificate(der)
	if perr != nil {
		panic(perr)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	// Trust the test cert in the delivery client explicitly: macOS Go
	// verifies with the system keychain and ignores SSL_CERT_FILE.
	delivery.TrustRootsForTest(pool)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// -- harness -------------------------------------------------------------

// faultInjector wraps the receiver handler: the first `hang` deliveries
// stall past the platform's response timeout (→ uncertain), the first
// `fail` deliveries answer 503 (→ failed). The receiver's own logic is
// untouched.
type faultInjector struct {
	inner http.Handler
	hang  atomic.Int32
	fail  atomic.Int32
}

func (f *faultInjector) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h := f.hang.Add(-1); h >= 0 {
		time.Sleep(5 * time.Second) // > the 1s test-injected request timeout
	}
	if n := f.fail.Add(-1); n >= 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"receiver unavailable"}`))
		return
	}
	f.inner.ServeHTTP(w, req)
}

type e2eEnv struct {
	pool       *pg.Pool
	kungfu     *httptest.Server
	rcv        *httptest.Server
	injector   *faultInjector
	secret     string // SessionSecret = the agent_ref HMAC key
	pubKey     string // publisher Agent key
	pubID      int64
	agentKey   string // executor Agent key
	agentID    int64
	nextUnique atomic.Int64
}

func (e *e2eEnv) call(key, tool string, args map[string]any) (int, map[string]any) {
	body, _ := json.Marshal(args)
	req, _ := http.NewRequest(http.MethodPost, e.kungfu.URL+"/api/v1/"+tool, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := e.kungfu.Client().Do(req)
	if err != nil {
		return 0, map[string]any{"ok": false,
			"error": map[string]any{"code": "TRANSPORT", "message": err.Error()}}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env
}

// mustCall asserts acceptance and returns the envelope.
func (e *e2eEnv) mustCall(t *testing.T, key, tool string, args map[string]any) map[string]any {
	t.Helper()
	status, env := e.call(key, tool, args)
	if status != 200 || env["ok"] != true {
		t.Fatalf("%s: %d %v", tool, status, env)
	}
	return env
}

func (e *e2eEnv) agent(tool string, args map[string]any) (int, map[string]any) {
	return e.call(e.agentKey, tool, args)
}

func (e *e2eEnv) uniq(prefix string) string {
	return fmt.Sprintf("%s%d%d", prefix, time.Now().UnixNano()%1_000_000, e.nextUnique.Add(1))
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	secret := "wo10-e2e-session-secret"
	srv := server.New(&config.Config{SessionSecret: secret}, pool)
	kungfu := httptest.NewServer(srv.Router)
	t.Cleanup(kungfu.Close)

	e := &e2eEnv{pool: pool, kungfu: kungfu, secret: secret}
	// publisher + executor accounts (the executor starts at zero)
	e.pubID, e.pubKey = e.seedAccount(t, 100_000)
	e.agentID, e.agentKey = e.seedAccount(t, 0)
	return e
}

func (e *e2eEnv) seedAccount(t *testing.T, balance int64) (int64, string) {
	t.Helper()
	reg, err := service.Register(context.Background(), e.pool,
		e.uniq("e2ebot"), "passpass123", "127.0.0.1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var id int64
	_ = e.pool.QueryRow(context.Background(),
		`SELECT id FROM tb_bots WHERE bot_name = $1`, reg.BotName).Scan(&id)
	if balance > 0 {
		if _, err := e.pool.Exec(context.Background(),
			`UPDATE tb_bots SET balance = $2 WHERE id = $1`, id, balance); err != nil {
			t.Fatal(err)
		}
	}
	return id, reg.Key
}

// startReceiver loads a receiver config and serves it over TLS with
// the fault injector in front. env carries the receiver's own
// outbound credentials (the async verdict write-back).
func (e *e2eEnv) startReceiver(t *testing.T, cfgJSON string, env envConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receiver.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := loadConfig(path, env)
	if err != nil {
		t.Fatalf("receiver config: %v", err)
	}
	e.injector = &faultInjector{inner: r.handler()}
	srv := httptest.NewUnstartedServer(e.injector)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{e2eTLSCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	e.rcv = srv
}

const e2eGoodPayload = `{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`

// e2eContract builds a task_create contract map; the receiver URL and
// the mutation come from the journey.
func e2eContract(receiverURL string, mutate func(map[string]any)) map[string]any {
	c := map[string]any{
		"title":     "E2E summary",
		"objective": "Three bullets of a page for a newsletter.",
		"inputs":    "A public URL fetched by the executor.",
		"output": map[string]any{
			"description": "One JSON object with the bullets.",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string"},
					"bullets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 3, "maxItems": 3},
				},
				"required": []string{"url", "bullets"},
			},
		},
		"acceptance": map[string]any{
			"mode":     "sync",
			"criteria": []any{map[string]any{"id": "C1", "kind": "rule", "description": "three bullets"}},
		},
		"examples": []any{
			map[string]any{"payload": json.RawMessage(e2eGoodPayload), "accepted": true},
		},
		"price": 5,
	}
	if receiverURL != "" {
		c["receiver"] = map[string]any{"url": receiverURL}
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

// publishTask creates and opens a task through the publisher's own
// /api/v1 tools (task_create → task_open; the open-time test delivery
// must pass through the receiver).
func (e *e2eEnv) publishTask(t *testing.T, contract map[string]any) string {
	t.Helper()
	env := e.mustCall(t, e.pubKey, "task_create", map[string]any{"contract": contract, "budget": 1000})
	code, _ := env["code"].(string)
	if code == "" {
		t.Fatalf("task_create returned no code: %v", env)
	}
	open := e.mustCall(t, e.pubKey, "task_open", map[string]any{"code": code})
	if open["status"] != "open" {
		t.Fatalf("task_open: %v", open)
	}
	return code
}

func (e *e2eEnv) checkInvariants(t *testing.T, code string) {
	t.Helper()
	tr, err := repository.FindTaskByCode(context.Background(), e.pool, code)
	if err != nil || tr == nil {
		t.Fatalf("reload task %s: %v", code, err)
	}
	if err := task.CheckInvariants(context.Background(), e.pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants(%s): %v", code, err)
	}
}

// agentBalance reads the executor's credit balance through the surface
// an agent uses.
func (e *e2eEnv) agentBalance(t *testing.T) int64 {
	t.Helper()
	st := e.mustCall(t, e.agentKey, "account_status", map[string]any{})
	balance, _ := st["balance"].(float64)
	return int64(balance)
}

// sha256Hex is the memory checksum seed helper.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// -- the executor driver --------------------------------------------------

// drive runs an executor strictly by next_action (§8.3): it submits,
// then follows whatever the platform says — poll (after the journey's
// clock advance), revise (corrected payload + revises), retry (after
// the journey's reprepare, e.g. a fresh claim; the return value
// replaces the riding extra), wait (resend) — until done or stop.
type drive struct {
	agent    func(tool string, args map[string]any) (int, map[string]any)
	payload  map[string]any
	extra    map[string]any // rides on every work_submit (claim_id…)
	key      string         // request_key namespace; defaults to the task code
	onRevise func(attempt int) map[string]any
	onRetry  func(env map[string]any) map[string]any
	onPoll   func(env map[string]any)
}

func (d *drive) run(t *testing.T, code string) []map[string]any {
	t.Helper()
	if d.key == "" {
		d.key = code
	}
	steps := []map[string]any{}
	attempt := 0
	submitArgs := func() map[string]any {
		attempt++
		args := map[string]any{
			"code": code, "request_key": fmt.Sprintf("e2e-%s-%d", d.key, attempt),
			"payload": d.payload,
		}
		for k, v := range d.extra {
			args[k] = v
		}
		return args
	}
	args, act := submitArgs(), "work_submit"
	var revises any
	for i := 0; i < 24; i++ {
		if revises != nil {
			args["revises"] = revises
		}
		status, env := d.agent(act, args)
		steps = append(steps, env)
		if env["next_action"] == nil && status != 200 {
			t.Fatalf("driver: %s → %d %v", act, status, env)
		}
		switch na, _ := env["next_action"].(string); na {
		case "done", "stop":
			return steps
		case "poll":
			if d.onPoll != nil {
				d.onPoll(env)
			}
			act, args = "work_status", map[string]any{"submission_id": env["submission_id"]}
		case "revise":
			if d.onRevise == nil {
				t.Fatalf("revise with no correction available: %v", env)
			}
			revises = env["submission_id"]
			d.payload = d.onRevise(attempt)
			args, act = submitArgs(), "work_submit"
		case "retry":
			if d.onRetry != nil {
				if nx := d.onRetry(env); nx != nil {
					d.extra = nx
				}
			}
			args, act = submitArgs(), "work_submit"
		case "wait":
			// rate limit: resend the identical request
		case "submit":
			act = "work_submit"
		default:
			t.Fatalf("driver: unknown next_action %q (%v)", na, env)
		}
	}
	t.Fatal("driver did not converge in 24 steps")
	return nil
}

// nextActions renders the journey's step chain: next_action when
// present, otherwise the error code of a not-accepted call.
func nextActions(steps []map[string]any) []string {
	out := make([]string, 0, len(steps))
	for _, env := range steps {
		if na, _ := env["next_action"].(string); na != "" {
			out = append(out, na)
		} else if errObj, ok := env["error"].(map[string]any); ok {
			out = append(out, fmt.Sprint(errObj["code"]))
		}
	}
	return out
}

func wantActions(t *testing.T, steps []map[string]any, want ...string) {
	t.Helper()
	if got := nextActions(steps); !strings.HasPrefix(strings.Join(got, ","), strings.Join(want, ",")) {
		t.Fatalf("step chain = %v, want prefix %v", got, want)
	}
}

// -- journeys --------------------------------------------------------------

// 1) sync: work_list discovery → work_harness read → submit → the
// receiver accepts → settled, the executor is paid.
func TestE2EJourney1SyncAccepted(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url","/bullets"]}]}`, envConfig{})

	// a publisher memory doubles as the harness snapshot
	memCode := fmt.Sprintf("w1%010d", time.Now().UnixNano()%10_000_000_000)
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status)
		VALUES ($1, $2, 'E2E harness', '["t"]', 'fetch the page, write three bullets', $3, 'private', 'active')`,
		memCode, e.pubID, sha256Hex(memCode)); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	code := e.publishTask(t, e2eContract(e.rcv.URL, func(c map[string]any) {
		c["harness_refs"] = []string{memCode}
	}))

	// discovery: work_list carries the task
	list := e.mustCall(t, e.agentKey, "work_list", map[string]any{})
	found := false
	for _, it := range list["tasks"].([]any) {
		if it.(map[string]any)["code"] == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("work_list does not carry %s", code)
	}
	// harness read: work_get lists the ref, work_harness returns it
	got := e.mustCall(t, e.agentKey, "work_get", map[string]any{"code": code})
	harness := e.mustCall(t, e.agentKey, "work_harness", map[string]any{"code": code, "ref_id": memCode})
	if !strings.Contains(fmt.Sprint(got["harness"]), memCode) ||
		!strings.Contains(fmt.Sprint(harness), "three bullets") {
		t.Fatalf("harness read: %v / %v", got["harness"], harness)
	}

	before := e.agentBalance(t)
	d := drive{agent: e.agent, payload: map[string]any{
		"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"},
	}}
	steps := d.run(t, code)
	wantActions(t, steps, "done")
	final := steps[len(steps)-1]
	if final["state"] != "settled" || final["paid"].(float64) != 5 {
		t.Fatalf("final step: %v", final)
	}
	if d := e.agentBalance(t) - before; d != 5 {
		t.Fatalf("executor balance delta = %d, want 5", d)
	}
	e.checkInvariants(t, code)
}

// 2) sync rejection with annotations → revise → the revision carries
// revises and is accepted.
func TestE2EJourney2SyncRevise(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","pattern":{"pointer":"/url","regex":"^https://[^\\s]+$"}}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, nil))

	d2 := drive{
		agent:   e.agent,
		payload: map[string]any{"url": "ftp://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
		onRevise: func(int) map[string]any {
			return map[string]any{"url": "https://example.com/fixed", "bullets": []string{"s1", "s2", "s3"}}
		},
	}
	steps := d2.run(t, code)
	wantActions(t, steps, "revise", "done")

	rejected := steps[0]
	if rejected["state"] != "rejected" {
		t.Fatalf("first step not a rejection: %v", rejected)
	}
	v, _ := rejected["verdict"].(map[string]any)
	ann, _ := v["annotations"].([]any)
	if len(ann) == 0 {
		t.Fatalf("rejection carries no annotations: %v", v)
	}
	if a := ann[0].(map[string]any); a["pointer"] != "/url" || a["criterion"] != "C1" {
		t.Fatalf("annotation = %v, want /url C1", a)
	}
	if steps[len(steps)-1]["state"] != "settled" {
		t.Fatalf("final state = %v", steps[len(steps)-1])
	}
	// the revision row carries revises = the rejected submission
	tr, _ := repository.FindTaskByCode(context.Background(), e.pool, code)
	var revises int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT revises FROM tb_task_submission WHERE task_id = $1 AND revises IS NOT NULL`,
		tr.ID).Scan(&revises); err != nil {
		t.Fatalf("no revising submission: %v", err)
	}
	if rejected["submission_id"].(string) != fmt.Sprint(revises) {
		t.Fatalf("revises = %d, want the rejected %v", revises, rejected["submission_id"])
	}
	e.checkInvariants(t, code)
}

// 3) async: the publisher's verdict accepts one submission; the other
// is never judged — the review window closes by clock advance and the
// platform accepts by timeout.
func TestE2EJourney3AsyncVerdictAndTimeout(t *testing.T) {
	e := newE2EEnv(t)
	// async WITHOUT a receiver: every submission parks in under_review
	code := e.publishTask(t, e2eContract("", func(c map[string]any) {
		c["acceptance"] = map[string]any{
			"mode":          "async",
			"review_window": 3600,
			"criteria":      []any{map[string]any{"id": "C1", "kind": "judgment", "description": "factual bullets"}},
		}
	}))
	payload := map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}}

	// (a) judged by the publisher as soon as it parks
	firstDrive := drive{
		agent: e.agent, payload: payload,
		onPoll: func(env map[string]any) {
			verdict := e.mustCall(t, e.pubKey, "task_verdict", map[string]any{
				"submission_id": env["submission_id"],
				"verdict":       map[string]any{"accepted": true},
			})
			if verdict["state"] != "settled" {
				t.Fatalf("publisher verdict: %v", verdict)
			}
		},
	}
	first := firstDrive.run(t, code)
	wantActions(t, first, "poll", "done")
	if v, _ := first[len(first)-1]["verdict"].(map[string]any); v["source"] != "publisher" {
		t.Fatalf("verdict source = %v, want publisher", v)
	}

	// (b) never judged: the window closes by clock advance
	secondDrive := drive{
		agent: e.agent, payload: payload, key: code + "-timeout",
		onPoll: func(map[string]any) {
			if _, err := service.ExpireReviews(context.Background(), e.pool,
				time.Now().Add(2*time.Hour), 100); err != nil {
				t.Fatalf("ExpireReviews: %v", err)
			}
		},
	}
	second := secondDrive.run(t, code)
	wantActions(t, second, "poll", "done")
	if v, _ := second[len(second)-1]["verdict"].(map[string]any); v["source"] != "timeout" || v["accepted"] != true {
		t.Fatalf("timeout verdict = %v", second[len(second)-1]["verdict"])
	}
	e.checkInvariants(t, code)
}

// 3c) the reference receiver in ASYNC mode: submissions park in
// under_review, the receiver judges in the background and writes the
// verdict back through POST /api/v1/task_verdict with the publisher
// key — reject (revise), then the revision is accepted.
func TestE2EJourney3cAsyncReceiverWriteBack(t *testing.T) {
	e := newE2EEnv(t)
	// pattern fails on the first payload, passes after the revision
	e.startReceiver(t, `{"mode":"async","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","pattern":{"pointer":"/url","regex":"^https://[^\\s]+$"}}]}`,
		envConfig{kungfuBaseURL: e.kungfu.URL, agentKey: e.pubKey})
	code := e.publishTask(t, e2eContract(e.rcv.URL, func(c map[string]any) {
		c["acceptance"] = map[string]any{
			"mode":          "async",
			"review_window": 3600,
			"criteria":      []any{map[string]any{"id": "C1", "kind": "rule", "description": "https url"}},
		}
	}))

	// waitWriteBack polls until the receiver's background verdict
	// lands: the submission leaves under_review (→ rejected or settled)
	waitWriteBack := func(env map[string]any) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			sub, _ := repository.FindSubmissionByID(context.Background(), e.pool,
				parseID(t, env["submission_id"]))
			if sub != nil && sub.State != task.SubUnderReview && sub.State != "" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("receiver write-back did not land within 5s (submission %v)",
			env["submission_id"])
	}

	wb := drive{
		agent: e.agent,
		payload: map[string]any{"url": "ftp://example.com/a",
			"bullets": []string{"s1", "s2", "s3"}},
		key:    code + "-wb",
		onPoll: func(env map[string]any) { waitWriteBack(env) },
		onRevise: func(int) map[string]any {
			return map[string]any{"url": "https://example.com/fixed", "bullets": []string{"s1", "s2", "s3"}}
		},
	}
	steps := wb.run(t, code)
	// Two legal orders: the 202 envelope may reach the executor before
	// or after the background write-back lands.
	got := strings.Join(nextActions(steps), ",")
	if got != "revise,done" && !strings.HasPrefix(got, "poll,revise,poll,done") {
		t.Fatalf("step chain = %q, want revise→done (write-back won the race) or poll→revise→poll→done", got)
	}
	var rejected map[string]any
	for _, st := range steps {
		if st["state"] == "rejected" {
			rejected = st
		}
	}
	if rejected == nil {
		t.Fatalf("no rejected step in %v", nextActions(steps))
	}
	if v, _ := rejected["verdict"].(map[string]any); v == nil || v["source"] != "publisher" {
		t.Fatalf("write-back rejection missing or wrong source: %v", rejected)
	}
	final := steps[len(steps)-1]
	if final["state"] != "settled" || final["paid"].(float64) != 5 {
		t.Fatalf("final step: %v", final)
	}
	e.checkInvariants(t, code)
}

// parseID reads a wire id (string or number) as int64.
func parseID(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case string:
		var id int64
		if _, err := fmt.Sscanf(n, "%d", &id); err != nil {
			t.Fatalf("bad id %q", n)
		}
		return id
	case float64:
		return int64(n)
	}
	t.Fatalf("bad id %v", v)
	return 0
}

// 4) claims: claim → renew → submit with the claim; then a claim aged
// past its TTL → CLAIM_INVALID/retry → re-claim → done.
func TestE2EJourney4ClaimLifecycle(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url"]}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, func(c map[string]any) {
		c["claim"] = map[string]any{"required": true}
	}))
	payload := map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}}

	// (a) claim → renew → submit with claim_id → accepted
	claim := e.mustCall(t, e.agentKey, "work_claim", map[string]any{"code": code})
	renewed := e.mustCall(t, e.agentKey, "work_claim_renew",
		map[string]any{"claim_id": claim["claim_id"]})
	if renewed["next_action"] != "submit" {
		t.Fatalf("renew next_action = %v", renewed["next_action"])
	}
	d3 := drive{agent: e.agent, payload: payload,
		extra: map[string]any{"claim_id": claim["claim_id"]}}
	steps := d3.run(t, code)
	wantActions(t, steps, "done")

	// (b) a fresh claim ages past its TTL before any submission
	aged := e.mustCall(t, e.agentKey, "work_claim", map[string]any{"code": code})
	// age THIS claim past its TTL (the shared gate database may hold
	// hundreds of other expired active claims from earlier packages,
	// which a global ExpireClaims pass would drain first) — the same
	// task -> claim lock order and event as the reclaimer
	ageClaim(t, e, code, parseID(t, aged["claim_id"]))
	d4 := drive{
		agent: e.agent, payload: payload, key: code + "-b",
		extra: map[string]any{"claim_id": aged["claim_id"]},
		onRetry: func(map[string]any) map[string]any {
			fresh := e.mustCall(t, e.agentKey, "work_claim", map[string]any{"code": code})
			return map[string]any{"claim_id": fresh["claim_id"]}
		},
	}
	steps2 := d4.run(t, code)
	for i, st := range steps2 {
		t.Logf("J4b step %d: %v", i, st)
	}
	wantActions(t, steps2, "retry", "done")
	if errObj, _ := steps2[0]["error"].(map[string]any); errObj["code"] != "CLAIM_INVALID" {
		t.Fatalf("first step error = %v, want CLAIM_INVALID", steps2[0]["error"])
	}
	e.checkInvariants(t, code)
}

// ageClaim expires one claim of this journey's task (§5.2 expiry:
// event expire + reservation release, task -> claim lock order).
func ageClaim(t *testing.T, e *e2eEnv, code string, claimID int64) {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, e.pool, code)
	tx, err := e.pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := repository.FindTaskByIDForUpdate(ctx, tx, tr.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("task lock: %v", err)
	}
	claim, err := repository.FindClaimByIDForUpdate(ctx, tx, claimID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("claim lock: %v", err)
	}
	if err := repository.ApplyClaimStatus(ctx, tx, claimID, task.ClaimActive, task.EventClaimExpire); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("expire: %v", err)
	}
	if err := repository.ReleaseTaskReservation(ctx, tx, tr.ID, claim.Amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("release: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// 5) delivery stall → uncertain/poll → the receiver recovers, the
// clock advances 31s and RecoverSubmissions settles the redelivery.
func TestE2EJourney5UncertainRecovery(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url"]}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, nil))

	// this journey alone runs with a 1s delivery request budget
	// (production 10s) so the stall resolves quickly; the rest of the
	// suite keeps the §11 value
	started := time.Now()
	delivery.SetRequestTimeoutForTest(time.Second)
	t.Cleanup(func() { delivery.SetRequestTimeoutForTest(0) })
	e.injector.hang.Store(1) // the first delivery stalls past the 1s test budget
	d5 := drive{
		agent: e.agent,
		payload: map[string]any{"url": "https://example.com/a",
			"bullets": []string{"s1", "s2", "s3"}},
		onPoll: func(map[string]any) {
			// 31s past the stall: the redelivery cadence is due and the
			// receiver has recovered — one recovery pass settles it
			if _, err := service.RecoverSubmissions(context.Background(), e.pool,
				[]byte(e.secret), time.Now().Add(31*time.Second), 50); err != nil {
				t.Fatalf("RecoverSubmissions: %v", err)
			}
		},
	}
	steps := d5.run(t, code)
	wantActions(t, steps, "poll", "done")
	if steps[0]["state"] != "uncertain" {
		t.Fatalf("first state = %v, want uncertain", steps[0]["state"])
	}
	if final := steps[len(steps)-1]; final["state"] != "settled" || final["paid"].(float64) != 5 {
		t.Fatalf("final step: %v", final)
	}
	if d := time.Since(started); d >= 3*time.Second {
		t.Fatalf("journey took %s with the injected 1s delivery budget, want < 3s", d)
	}
	e.checkInvariants(t, code)
}

// 6) five consecutive receiver 5xx → the task auto-pauses
// (RECEIVER_FAULT); the executor's next submission gets TASK_NOT_OPEN
// and stops.
func TestE2EJourney6ReceiverFaultPause(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url"]}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, nil))

	e.injector.fail.Store(1000) // every delivery 503s
	d := drive{agent: e.agent, payload: map[string]any{
		"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"},
	}}
	steps := d.run(t, code)
	got := nextActions(steps)
	if len(got) != 6 || got[0] != "retry" || got[len(got)-1] != "stop" {
		t.Fatalf("step chain = %v, want retry x5 then stop", got)
	}
	for i := 0; i < 5; i++ {
		if steps[i]["state"] != "failed" {
			t.Fatalf("step %d state = %v, want failed", i, steps[i]["state"])
		}
	}
	if errObj, _ := steps[len(steps)-1]["error"].(map[string]any); errObj["code"] != "TASK_NOT_OPEN" {
		t.Fatalf("final error = %v, want TASK_NOT_OPEN", steps[len(steps)-1]["error"])
	}
	paused := e.mustCall(t, e.pubKey, "task_get", map[string]any{"code": code})
	if paused["status"] != "paused" || paused["paused_reason"] != "RECEIVER_FAULT" {
		t.Fatalf("task after faults: %v", paused)
	}
	e.checkInvariants(t, code)
}

// 7) rejection cap: max_rejected_per_agent reached → SUBMISSION_LIMIT
// and stop.
func TestE2EJourney7RejectionCap(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","pattern":{"pointer":"/url","regex":"^https://never\\.matches$"}}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, func(c map[string]any) {
		c["limits"] = map[string]any{"max_rejected_per_agent": 1}
	}))

	payload := map[string]any{"url": "ftp://nope", "bullets": []string{"s1", "s2", "s3"}}
	d7 := drive{
		agent: e.agent, payload: payload,
		// the executor revises but still misses the pattern
		onRevise: func(int) map[string]any { return payload },
	}
	steps := d7.run(t, code)
	wantActions(t, steps, "revise", "stop")
	if errObj, _ := steps[len(steps)-1]["error"].(map[string]any); errObj["code"] != "SUBMISSION_LIMIT" {
		t.Fatalf("final error = %v, want SUBMISSION_LIMIT", steps[len(steps)-1]["error"])
	}
	e.checkInvariants(t, code)
}

// 8) platform pre-check: a schema-violating payload gets
// SCHEMA_MISMATCH with a pointer and revise; the corrected payload is
// accepted.
func TestE2EJourney8SchemaMismatchRevise(t *testing.T) {
	e := newE2EEnv(t)
	e.startReceiver(t, `{"mode":"sync","listen":"127.0.0.1:0","criteria":[
		{"id":"C1","required":["/url"]}]}`, envConfig{})
	code := e.publishTask(t, e2eContract(e.rcv.URL, nil))

	d8 := drive{
		agent: e.agent,
		payload: map[string]any{"url": "https://example.com/a",
			"bullets": []string{"only one"}},
		onRevise: func(int) map[string]any {
			return map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}}
		},
	}
	steps := d8.run(t, code)
	wantActions(t, steps, "revise", "done")
	errObj, _ := steps[0]["error"].(map[string]any)
	if errObj["code"] != "SCHEMA_MISMATCH" {
		t.Fatalf("first error = %v, want SCHEMA_MISMATCH", steps[0]["error"])
	}
	if !strings.Contains(fmt.Sprint(errObj["details"]), "/bullets") {
		t.Fatalf("SCHEMA_MISMATCH details carry no pointer: %v", errObj["details"])
	}
	if steps[len(steps)-1]["state"] != "settled" {
		t.Fatalf("final state = %v", steps[len(steps)-1])
	}
	e.checkInvariants(t, code)
}
