package service

// Publisher verdict + listing tests (WO-5b). Every test ends with
// task.CheckInvariants.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

func verdictReviewTask(t *testing.T, pool *pg.Pool, publisher, agent int64) (string, int64) {
	t.Helper()
	rcv := startProgReceiver(t)
	code, subID := makeUnderReview(t, pool, publisher, agent, rcv)
	return code, subID
}

func TestVerdictAccept(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, subID := verdictReviewTask(t, pool, publisher, agent)
	view, err := SubmitVerdict(ctx, pool, publisher, subID,
		[]byte(`{"accepted":true}`), time.Now())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if view.State != task.SubSettled || view.Paid != 5 {
		t.Fatalf("view = %+v, want settled/paid", view)
	}
	if !containsAll(string(view.Verdict), `"source"`, `"publisher"`) {
		t.Fatalf("verdict = %s", view.Verdict)
	}
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, agent).Scan(&balance)
	if balance != 5 {
		t.Fatalf("agent balance = %d, want 5", balance)
	}
	assertEvents(t, pool, subID, task.SubUnderReview, task.SubSettled)
	deliverInvariants(t, pool, code)
}

func TestVerdictReject(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, subID := verdictReviewTask(t, pool, publisher, agent)
	view, err := SubmitVerdict(ctx, pool, publisher, subID,
		[]byte(`{"accepted":false,"criteria":["C1"],"reason":"缺来源","retryable":false}`), time.Now())
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if view.State != task.SubRejected || view.Paid != 0 {
		t.Fatalf("view = %+v, want rejected", view)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 (rejection releases)", tr.Reserved)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if !containsAll(string(sub.Verdict), `"retryable"`, `false`) {
		t.Fatalf("verdict = %s", sub.Verdict)
	}
	assertEvents(t, pool, subID, task.SubUnderReview, task.SubRejected)
	deliverInvariants(t, pool, code)
}

func TestVerdictInvalid(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, subID := verdictReviewTask(t, pool, publisher, agent)
	_, err := SubmitVerdict(ctx, pool, publisher, subID,
		[]byte(`{"accepted":false,"criteria":["C9"],"reason":"undeclared"}`), time.Now())
	appErr := appErrOf(t, err)
	if appErr.Code != "VERDICT_INVALID" || appErr.Details["message"] == nil {
		t.Fatalf("%v (%#v), want VERDICT_INVALID with details.message", err, appErr.Details)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubUnderReview {
		t.Fatalf("state = %s, want unchanged under_review", sub.State)
	}
	deliverInvariants(t, pool, code)
}

func TestVerdictNotOwner(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	stranger := pubSeedBot(t, pool, 0)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, subID := verdictReviewTask(t, pool, publisher, agent)
	if _, err := SubmitVerdict(ctx, pool, stranger, subID,
		[]byte(`{"accepted":true}`), time.Now()); appErrOf(t, err).Code != "NOT_OWNER" {
		t.Fatalf("%v, want NOT_OWNER", err)
	}
	deliverInvariants(t, pool, code)
}

func TestVerdictNotUnderReview(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// a settled submission (sync 2xx) is not under review
	rcv := startProgReceiver(t)
	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	view, _ := deliverSubmit(t, pool, agent, code)
	if view.State != task.SubSettled {
		t.Fatalf("setup state = %s", view.State)
	}
	_, err := SubmitVerdict(ctx, pool, publisher, view.SubmissionID,
		[]byte(`{"accepted":true}`), time.Now())
	appErr := appErrOf(t, err)
	if appErr.Code != "NOT_UNDER_REVIEW" || appErr.Details["state"] != task.SubSettled {
		t.Fatalf("%v (%#v), want NOT_UNDER_REVIEW/settled", err, appErr.Details)
	}
	deliverInvariants(t, pool, code)
}

func TestVerdictExpiredSettlesByTimeoutFirst(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code, subID := verdictReviewTask(t, pool, publisher, agent)
	// the verdict arrives after the review window closed (deadline is
	// submit-time + 3600s)
	_, err := SubmitVerdict(ctx, pool, publisher, subID,
		[]byte(`{"accepted":false,"criteria":["C1"],"reason":"too late"}`),
		time.Now().Add(2*time.Hour))
	appErr := appErrOf(t, err)
	if appErr.Code != "NOT_UNDER_REVIEW" || appErr.Details["state"] != task.SubSettled {
		t.Fatalf("%v (%#v), want NOT_UNDER_REVIEW/settled", err, appErr.Details)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled by timeout", sub.State)
	}
	if !containsAll(string(sub.Verdict), `"timeout"`) {
		t.Fatalf("verdict = %s, want source=timeout", sub.Verdict)
	}
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, agent).Scan(&balance)
	if balance != 5 {
		t.Fatalf("agent balance = %d, want 5 (timeout settlement)", balance)
	}
	assertEvents(t, pool, subID, task.SubUnderReview, task.SubSettled)
	deliverInvariants(t, pool, code)
}

func TestVerdictNotFound(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	_, err := SubmitVerdict(context.Background(), pool, publisher, 999999,
		[]byte(`{"accepted":true}`), time.Now())
	if appErrOf(t, err).Code != "SUBMISSION_NOT_FOUND" {
		t.Fatalf("%v, want SUBMISSION_NOT_FOUND", err)
	}
}

func TestListSubmissionsForPublisher(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	stranger := pubSeedBot(t, pool, 0)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// three submissions: settled (sync 2xx) + two under_review (202)
	rcv := startProgReceiver(t)
	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	settled, _ := deliverSubmit(t, pool, agent, code)

	rcv.mu.Lock()
	rcv.status = http.StatusAccepted
	rcv.mu.Unlock()
	c2, r1 := makeUnderReview(t, pool, publisher, agent, rcv)
	_ = c2

	rows, total, err := ListSubmissionsForPublisher(ctx, pool, publisher, code, "", 1, 10, testAgentRefKey)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].SubmissionID != settled.SubmissionID {
		t.Fatalf("list: rows=%d total=%d err=%v", len(rows), total, err)
	}
	if rows[0].AgentRef != AgentRef(testAgentRefKey, code, agent) {
		t.Fatalf("agent_ref = %s, want the delivery derivation", rows[0].AgentRef)
	}
	if rows[0].State != task.SubSettled || len(rows[0].Payload) == 0 {
		t.Fatalf("row = %+v", rows[0])
	}

	// state filter + pagination on the async task
	rv, rtotal, err := ListSubmissionsForPublisher(ctx, pool, publisher, c2, task.SubUnderReview, 1, 10, testAgentRefKey)
	if err != nil || rtotal != 1 || len(rv) != 1 || rv[0].SubmissionID != r1 {
		t.Fatalf("filtered: rows=%d total=%d err=%v", len(rv), rtotal, err)
	}

	// a second submission on the same task gives pagination something
	// to page (the receiver still answers 202)
	r2view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code:       c2,
		RequestKey: fmt.Sprintf("rev2-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("second review submission: %v", err)
	}
	r2 := r2view.SubmissionID
	page1, total2, _ := ListSubmissionsForPublisher(ctx, pool, publisher, c2, "", 1, 1, testAgentRefKey)
	page2, _, _ := ListSubmissionsForPublisher(ctx, pool, publisher, c2, "", 2, 1, testAgentRefKey)
	if total2 != 2 || len(page1) != 1 || len(page2) != 1 || page1[0].SubmissionID == page2[0].SubmissionID {
		t.Fatalf("pagination: total=%d p1=%v p2=%v", total2, page1, page2)
	}
	// newest first
	if page1[0].SubmissionID != r2 {
		t.Fatalf("order: first = %d, want %d (newest)", page1[0].SubmissionID, r2)
	}

	if _, _, err := ListSubmissionsForPublisher(ctx, pool, stranger, code, "", 1, 10, testAgentRefKey); appErrOf(t, err).Code != "NOT_OWNER" {
		t.Fatalf("%v, want NOT_OWNER", err)
	}
	deliverInvariants(t, pool, code)
	deliverInvariants(t, pool, c2)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
