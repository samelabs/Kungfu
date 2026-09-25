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

func deliverSubmit(t *testing.T, pool *pg.Pool, agent int64, code string) (submissionView, int64) {
	t.Helper()
	view, err := SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("dl-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return view, view.SubmissionID
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

// -- §7.2: 2xx → settled --

func TestDeliver2xxSettles(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubSettled || view.Paid != 5 {
		t.Fatalf("view = %+v, want settled/paid 5", view)
	}
	var av struct {
		Accepted  bool   `json:"accepted"`
		Source    string `json:"source"`
		Retryable bool   `json:"retryable"`
	}
	if view.Verdict == nil || json.Unmarshal(view.Verdict, &av) != nil ||
		!av.Accepted || av.Source != "receiver" || !av.Retryable {
		t.Fatalf("verdict = %s", view.Verdict)
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
	assertEvents(t, pool, subID, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// §7.2: 202 on async → under_review --

func TestDeliver202AsyncUnderReview(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	rcv.set(t, http.StatusAccepted, `{"ok":true}`)
	ctx := context.Background()

	c := deliverContract(rcv.url)
	c.Acceptance.Mode = task.ModeAsync
	w := int64(3600)
	c.Acceptance.ReviewWindow = &w
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	before := time.Now()
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubUnderReview {
		t.Fatalf("state = %s, want under_review", view.State)
	}
	if view.ReviewDeadline == nil || view.ReviewDeadline.Before(before.Add(3599*time.Second)) ||
		view.ReviewDeadline.After(before.Add(3601*time.Second)) {
		t.Fatalf("review_deadline = %v, want ≈ now+3600s", view.ReviewDeadline)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 5 {
		t.Fatalf("reserved = %d, want 5 (review holds the reservation)", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubUnderReview)
	deliverInvariants(t, pool, code)
}

// §7.2: 202 on sync → RECEIVER_PROTOCOL --

func TestDeliver202SyncProtocolError(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	rcv.set(t, http.StatusAccepted, `{}`) // 202 after a 200 open
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubFailed || view.Failure == nil || *view.Failure != "RECEIVER_PROTOCOL" {
		t.Fatalf("view = %+v, want failed/RECEIVER_PROTOCOL", view)
	}
	tr, _ := repository.FindTaskByCode(context.Background(), pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 (failure releases)", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// §7.2: 4xx with a valid rejecting Verdict → rejected --

func TestDeliver4xxValidVerdictRejected(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusUnprocessableEntity,
		`{"accepted":false,"criteria":["C1"],"reason":"第三个要点缺少来源","retryable":true,`+
			`"annotations":[{"pointer":"/bullets/2","criterion":"C1","message":"缺少来源"}]}`)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubRejected {
		t.Fatalf("state = %s, want rejected", view.State)
	}
	var v struct {
		Accepted bool                `json:"accepted"`
		Source   string              `json:"source"`
		Reason   string              `json:"reason"`
		Annot    []map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(view.Verdict, &v); err != nil || v.Accepted {
		t.Fatalf("verdict = %s (%v)", view.Verdict, err)
	}
	if v.Source != "receiver" || len(v.Annot) != 1 || v.Annot[0]["pointer"] != "/bullets/2" {
		t.Fatalf("verdict detail = %+v", v)
	}
	tr, _ := repository.FindTaskByCode(context.Background(), pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 (rejection releases)", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubRejected)
	deliverInvariants(t, pool, code)
}

// §7.2: 4xx whose body is not a valid Verdict → RECEIVER_PROTOCOL --

func TestDeliver4xxInvalidBodyProtocol(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusBadRequest, `not a verdict`)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubFailed || view.Failure == nil || *view.Failure != "RECEIVER_PROTOCOL" {
		t.Fatalf("view = %+v, want failed/RECEIVER_PROTOCOL", view)
	}
	if view.Verdict != nil {
		t.Fatalf("verdict recorded for a protocol failure: %s", view.Verdict)
	}
	assertEvents(t, pool, subID, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// §7.2: 4xx verdict citing an undeclared criterion → not a valid
// rejection → RECEIVER_PROTOCOL --

func TestDeliver4xxUndeclaredCriterion(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)

	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens on 200
	rcv.set(t, http.StatusBadRequest, `{"accepted":false,"criteria":["C9"],"reason":"nope"}`)
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubFailed || view.Failure == nil || *view.Failure != "RECEIVER_PROTOCOL" {
		t.Fatalf("view = %+v, want failed/RECEIVER_PROTOCOL", view)
	}
	assertEvents(t, pool, subID, task.SubFailed)
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

	if view.State != task.SubFailed || *view.Failure != "RECEIVER_FAULT" {
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
	c.Acceptance.Mode = task.ModeAsync
	w := int64(3600)
	c.Acceptance.ReviewWindow = &w
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
	subID := view.SubmissionID
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
		SubmissionID json.Number     `json:"submission_id"`
		TaskCode     string          `json:"task_code"`
		Version      int             `json:"version"`
		AgentRef     string          `json:"agent_ref"`
		Payload      json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if got.SubmissionID.String() != fmt.Sprint(subID) || got.TaskCode != code || got.Version != 1 {
		t.Fatalf("body identity = %+v", got)
	}
	var wantPayload interface{}
	_ = json.Unmarshal([]byte(submitPayloadOK), &wantPayload)
	var gotPayload interface{}
	_ = json.Unmarshal(got.Payload, &gotPayload)
	if fmt.Sprint(wantPayload) != fmt.Sprint(gotPayload) {
		t.Fatalf("payload = %v", gotPayload)
	}

	// agent_ref: stable per (agent, task), differs across tasks, and
	// never contains the agent id
	ref := got.AgentRef
	if len(ref) != 16 {
		t.Fatalf("agent_ref = %q, want 16 hex chars", ref)
	}
	if strings.Contains(ref, fmt.Sprint(agent)) {
		t.Fatal("agent_ref leaks the agent id")
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
	if again.SubmissionID != subID || again.State != task.SubSettled || again.Paid != 5 {
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

// §5.4: async without a receiver → under_review with the deadline. --

func TestDeliverAsyncNoReceiver(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 1000, nil) // async, no receiver, claims optional
	before := time.Now()
	view, subID := deliverSubmit(t, pool, agent, code)

	if view.State != task.SubUnderReview {
		t.Fatalf("state = %s, want under_review", view.State)
	}
	if view.ReviewDeadline == nil || view.ReviewDeadline.Before(before.Add(3599*time.Second)) {
		t.Fatalf("review_deadline = %v, want ≈ now+3600s", view.ReviewDeadline)
	}
	events, _ := repository.ListSubmissionEvents(ctx, pool, subID)
	if len(events) != 2 || events[1].Cause != task.EventNoReceiver {
		t.Fatalf("events = %+v, want submit + no_receiver", events)
	}
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
