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

func pubContract(receiverURL string) task.Contract {
	c := task.Contract{
		Title:     "Summarize a page",
		Objective: "Three bullets of the given page for a newsletter.",
		Inputs:    "A public URL fetched by the executor.",
		Output: task.Output{
			Description: "One JSON object with the bullets.",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"url": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
				},
				"required": ["url", "bullets"]
			}`),
		},
		Acceptance: task.Acceptance{
			Mode: task.ModeSync,
			Criteria: []task.Criterion{
				{ID: "C1", Kind: task.KindRule, Description: "exactly three bullets"},
				{ID: "C2", Kind: task.KindSchema, Description: "matches schema"},
			},
		},
		Examples: []task.Example{
			{Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), Accepted: true},
			{Payload: []byte(`{"url":"https://example.com/b","bullets":["one"]}`),
				Accepted: false, Criteria: []string{"C1"}},
		},
		Price: 5,
		Claim: task.ClaimConfig{Required: true},
	}
	if receiverURL != "" {
		c.Receiver = task.Receiver{URL: receiverURL}
	}
	return c
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

	_, err := CreateTask(context.Background(), pool, publisher, pubContract(""), 500)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
	}
	// budget below price, too
	_, err = CreateTask(context.Background(), pool, publisher, pubContract(""), 4)
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

func TestPublisherCreateInvalidContract(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)

	c := pubContract("")
	c.Title = "" // §3: title required
	c.Acceptance.Criteria = nil

	_, err := CreateTask(context.Background(), pool, publisher, c, 2000)
	appErr := appErrOf(t, err)
	if appErr.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
	}
	items, ok := appErr.Details["errors"].([]map[string]string)
	if !ok || len(items) < 2 {
		t.Fatalf("details.errors = %#v, want field errors for title and criteria", appErr.Details)
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
	if rcv.headers.Get("Idempotency-Key") != "test-"+code+"-1" ||
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
	if body.SubmissionID != "test-"+code+"-1" || body.TaskCode != code ||
		body.Version != 1 || body.AgentRef != "test" {
		t.Fatalf("body identity fields = %+v", body)
	}
	var wantPayload, gotPayload interface{}
	_ = json.Unmarshal([]byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), &wantPayload)
	_ = json.Unmarshal(body.Payload, &gotPayload)
	if fmt.Sprint(wantPayload) != fmt.Sprint(gotPayload) {
		t.Fatalf("payload = %v, want the first accepted example", gotPayload)
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

func TestPublisherOpenAsyncWithoutReceiverSkipsDelivery(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	c := pubContract("")
	c.Acceptance.Mode = task.ModeAsync
	window := int64(3600)
	c.Acceptance.ReviewWindow = &window
	c.Acceptance.Criteria = append(c.Acceptance.Criteria,
		task.Criterion{ID: "C3", Kind: task.KindJudgment, Description: "quality"})

	code := pubCreateForTest(t, pool, publisher, c, 2000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open (async, no receiver): %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskOpen || tr.Version != 1 {
		t.Fatalf("task = %s/v%d, want open/v1", tr.Status, tr.Version)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestPublisherOpenAsyncAccepts202(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()
	rcv := startPubReceiver(t, http.StatusAccepted)

	code := pubCreateForTest(t, pool, publisher, pubContract(rcv.url), 2000)
	tr0, _ := repository.FindTaskByCode(ctx, pool, code)
	var c task.Contract
	if err := json.Unmarshal(tr0.DraftContract, &c); err != nil {
		t.Fatalf("draft contract: %v", err)
	}
	window := int64(3600)
	c.Acceptance.Mode = task.ModeAsync
	c.Acceptance.ReviewWindow = &window
	if _, err := UpdateTask(ctx, pool, publisher, code, c); err != nil {
		t.Fatalf("update to async: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open (async 202): %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskOpen {
		t.Fatalf("status = %s, want open", tr.Status)
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

	// Second refund: nothing available → INVALID_STATE.
	if _, err := RefundTask(ctx, pool, publisher, code); appErrOf(t, err).Code != "INVALID_STATE" {
		t.Fatalf("second refund: %v, want INVALID_STATE", err)
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
