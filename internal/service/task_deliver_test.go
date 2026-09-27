package service

// Delivery / reply-mapping / settlement tests (WO-5a) — real
// PostgreSQL, TLS httptest receivers (the WO-2b test CA). Every test
// ends with task.CheckInvariants and asserts the event sequence.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"crypto/tls"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// progReceiver is a TLS httptest receiver whose response (status +
// body) is settable per test, capturing the last request.
type progReceiver struct {
	mu       sync.Mutex
	headers  http.Header
	body     []byte
	status   int
	respBody string
	sleep    time.Duration
	url      string
	keyHits  map[string]int // Idempotency-Key -> request count
}

func startProgReceiver(t *testing.T) *progReceiver {
	t.Helper()
	r := &progReceiver{status: http.StatusOK, respBody: `{"ok":true}`, keyHits: map[string]int{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		s, rb, sleep := r.status, r.respBody, r.sleep
		r.headers = req.Header.Clone()
		r.body = body
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			r.keyHits[k]++
		}
		r.mu.Unlock()
		if sleep > 0 {
			time.Sleep(sleep)
		}
		w.WriteHeader(s)
		_, _ = w.Write([]byte(rb))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pubTestTLSCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *progReceiver) set(t *testing.T, status int, body string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.respBody = status, body
}

func (r *progReceiver) last() (http.Header, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.headers.Clone(), r.body
}

// hitsFor is how many requests carried this Idempotency-Key.
func (r *progReceiver) hitsFor(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keyHits[key]
}

// deliverSyncTask opens a SYNC task whose receiver is the programmable
// one (test delivery at open gets a plain 200 first).
// deliverContract: the standard test contract against a receiver,
// claims optional (delivery tests submit without one).
func deliverContract(receiverURL string) task.Contract {
	c := pubContract(receiverURL)
	c.Claim = task.ClaimConfig{}
	return c
}

func deliverSyncTask(t *testing.T, pool *pg.Pool, publisher int64, rcv *progReceiver, budget int64) string {
	t.Helper()
	c := deliverContract(rcv.url)
	code := pubCreateForTest(t, pool, publisher, c, budget)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

func deliverSubmit(t *testing.T, pool *pg.Pool, agent int64, code string) (SubmissionView, int64) {
	t.Helper()
	view, err := SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("dl-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return view, view.SubmissionID.Int64()
}

func deliverTaskID(t *testing.T, pool *pg.Pool, code string) int64 {
	return mustTaskID(t, pool, code)
}

// assertEvents checks the submission's event chain from creation to
// the listed target states (§5.4: first event NULL→delivering).
func assertEvents(t *testing.T, pool *pg.Pool, subID int64, wantTo ...string) {
	t.Helper()
	events, err := repository.ListSubmissionEvents(context.Background(), pool, subID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != len(wantTo)+1 {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTo)+1, events)
	}
	if events[0].FromState != nil || events[0].ToState != task.SubDelivering || events[0].Cause != task.EventSubmit {
		t.Fatalf("first event = %+v, want NULL→delivering (submit)", events[0])
	}
	for i, want := range wantTo {
		e := events[i+1]
		if e.ToState != want || e.FromState == nil || *e.FromState != events[i].ToState {
			t.Fatalf("event %d = %+v, want %s→%s", i+1, e, events[i].ToState, want)
		}
	}
}

func deliverInvariants(t *testing.T, pool *pg.Pool, code string) {
	t.Helper()
	if err := task.CheckInvariants(context.Background(), pool, deliverTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §7.2: 2xx → settled; the reply reaches the executor --

func TestDeliver2xxSettles(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	rcv.set(t, http.StatusOK, `{"message":"great bullets"}`)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubSettled || view.Paid != 5 {
		t.Fatalf("view = %+v, want settled/paid 5", view)
	}
	if view.Reply == nil || view.Reply.Status != 200 || view.Reply.Body != `{"message":"great bullets"}` {
		t.Fatalf("reply = %+v, want the receiver's 200 and body verbatim", view.Reply)
	}
	// executor balance + amount; exactly one earn_task ledger row
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, agent).Scan(&balance)
	if balance != 5 {
		t.Fatalf("agent balance = %d, want 5", balance)
	}
	var n int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE bot_id=$1 AND type='earn_task' AND ref_type='task_submission' AND ref_id=$2`,
		agent, fmt.Sprint(subID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("earn_task rows = %d (err %v), want exactly 1", n, err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Settled != 5 || tr.Reserved != 0 {
		t.Fatalf("task counters settled=%d reserved=%d, want 5/0", tr.Settled, tr.Reserved)
	}
	// the payload is kept only until the submission is terminal
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.Payload != nil {
		t.Fatalf("payload kept after settlement: %s", sub.Payload)
	}
	assertEvents(t, pool, subID, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// §7.2: 202 is a 2xx → settled --

func TestDeliver202Settles(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	rcv.set(t, http.StatusAccepted, `stored`)
	view, subID := deliverSubmit(t, pool, agent, code)
	if view.State != task.SubSettled || view.Reply == nil || view.Reply.Status != 202 || view.Reply.Body != "stored" {
		t.Fatalf("view = %+v, want settled with reply 202/stored", view)
	}
	assertEvents(t, pool, subID, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// §7.2 + §10: every 4xx is a rejection and its body reaches the
// executor verbatim, whatever its format; the reservation is released.

func TestDeliver4xxRejectsWithReplyVerbatim(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusUnprocessableEntity, `{"accepted":false,"message":"第三个要点缺少来源","problems":[{"pointer":"/bullets/2"}]}`},
		{http.StatusBadRequest, `not json at all: bullet 3 has no source`},
		{http.StatusConflict, ``},
	} {
		rcv.set(t, tc.status, tc.body)
		view, subID := deliverSubmit(t, pool, agent, code)
		if view.State != task.SubRejected {
			t.Fatalf("%d: state = %s, want rejected", tc.status, view.State)
		}
		if view.Reply == nil || view.Reply.Status != tc.status || view.Reply.Body != tc.body {
			t.Fatalf("%d: reply = %+v, want the receiver's reply verbatim", tc.status, view.Reply)
		}
		// work_status returns the same recorded reply
		st, err := GetSubmissionStatus(ctx, pool, agent, &subID, "", "")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		reply, _ := st["reply"].(map[string]any)
		if reply == nil || reply["body"] != tc.body {
			t.Fatalf("%d: work_status reply = %v", tc.status, st["reply"])
		}
		assertEvents(t, pool, subID, task.SubRejected)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 (rejections release)", tr.Reserved)
	}
	deliverInvariants(t, pool, code)
}

// §7.2: the recorded body is the first 4 000 bytes, cut on a rune
// boundary.

func TestDeliverReplyBodyBounded(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	rcv.set(t, http.StatusBadRequest, "x"+strings.Repeat("驳", 2000)) // 1 + 6000 bytes
	view, _ := deliverSubmit(t, pool, agent, code)
	if view.Reply == nil || len(view.Reply.Body) > 4000 || !utf8.ValidString(view.Reply.Body) ||
		!strings.HasPrefix(view.Reply.Body, "x驳") {
		t.Fatalf("reply body = %d bytes (valid=%v), want ≤ 4000 valid UTF-8", len(view.Reply.Body), utf8.ValidString(view.Reply.Body))
	}
	deliverInvariants(t, pool, code)
}

// §7.2: 3xx → RECEIVER_PROTOCOL --

func TestDeliver3xxProtocol(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusFound, ``)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubFailed || *view.Failure != "RECEIVER_PROTOCOL" {
		t.Fatalf("view = %+v", view)
	}
	assertEvents(t, pool, subID, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// §7.2: 5xx → RECEIVER_FAULT --

func TestDeliver5xxFault(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusInternalServerError, `boom`)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubFailed || *view.Failure != "RECEIVER_FAULT" ||
		view.Reply == nil || view.Reply.Status != 500 || view.Reply.Body != "boom" {
		t.Fatalf("view = %+v", view)
	}
	tr, _ := repository.FindTaskByCode(context.Background(), pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// §7.2: connection refused (definitively not delivered) →
// RECEIVER_UNREACHABLE. The version snapshot is repointed at a closed
// local port after a normal open (test-only seeding). --

func TestDeliverConnectionRefused(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	// a port nothing listens on
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	c := deliverContract(rcv.url) // opens fine against the live receiver
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if _, err := pool.Exec(ctx, `
		UPDATE tb_task_version
		SET contract = replace(contract::text, $2, $3)::jsonb
		WHERE task_id = $1 AND version = 1`,
		tr.ID, rcv.url, fmt.Sprintf("https://127.0.0.1:%d/hook", deadPort)); err != nil {
		t.Fatalf("repoint receiver: %v", err)
	}

	view, subID := deliverSubmit(t, pool, agent, code)
	if view.State != task.SubFailed || *view.Failure != "RECEIVER_UNREACHABLE" {
		t.Fatalf("view = %+v, want failed/RECEIVER_UNREACHABLE", view)
	}
	assertEvents(t, pool, subID, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// §7.2: timeout (no usable response, not provably undelivered) →
// uncertain with the reservation held. The caller's context deadline
// bounds the wait (injectable timeout). --

func TestDeliverTimeoutUncertain(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens fast
	rcv.mu.Lock()
	rcv.sleep = 5 * time.Second
	rcv.mu.Unlock()

	// intake with a caller deadline shorter than the receiver's sleep
	deadlineCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	view, err := SubmitWork(deadlineCtx, pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("to-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	subID := view.SubmissionID.Int64()
	if view.State != task.SubUncertain {
		t.Fatalf("state = %s, want uncertain", view.State)
	}
	if view.Paid != 0 {
		t.Fatalf("paid = %d, want 0", view.Paid)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 5 {
		t.Fatalf("reserved = %d, want 5 (uncertain holds it)", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubUncertain)
	deliverInvariants(t, pool, code)
}

// §7.1 request shape: headers and body, agent_ref properties. --

func TestDeliverRequestShape(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	view, subID := deliverSubmit(t, pool, agent, code)
	if view.State != task.SubSettled {
		t.Fatalf("state = %s", view.State)
	}

	h, body := rcv.last()
	if h.Get("Idempotency-Key") != fmt.Sprint(subID) ||
		h.Get("Kungfu-Task") != code ||
		h.Get("Kungfu-Task-Version") != "1" {
		t.Fatalf("headers = Idempotency-Key=%q Kungfu-Task=%q Kungfu-Task-Version=%q",
			h.Get("Idempotency-Key"), h.Get("Kungfu-Task"), h.Get("Kungfu-Task-Version"))
	}
	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var got struct {
		SubmissionID string          `json:"submission_id"`
		TaskCode     string          `json:"task_code"`
		Version      int             `json:"version"`
		AgentRef     string          `json:"agent_ref"`
		Payload      json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if got.SubmissionID != fmt.Sprint(subID) || got.TaskCode != code || got.Version != 1 {
		t.Fatalf("body identity = %+v", got)
	}
	var wantPayload interface{}
	_ = json.Unmarshal([]byte(submitPayloadOK), &wantPayload)
	var gotPayload interface{}
	_ = json.Unmarshal(got.Payload, &gotPayload)
	if fmt.Sprint(wantPayload) != fmt.Sprint(gotPayload) {
		t.Fatalf("payload = %v", gotPayload)
	}

	// agent_ref: stable per (agent, task), differs across tasks. The
	// id can never be RECOVERED from it: it is a keyed HMAC (asserted
	// below by the exact derivation) — a decimal id happening to appear
	// as a substring inside the 16 hex chars is a coincidence, not a
	// leak, so no substring assertion here.
	ref := got.AgentRef
	if len(ref) != 16 {
		t.Fatalf("agent_ref = %q, want 16 hex chars", ref)
	}
	if AgentRef(testAgentRefKey, code, agent) != ref {
		t.Fatal("agent_ref is not the documented derivation")
	}
	other := AgentRef(testAgentRefKey, "othertaskcode", agent)
	if other == ref {
		t.Fatal("agent_ref identical across tasks")
	}
	if AgentRef([]byte("another-key"), code, agent) == ref {
		t.Fatal("agent_ref ignores the key")
	}
	deliverInvariants(t, pool, code)
}

// settled is idempotent: re-delivery does not settle twice. --

func TestDeliverIdempotentOnSettled(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	view, subID := deliverSubmit(t, pool, agent, code)
	if view.State != task.SubSettled {
		t.Fatalf("state = %s", view.State)
	}

	again, err := DeliverSubmission(ctx, pool, subID, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	if again.SubmissionID.Int64() != subID || again.State != task.SubSettled || again.Paid != 5 {
		t.Fatalf("re-deliver view = %+v", again)
	}
	var n int64
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type='earn_task' AND ref_type='task_submission' AND ref_id=$1`,
		fmt.Sprint(subID)).Scan(&n)
	if n != 1 {
		t.Fatalf("earn_task rows = %d, want still exactly 1", n)
	}
	// exactly two events (submit + settle): the idempotent re-delivery
	// wrote nothing
	assertEvents(t, pool, subID, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// §7.3: five consecutive terminal failures platform-pause the task. --

func TestDeliverFaultGovernance(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusInternalServerError, `down`)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		view, _ := deliverSubmit(t, pool, agent, code)
		if view.State != task.SubFailed {
			t.Fatalf("submission %d = %s", i, view.State)
		}
		tr, _ := repository.FindTaskByCode(ctx, pool, code)
		if tr.Status != task.TaskOpen {
			t.Fatalf("paused after %d failures, want still open", i+1)
		}
	}
	deliverSubmit(t, pool, agent, code) // the fifth
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskPaused || tr.PausedReason == nil || *tr.PausedReason != "RECEIVER_FAULT" {
		t.Fatalf("task = %s/%v, want paused/RECEIVER_FAULT", tr.Status, tr.PausedReason)
	}
	// a sixth submission now hits the paused guard
	_, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("after-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if appErrOf(t, err).Code != "TASK_NOT_OPEN" {
		t.Fatalf("post-pause submit: %v, want TASK_NOT_OPEN", err)
	}
	deliverInvariants(t, pool, code)
}

func TestDeliverFaultGovernanceBrokenBySettled(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	for i := 0; i < 4; i++ {
		rcv.set(t, http.StatusInternalServerError, `down`)
		deliverSubmit(t, pool, agent, code)
		if i == 1 {
			// a success breaks the streak
			rcv.set(t, http.StatusOK, `{"ok":true}`)
			if view, _ := deliverSubmit(t, pool, agent, code); view.State != task.SubSettled {
				t.Fatalf("streak breaker = %s", view.State)
			}
		}
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskOpen {
		t.Fatalf("task = %s, want open (streak was broken)", tr.Status)
	}
	deliverInvariants(t, pool, code)
}
