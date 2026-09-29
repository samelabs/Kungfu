package service

// Synchronous delivery, reply mapping and settlement — spec §5.4,
// §7.1, §7.2, §7.3. DeliverSubmission takes a delivering (or uncertain)
// submission, performs ONE outbound POST outside any transaction, maps
// the reply per the §7.2 table, and writes the outcome — the reply
// record (status code + body), its SubmissionEvent, and the money move
// for settled / the reservation release for rejected / failed — in a
// single transaction that re-checks the pre-delivery state under the
// row lock. The uncertain recovery loop is task_recovery.go.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// receiverFaultThreshold is §7.3: five consecutive terminal failures
// platform-pause the task.
const receiverFaultThreshold = 5

// receiverFaultReason is the §7.3 paused_reason.
const receiverFaultReason = "RECEIVER_FAULT"

// maxReplyBodyBytes bounds the recorded reply body (§7.2: the first
// 4 000 bytes of the receiver's response, cut on a rune boundary).
const maxReplyBodyBytes = 4000

// AgentRef is the executor's stable anonymous identity inside one task
// (§7.1): hex(HMAC-SHA256(agentRefKey, "agent-ref:"+taskCode+":"+agentID))
// truncated to 16 hex characters. The same agent+task always yields the
// same value; different tasks diverge; the account id never appears.
func AgentRef(agentRefKey []byte, taskCode string, agentID int64) string {
	mac := hmac.New(sha256.New, agentRefKey)
	_, _ = mac.Write([]byte(fmt.Sprintf("agent-ref:%s:%d", taskCode, agentID)))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// DeliverSubmission delivers one submission synchronously and settles
// its outcome. Non-delivering/uncertain states return the current view
// unchanged (idempotent; no re-settlement).
func DeliverSubmission(ctx context.Context, pool *pg.Pool, submissionID int64, agentRefKey []byte, now time.Time) (SubmissionView, error) {
	// The caller's cancellation (an executor disconnecting mid-request)
	// must NOT abort the delivery or its outcome write: the POST and the
	// settlement run to completion on this detached context, bounded by
	// the HTTP client's 10s total timeout (§11), not by the caller. A
	// submission orphaned by a disconnect is picked up as delivering by
	// the recovery worker (§5.4). WithoutCancel also drops the caller's
	// deadline, so a fresh 30s ceiling replaces it — the recovery
	// worker's pass budget can never be escaped by an unbounded write.
	var deliverCancel context.CancelFunc
	ctx, deliverCancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer deliverCancel()
	sub, err := repository.FindSubmissionByID(ctx, pool, submissionID)
	if goerrors.Is(err, pgx.ErrNoRows) || sub == nil {
		return SubmissionView{}, errors.New(0, "SUBMISSION_NOT_FOUND", "Submission not found")
	}
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if sub.State != task.SubDelivering && sub.State != task.SubUncertain {
		return SubmissionViewByID(ctx, pool, submissionID)
	}

	t, err := repository.FindTaskByID(ctx, pool, sub.TaskID)
	if err != nil || t == nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	contract, err := func() (task.Contract, error) {
		var c task.Contract
		if err := json.Unmarshal(t.Contract, &c); err != nil {
			return c, err
		}
		return c, nil
	}()
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	// §7.1 request, delivered outside any transaction. submission_id
	// travels as the STRING the spec's request example shows ("…"), so
	// receivers typed against §7.1 parse it; the Idempotency-Key header
	// carries the same value.
	body, err := json.Marshal(map[string]json.RawMessage{
		"submission_id": json.RawMessage(fmt.Sprintf(`"%d"`, sub.SubmissionID)),
		"task_code":     json.RawMessage(`"` + t.Code + `"`),

		"agent_ref": json.RawMessage(`"` + AgentRef(agentRefKey, t.Code, sub.AgentID) + `"`),
		"payload":   sub.Payload,
	})
	if err != nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	res := delivery.PostJSON(ctx, contract.Receiver.URL, body, map[string]string{
		"Idempotency-Key": fmt.Sprintf("%d", sub.SubmissionID),
		"Kungfu-Task":     t.Code,
	}, delivery.AgentSubmitErrorConfig())

	outcome := mapReply(res)
	if outcome.event == task.EventTimeout && sub.State == task.SubUncertain {
		// another unresolved retry: stays uncertain without a state write
		return SubmissionViewByID(context.WithoutCancel(ctx), pool, submissionID)
	}

	var settle func(ctx context.Context, tx pgx.Tx) error
	var release func(ctx context.Context, tx pgx.Tx) error
	switch outcome.event {
	case task.EventDeliver2XX:
		settle = func(ctx context.Context, tx pgx.Tx) error {
			return repository.SettleTaskSubmission(ctx, pool, tx, sub.TaskID, sub.SubmissionID, sub.AgentID, sub.Amount)
		}
	case task.EventDeliver4XX, task.EventDeliveryFailed:
		release = func(ctx context.Context, tx pgx.Tx) error {
			return repository.ReleaseTaskReservation(ctx, tx, sub.TaskID, sub.Amount)
		}
	}
	if err := writeDeliveryOutcome(ctx, pool, sub, outcome.event, outcome.opts, settle, release); err != nil {
		return SubmissionView{}, err
	}

	// §7.3 receiver-fault governance. The outcome above is already
	// durable — a governance failure is logged, not returned: masking a
	// settled/rejected result with INTERNAL_ERROR would tell the caller
	// nothing new (the next counting failure re-runs the check).
	if outcome.event == task.EventDeliveryFailed {
		if err := maybePauseForReceiverFault(ctx, pool, sub.TaskID); err != nil {
			log.Printf("receiver-fault governance failed: task_id=%d submission_id=%d err=%v",
				sub.TaskID, sub.SubmissionID, err)
		}
	}
	return SubmissionViewByID(context.WithoutCancel(ctx), pool, submissionID)
}

// SubmissionViewByID loads a submission (and its task code) and
// projects the view.
func SubmissionViewByID(ctx context.Context, pool *pg.Pool, submissionID int64) (SubmissionView, error) {
	sub, err := repository.FindSubmissionByID(ctx, pool, submissionID)
	if err != nil || sub == nil {
		return SubmissionView{}, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	code := ""
	if t, err := repository.FindTaskByID(ctx, pool, sub.TaskID); err == nil && t != nil {
		code = t.Code
	}
	return newSubmissionView(sub, code), nil
}

// replyOutcome is the mapped §7.2 result.
type replyOutcome struct {
	event string
	opts  *repository.SetSubmissionStateOpts
}

// mapReply implements the §7.2 table. The status code alone decides
// the outcome; the body is recorded verbatim (bounded) and handed to
// the executor — the platform never parses or rewrites it.
func mapReply(res delivery.PostResult) replyOutcome {
	if res.ResponseCode != nil {
		code := *res.ResponseCode
		opts := &repository.SetSubmissionStateOpts{ResponseCode: &code, ResponseBody: replyBody(res.ResponseBody)}
		fail := func(reason string) replyOutcome {
			opts.Failure = &reason
			return replyOutcome{event: task.EventDeliveryFailed, opts: opts}
		}
		switch {
		case code >= 200 && code < 300:
			return replyOutcome{event: task.EventDeliver2XX, opts: opts}
		case code >= 400 && code < 500:
			return replyOutcome{event: task.EventDeliver4XX, opts: opts}
		case code >= 500:
			return fail("RECEIVER_FAULT")
		default: // 1xx / 3xx
			return fail("RECEIVER_PROTOCOL")
		}
	}
	if delivery.IsDefinitiveNotDelivered(res) {
		reason := "RECEIVER_UNREACHABLE" // 连接被拒 / DNS / SSRF 拦截
		return replyOutcome{event: task.EventDeliveryFailed,
			opts: &repository.SetSubmissionStateOpts{Failure: &reason}}
	}
	// 超时 / 连接中途断开 → uncertain（预留保持）
	return replyOutcome{event: task.EventTimeout}
}

// replyBody bounds a reply body to maxReplyBodyBytes, cut on a rune
// boundary with invalid UTF-8 replaced; nil stays nil.
func replyBody(body *string) *string {
	if body == nil {
		return nil
	}
	b := strings.ToValidUTF8(*body, "\uFFFD")
	if len(b) > maxReplyBodyBytes {
		cut := maxReplyBodyBytes
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		b = b[:cut]
	}
	return &b
}

// writeDeliveryOutcome applies one delivery outcome atomically: state +
// event via SetSubmissionState, plus the settlement or reservation
// release, under the task and submission row locks, only if the state did not
// change since delivery started (a concurrent writer wins and its
// outcome stands).
func writeDeliveryOutcome(ctx context.Context, pool *pg.Pool, pre *repository.SubmissionRow,
	event string, opts *repository.SetSubmissionStateOpts,
	settle, release func(ctx context.Context, tx pgx.Tx) error) error {

	// The delivery POST may have exhausted the caller's context (e.g. a
	// request deadline); the outcome write still MUST complete — but
	// bounded: WithoutCancel drops the deadline too, so a fresh 30s
	// ceiling keeps shutdown joins and DB hangs finite.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer writeCancel()
	tx, err := pool.TxBegin(writeCtx)
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	// Global lock order Task → Claim → Submission (WO-7c): the task row
	// first, since settlement / release move its counters. Locking the
	// submission first deadlocks against a concurrent intake that holds
	// the task lock and waits on this submission's unique key.
	if _, err := repository.FindTaskByIDForUpdate(writeCtx, tx, pre.TaskID); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	current, err := repository.FindSubmissionByIDForUpdate(writeCtx, tx, pre.SubmissionID)
	if err != nil || current == nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if current.State != pre.State {
		// someone else already wrote an outcome — keep theirs
		return nil
	}
	if err := repository.SetSubmissionState(writeCtx, tx, current.SubmissionID, pre.State, event, opts); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if settle != nil {
		if err := settle(writeCtx, tx); err != nil {
			return errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}
	if release != nil {
		if err := release(writeCtx, tx); err != nil {
			return errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}
	if err := tx.Commit(writeCtx); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return nil
}

// countingFailures are the §7.3 receiver-fault causes; DELIVERY_UNRESOLVED
// breaks the consecutive count instead of joining it.
var countingFailures = map[string]bool{
	"RECEIVER_PROTOCOL": true, "RECEIVER_FAULT": true, "RECEIVER_UNREACHABLE": true,
}

// maybePauseForReceiverFault is §7.3: with the Task row locked, the
// five most recent terminal submissions are re-read; only when ALL are
// failed with a counting receiver-fault reason does the platform pause
// the task with paused_reason = RECEIVER_FAULT (only from open).
func maybePauseForReceiverFault(ctx context.Context, pool *pg.Pool, taskID int64) error {
	govCtx := context.WithoutCancel(ctx)
	tx, err := pool.TxBegin(govCtx)
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := repository.FindTaskByIDForUpdate(govCtx, tx, taskID)
	if err != nil || t == nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t.Status != task.TaskOpen {
		return nil // already paused/closed — nothing to do
	}
	outcomes, err := repository.RecentTerminalOutcomes(govCtx, tx, taskID, receiverFaultThreshold)
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if len(outcomes) < receiverFaultThreshold {
		return nil
	}
	for _, o := range outcomes {
		if o.State != task.SubFailed || o.Failure == nil || !countingFailures[*o.Failure] {
			return nil
		}
	}
	reason := receiverFaultReason
	if err := repository.ApplyTaskStatus(govCtx, tx, taskID, task.TaskOpen, task.EventPlatformPause, &reason); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(govCtx); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return nil
}
