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

func pubCreateForTest(t *testing.T, pool *pg.Pool, publisher int64, c task.Contract, budget int64) string {
	t.Helper()
	view, err := CreateTask(context.Background(), pool, publisher, c, budget)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code, _ := view["code"].(string)
	return code
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
	if tr.Status != task.TaskPaused {
		t.Fatalf("status = %s, want draft", tr.Status)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §4 update: paused edit → next open builds a new version --

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
		TaskID: tr.ID, AgentID: agent,
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
	if appErr.Code != "INVALID_STATE" || appErr.Details["status"] != task.TaskPaused {
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
