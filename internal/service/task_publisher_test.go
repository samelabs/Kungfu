package service

// Publisher lifecycle tests (WO-2b) against a real PostgreSQL with
// httptest receivers. Every test ends with task.CheckInvariants.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

func pubTestPool(t *testing.T) *pg.Pool {
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
	return pool
}

func pubSeedBot(t *testing.T, pool *pg.Pool, balance int64) int64 {
	t.Helper()
	name := fmt.Sprintf("pub_%d_%d", time.Now().UnixNano(), balance)
	digest := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'p1p1', 'x', $3) RETURNING id`,
		name, digest[:], balance).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return id
}

func pubSeedKungfu(t *testing.T, pool *pg.Pool, botID int64, code, title string) {
	t.Helper()
	sum := sha256.Sum256([]byte(code))
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tb_kungfus (code, bot_id, title, tags_json, content, checksum, visibility, status)
		VALUES ($1, $2, $3, '["t"]', 'harness body', $4, 'private', 'active')`,
		code, botID, title, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("seed kungfu: %v", err)
	}
}

// pubReceiver is an httptest receiver capturing the last request; its
// response body echoes the configured status.
type pubReceiver struct {
	mu      sync.Mutex
	headers http.Header
	body    []byte
	status  int
	url     string
}

func startPubReceiver(t *testing.T, status int) *pubReceiver {
	t.Helper()
	r := &pubReceiver{status: status}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.headers = req.Header.Clone()
		r.body = body
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":%d}`, status)))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pubTestTLSCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// pubContract is a complete §3 contract; an empty receiverURL uses the
// package-wide accept-everything receiver.
func pubContract(receiverURL string) task.Contract {
	if receiverURL == "" {
		receiverURL = okReceiverURL
	}
	return task.Contract{
		Title:        "Summarize a page",
		Requirements: "Fetch the given page and return exactly three summary bullets for a newsletter.",
		Output: task.Output{
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"url": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
				},
				"required": ["url", "bullets"]
			}`),
		},
		Receiver: task.Receiver{URL: receiverURL},
		Price:    5,
		Claim:    task.ClaimConfig{Required: true},
	}
}

func ledgerSum(t *testing.T, pool *pg.Pool, botID int64, txnType string) int64 {
	t.Helper()
	var sum int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0) FROM tb_transactions WHERE bot_id = $1 AND type = $2`,
		botID, txnType).Scan(&sum); err != nil {
		t.Fatalf("ledger sum: %v", err)
	}
	return sum
}

// -- §4 create --

func TestPublisherCreateInsufficientCredits(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100)

	_, err := CreateTask(context.Background(), pool, publisher, pubContract("https://example.com/hook"), 2000)
	appErr := appErrOf(t, err)
	if appErr.Code != "INSUFFICIENT_CREDITS" {
		t.Fatalf("code = %s, want INSUFFICIENT_CREDITS", appErr.Code)
	}
	var rows int64
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_task WHERE publisher_id = $1`, publisher).Scan(&rows)
	if rows != 0 {
		t.Fatalf("task rows = %d, want 0 (nothing created)", rows)
	}
	if got := ledgerSum(t, pool, publisher, "lock_task"); got != 0 {
		t.Fatalf("lock_task ledger = %d, want 0 (no debit)", got)
	}
}

func TestPublisherCreateBudgetBelowFloor(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)

	// budget below one price
	_, err := CreateTask(context.Background(), pool, publisher, pubContract(""), 4)
	if appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("budget < price: code = %v, want VALIDATION_FAILED", err)
	}
	var rows int64
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_task WHERE publisher_id = $1`, publisher).Scan(&rows)
	if rows != 0 {
		t.Fatalf("task rows = %d, want 0", rows)
	}
	if got := ledgerSum(t, pool, publisher, "lock_task"); got != 0 {
		t.Fatalf("lock_task ledger = %d, want 0", got)
	}
}

// TestPublisherCreateBudgetOverMax: budget above task.MaxAmount is
// rejected with the budget field named (§4 cap).
func TestPublisherCreateBudgetOverMax(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)

	_, err := CreateTask(context.Background(), pool, publisher, pubContract(""), task.MaxAmount+1)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("budget over max: %v, want VALIDATION_FAILED", err)
	}
	items, _ := appErr.Details["errors"].([]map[string]string)
	if len(items) != 1 || items[0]["field"] != "budget" {
		t.Fatalf("details.errors = %#v, want a single budget field error", appErr.Details)
	}
}

// TestPublisherFundOverMax: a fund amount that would push budget_locked
// past task.MaxAmount is rejected with the amount field named; the
// task keeps its budget (§4 cap).
func TestPublisherFundOverMax(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	code := pubCreateForTest(t, pool, publisher, pubContract("https://example.com/hook"), 10)

	// amount itself within the cap, but 10 + MaxAmount overflows it
	_, err := FundTask(ctx, pool, publisher, code, task.MaxAmount)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("fund over max: %v, want VALIDATION_FAILED", err)
	}
	items, _ := appErr.Details["errors"].([]map[string]string)
	if len(items) != 1 || items[0]["field"] != "amount" {
		t.Fatalf("details.errors = %#v, want a single amount field error", appErr.Details)
	}

	// an amount above the cap is rejected on its own
	_, err = FundTask(ctx, pool, publisher, code, task.MaxAmount+1)
	if appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("fund amount over max: %v, want VALIDATION_FAILED", err)
	}

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.BudgetLocked != 10 {
		t.Fatalf("budget_locked = %d, want 10 (nothing funded)", tr.BudgetLocked)
	}
	if got := ledgerSum(t, pool, publisher, "fund_task"); got != 0 {
		t.Fatalf("fund_task ledger = %d, want 0", got)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestPublisherCreateInvalidContract(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)

	c := pubContract("")
	c.Title = ""        // §3: title required
	c.Requirements = "" // §3: requirements required

	_, err := CreateTask(context.Background(), pool, publisher, c, 2000)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
	}
	items, ok := appErr.Details["errors"].([]map[string]string)
	if !ok || len(items) < 2 {
		t.Fatalf("details.errors = %#v, want field errors for title and requirements", appErr.Details)
	}
	var rows int64
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_task WHERE publisher_id = $1`, publisher).Scan(&rows)
	if rows != 0 {
		t.Fatalf("task rows = %d, want 0", rows)
	}
}

func TestPublisherCreateSuccessLocksBudget(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	view, err := CreateTask(ctx, pool, publisher, pubContract("https://example.com/hook"), 2000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code, _ := view["code"].(string)
	tr, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || tr == nil {
		t.Fatalf("reload task: %v", err)
	}
	if tr.Status != task.TaskDraft {
		t.Fatalf("status = %s, want draft", tr.Status)
	}
	if tr.BudgetLocked != 2000 {
		t.Fatalf("budget_locked = %d, want 2000", tr.BudgetLocked)
	}
	if got := ledgerSum(t, pool, publisher, "lock_task"); got != -2000 {
		t.Fatalf("lock_task ledger = %d, want -2000", got)
	}
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, publisher).Scan(&balance)
	if balance != 8000 {
		t.Fatalf("balance = %d, want 8000", balance)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §4 open + §5.4 test delivery --

func TestPublisherOpenSyncReceiver2xx(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)

	view, err := OpenTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if view["status"] != task.TaskOpen || view["version"] != int32(1) {
		t.Fatalf("view = %v/%v, want open/version 1", view["status"], view["version"])
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Version != 1 {
		t.Fatalf("task.version = %d, want 1", tr.Version)
	}
	v, err := repository.FindTaskVersion(ctx, pool, tr.ID, 1)
	if err != nil || v == nil {
		t.Fatalf("version row: %v", err)
	}

	// §7.1 + §5.4: assert the test request headers and body fields.
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	keyPrefix := "test-" + code + "-1-"
	if !strings.HasPrefix(rcv.headers.Get("Idempotency-Key"), keyPrefix) ||
		len(rcv.headers.Get("Idempotency-Key")) <= len(keyPrefix) ||
		rcv.headers.Get("Kungfu-Task") != code ||
		rcv.headers.Get("Kungfu-Task-Version") != "1" ||
		rcv.headers.Get("Kungfu-Test") != "1" {
		t.Fatalf("test delivery headers = %v", rcv.headers)
	}
	if ct := rcv.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		SubmissionID string          `json:"submission_id"`
		TaskCode     string          `json:"task_code"`
		Version      int             `json:"version"`
		AgentRef     string          `json:"agent_ref"`
		Payload      json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(rcv.body, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rcv.body)
	}
	if body.SubmissionID != rcv.headers.Get("Idempotency-Key") || body.TaskCode != code ||
		body.Version != 1 || body.AgentRef != "test" {
		t.Fatalf("body identity fields = %+v", body)
	}
	// WO-20b: the payload is a FIXED {} — the test delivery checks
	// reachability and liveness, never content
	if string(body.Payload) != "{}" {
		t.Fatalf("payload = %s, want the fixed {}", body.Payload)
	}

	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func pubCreateForTest(t *testing.T, pool *pg.Pool, publisher int64, c task.Contract, budget int64) string {
	t.Helper()
	view, err := CreateTask(context.Background(), pool, publisher, c, budget)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code, _ := view["code"].(string)
	return code
}

func TestPublisherOpenReceiver500(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusInternalServerError)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)

	_, err := OpenTask(ctx, pool, publisher, code)
	appErr := appErrOf(t, err)
	if appErr.Code != "TEST_DELIVERY_FAILED" {
		t.Fatalf("code = %s, want TEST_DELIVERY_FAILED (%v)", appErr.Code, err)
	}
	if sc, _ := appErr.Details["status_code"].(int); sc != 500 {
		t.Fatalf("details.status_code = %#v, want 500", appErr.Details["status_code"])
	}
	if resp, _ := appErr.Details["response"].(string); !strings.Contains(resp, `"status":500`) {
		t.Fatalf("details.response = %#v, want the receiver body excerpt", appErr.Details["response"])
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskDraft || tr.Version != 0 {
		t.Fatalf("task = %s/v%d, want draft/v0 after failed delivery", tr.Status, tr.Version)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestPublisherOpenHarnessNotOwned(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	nonce := time.Now().UnixNano() % 1_000_000_000
	foreign := fmt.Sprintf("h%010d1", nonce)
	own := fmt.Sprintf("h%010d2", nonce)
	missing := fmt.Sprintf("h%010d3", nonce)
	pubSeedKungfu(t, pool, stranger, foreign, "Stranger memory")
	pubSeedKungfu(t, pool, publisher, own, "Own memory")

	c := pubContract("https://example.com/hook")
	c.HarnessRefs = []string{own, foreign, missing}

	// harness validation runs before the test delivery, so no live
	// receiver is needed here
	code := pubCreateForTest(t, pool, publisher, c, 2000)
	_, err := OpenTask(ctx, pool, publisher, code)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
	}
	items, _ := appErr.Details["errors"].([]map[string]string)
	if len(items) != 2 { // foreign ref + missing ref
		t.Fatalf("details.errors = %#v, want two harness_refs violations", appErr.Details)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskDraft {
		t.Fatalf("status = %s, want draft", tr.Status)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §4 update: paused edit → next open builds a new version --

func TestPublisherPauseUpdateOpenNewVersion(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open v1: %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	v1, err := repository.FindTaskVersion(ctx, pool, tr.ID, 1)
	if err != nil || v1 == nil {
		t.Fatalf("version 1: %v", err)
	}
	v1Contract, v1Harness := append([]byte{}, v1.Contract...), append([]byte{}, v1.Harness...)

	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	updated := pubContract(rcv.url)
	updated.Title = "Summarize a page, revised"
	if _, err := UpdateTask(ctx, pool, publisher, code, updated); err != nil {
		t.Fatalf("update while paused: %v", err)
	}
	view, err := OpenTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("open v2: %v", err)
	}
	if view["version"] != int32(2) {
		t.Fatalf("version = %v, want 2", view["version"])
	}

	tr, _ = repository.FindTaskByCode(ctx, pool, code)
	v1After, err := repository.FindTaskVersion(ctx, pool, tr.ID, 1)
	if err != nil || v1After == nil {
		t.Fatalf("version 1 after reopen: %v", err)
	}
	if string(v1After.Contract) != string(v1Contract) || string(v1After.Harness) != string(v1Harness) {
		t.Fatal("version 1 snapshot changed across the paused edit + reopen")
	}
	v2, err := repository.FindTaskVersion(ctx, pool, tr.ID, 2)
	if err != nil || v2 == nil {
		t.Fatalf("version 2: %v", err)
	}
	var v2Contract task.Contract
	if err := json.Unmarshal(v2.Contract, &v2Contract); err != nil {
		t.Fatalf("version 2 contract: %v", err)
	}
	if v2Contract.Title != "Summarize a page, revised" {
		t.Fatalf("version 2 title = %q, want the revised one", v2Contract.Title)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// TestPublisherPauseDraftVisibility (WO-17): a paused edit is visible —
// the task_update result and task_get carry the saved draft plus
// draft_pending while the effective contract stays on the opened
// version; the next open applies the draft as the new version.
// task_get also carries the §6.3 stats block (work_get's scope).
func TestPublisherPauseDraftVisibility(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open v1: %v", err)
	}
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}

	updated := pubContract(rcv.url)
	updated.Title = "Summarize a page, revised"
	updated.Requirements = "Revised requirements: five bullets instead."
	updView, err := UpdateTask(ctx, pool, publisher, code, updated)
	if err != nil {
		t.Fatalf("update while paused: %v", err)
	}
	if updView["draft_pending"] != true {
		t.Fatalf("task_update result draft_pending = %v, want true", updView["draft_pending"])
	}
	var updDraft task.Contract
	if err := json.Unmarshal(updView["draft"].(json.RawMessage), &updDraft); err != nil || updDraft.Title != updated.Title {
		t.Fatalf("task_update result draft: %+v (%v)", updDraft, err)
	}

	view, err := GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	if view["version"] != int32(1) {
		t.Fatalf("version = %v, want 1 (still the opened snapshot)", view["version"])
	}
	var live, draft task.Contract
	if err := json.Unmarshal(view["contract"].(json.RawMessage), &live); err != nil {
		t.Fatalf("live contract: %v", err)
	}
	if err := json.Unmarshal(view["draft"].(json.RawMessage), &draft); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if live.Title != "Summarize a page" || live.Requirements == updated.Requirements {
		t.Fatalf("live contract changed before reopen: %q", live.Title)
	}
	if draft.Title != updated.Title || draft.Requirements != updated.Requirements {
		t.Fatalf("draft is not the saved edit: %q", draft.Title)
	}
	if view["draft_pending"] != true {
		t.Fatalf("draft_pending = %v, want true", view["draft_pending"])
	}

	// task_get carries the §6.3 stats (accept_rate / median_reply_seconds
	// / failure_rate — null on a fresh task with no terminals)
	flat := map[string]any{}
	if err := json.Unmarshal(mustMarshalView(t, view), &flat); err != nil {
		t.Fatalf("re-unmarshal view: %v", err)
	}
	stats, ok := flat["stats"].(map[string]any)
	if !ok {
		t.Fatalf("task_get stats missing: %#v", flat["stats"])
	}
	for _, k := range []string{"accept_rate", "median_reply_seconds", "failure_rate"} {
		if _, ok := stats[k]; !ok {
			t.Fatalf("stats.%s missing: %#v", k, stats)
		}
	}

	openView, err := OpenTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("open v2: %v", err)
	}
	if openView["version"] != int32(2) {
		t.Fatalf("version = %v, want 2", openView["version"])
	}
	if _, has := openView["draft"]; has {
		t.Fatal("open view must not carry draft")
	}
	if openView["draft_pending"] != false {
		t.Fatalf("draft_pending after open = %v, want false", openView["draft_pending"])
	}
	var live2 task.Contract
	if err := json.Unmarshal(openView["contract"].(json.RawMessage), &live2); err != nil {
		t.Fatalf("v2 contract: %v", err)
	}
	if live2.Title != updated.Title || live2.Requirements != updated.Requirements {
		t.Fatalf("v2 contract is not the former draft: %q", live2.Title)
	}

	// paused again without editing: the draft equals the snapshot →
	// nothing pending
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause again: %v", err)
	}
	if view2, err := GetTask(ctx, pool, publisher, code); err != nil || view2["draft_pending"] != false {
		t.Fatalf("task_get after reopen: %v draft_pending=%v, want false", err, view2["draft_pending"])
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func mustMarshalView(t *testing.T, v map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	return b
}

// -- §4 close + refund --

func TestPublisherCloseAndRefund(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := CloseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("close: %v", err)
	}

	view, err := RefundTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if view["refunded"] != int64(2000) || view["available"] != int64(0) {
		t.Fatalf("refund view = %#v, want refunded 2000 available 0", view)
	}
	if got := ledgerSum(t, pool, publisher, "refund_task"); got != 2000 {
		t.Fatalf("refund_task ledger = %d, want 2000", got)
	}
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, publisher).Scan(&balance)
	if balance != 10_000 {
		t.Fatalf("balance = %d, want 10000 (fully refunded)", balance)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}

	// Second refund: nothing available → NOTHING_TO_REFUND.
	if _, err := RefundTask(ctx, pool, publisher, code); appErrOf(t, err).Code != "NOTHING_TO_REFUND" {
		t.Fatalf("second refund: %v, want NOTHING_TO_REFUND", err)
	}
}

func TestPublisherRefundHasReservations(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)

	// An active claim holds a reservation (§5.2 reserve).
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	now := time.Now()
	if _, err := repository.InsertClaim(ctx, tx, repository.NewClaimRow{
		TaskID: tr.ID, AgentID: agent, Version: 1,
		ExpiresAt: now.Add(30 * time.Minute), Deadline: now.Add(2 * time.Hour), Amount: 5,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := repository.ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := CloseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("close with reservation: %v", err)
	}
	_, err = RefundTask(ctx, pool, publisher, code)
	appErr := appErrOf(t, err)
	if appErr.Code != "HAS_RESERVATIONS" {
		t.Fatalf("code = %s, want HAS_RESERVATIONS", appErr.Code)
	}
	if got := ledgerSum(t, pool, publisher, "refund_task"); got != 0 {
		t.Fatalf("refund_task ledger = %d, want 0", got)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- ownership and state guards --

func TestPublisherNotOwner(t *testing.T) {
	pool := pubTestPool(t)
	owner := pubSeedBot(t, pool, 10_000)
	stranger := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	code := pubCreateForTest(t, pool, owner, pubContract("https://example.com/hook"), 2000)
	for name, fn := range map[string]func() error{
		"update": func() error {
			_, err := UpdateTask(ctx, pool, stranger, code, pubContract("https://example.com/hook"))
			return err
		},
		"open":   func() error { _, err := OpenTask(ctx, pool, stranger, code); return err },
		"pause":  func() error { _, err := PauseTask(ctx, pool, stranger, code); return err },
		"close":  func() error { _, err := CloseTask(ctx, pool, stranger, code); return err },
		"refund": func() error { _, err := RefundTask(ctx, pool, stranger, code); return err },
		"get":    func() error { _, err := GetTask(ctx, pool, stranger, code); return err },
	} {
		if err := fn(); appErrOf(t, err).Code != "NOT_OWNER" {
			t.Fatalf("%s by non-owner: %v, want NOT_OWNER", name, err)
		}
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestPublisherInvalidStateTransitions(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)

	// pause on draft → INVALID_STATE (details.status)
	_, err := PauseTask(ctx, pool, publisher, code)
	appErr := appErrOf(t, err)
	if appErr.Code != "INVALID_STATE" || appErr.Details["status"] != task.TaskDraft {
		t.Fatalf("pause draft: %v (%#v), want INVALID_STATE/draft", err, appErr.Details)
	}
	// update on open → INVALID_STATE
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("double open: %v, want INVALID_STATE", err)
	}
	if _, err := UpdateTask(ctx, pool, publisher, code, pubContract("https://example.com/hook")); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("update while open: %v, want INVALID_STATE", err)
	}
	// fund on closed → INVALID_STATE
	if _, err := CloseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := FundTask(ctx, pool, publisher, code, 100); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("fund closed: %v, want INVALID_STATE", err)
	}
	// refund on non-closed → INVALID_STATE
	code2 := pubCreateForTest(t, pool, publisher, pubContract("https://example.com/hook"), 2000)
	if _, err := RefundTask(ctx, pool, publisher, code2); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("refund open task: %v, want INVALID_STATE", err)
	}
	for _, c := range []string{code, code2} {
		tr, _ := repository.FindTaskByCode(ctx, pool, c)
		if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
			t.Fatalf("CheckInvariants(%s): %v", c, err)
		}
	}
}

// TestOpenTestDeliveryUniqueKeyPerAttempt: a receiver that caches by
// Idempotency-Key replays the first answer forever for that key. With
// the old fixed key test-<code>-<version>, a second open would replay
// the cached 500; with a unique key per attempt the second open
// succeeds.
func TestOpenTestDeliveryUniqueKeyPerAttempt(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	// a keyed cache: the first answer for a key is 500, every replay
	// of the SAME key returns the cached 500; a NEW key gets 200
	firstKey := ""
	var mu sync.Mutex
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		defer mu.Unlock()
		if firstKey == "" {
			// the very first request ever: answer 500 and cache it
			// for this key
			firstKey = key
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"receiver cold start"}`))
			return
		}
		if key == firstKey {
			// cached 500 replayed for the same key
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"receiver cold start (cached)"}`))
			return
		}
		// a different key: healthy answer
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pubTestTLSCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := pubContract(srv.URL)
	code := pubCreateForTest(t, pool, publisher, c, 1000)

	// first open fails: the receiver's very first request gets 500
	if _, err := OpenTask(ctx, pool, publisher, code); err == nil {
		t.Fatal("first open should fail (receiver cold start)")
	}
	// second open succeeds: the new unique key is not the cached one
	view, err := OpenTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("second open should succeed with a fresh key: %v", err)
	}
	if view["status"] != task.TaskOpen {
		t.Fatalf("status = %v, want open", view["status"])
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// TestPublisherStatsCounters (WO-18): task_get's stats carry the two
// publisher-only counters — submissions_30d (terminals inside the
// 30-day window) and active_claims (claims valid right now).
func TestPublisherStatsCounters(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusOK)
	now := time.Now()

	// claim.required so a claim survives the pause-free open flow
	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}

	// no activity yet: both counters zero
	view, err := GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	flat := map[string]any{}
	if err := json.Unmarshal(mustMarshalView(t, view), &flat); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	stats := flat["stats"].(map[string]any)
	if stats["submissions_30d"].(float64) != 0 || stats["active_claims"].(float64) != 0 {
		t.Fatalf("fresh stats: %#v", stats)
	}

	// one active claim → active_claims 1
	cv, err := ClaimTask(ctx, pool, agent, code, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// one settled submission through the real intake+delivery path,
	// carrying the claim (pubContract requires one)
	claimID := cv.ClaimID
	if _, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "wo18-stats-1",
		Payload: []byte(submitPayloadOK),
		ClaimID: &claimID,
	}, testAgentRefKey, now); err != nil {
		t.Fatalf("submit: %v", err)
	}

	view, err = GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	flat = map[string]any{}
	if err := json.Unmarshal(mustMarshalView(t, view), &flat); err != nil {
		t.Fatalf("re-unmarshal 2: %v", err)
	}
	stats = flat["stats"].(map[string]any)
	if stats["submissions_30d"].(float64) != 1 {
		t.Fatalf("submissions_30d = %v, want 1", stats["submissions_30d"])
	}
	if stats["active_claims"].(float64) != 0 {
		t.Fatalf("active_claims = %v, want 0 (the claim was used by the submission)", stats["active_claims"])
	}

	// a fresh claim counts again; an expired-but-unswept one does not
	if _, err := ClaimTask(ctx, pool, agent, code, now); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	view, err = GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	flat = map[string]any{}
	_ = json.Unmarshal(mustMarshalView(t, view), &flat)
	if got := flat["stats"].(map[string]any)["active_claims"].(float64); got != 1 {
		t.Fatalf("active_claims after fresh claim = %v, want 1", got)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// TestListTasksTotalIndependentOfPage (WO-19b): with 20 matching
// tasks, page 3 at page_size 10 is beyond the end — the page is empty
// but total stays 20 (a window COUNT would report 0).
func TestListTasksTotalIndependentOfPage(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 500)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		c := pubContract("")
		c.Title = fmt.Sprintf("WO-19b page fixture %02d", i)
		pubCreateForTest(t, pool, publisher, c, 5)
	}
	items, total, err := ListTasks(ctx, pool, publisher, TaskListFilter{Page: 3, PageSize: 10})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(items) != 0 || total != 20 {
		t.Fatalf("out-of-range page: %d items, total=%d, want 0/20", len(items), total)
	}
	items, total, err = ListTasks(ctx, pool, publisher, TaskListFilter{Page: 2, PageSize: 10})
	if err != nil || len(items) != 10 || total != 20 {
		t.Fatalf("page 2: err=%v %d items, total=%d, want 10/20", err, len(items), total)
	}
}
