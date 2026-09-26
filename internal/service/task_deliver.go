package service

// Synchronous delivery, reply mapping and settlement — spec §5.4, §6.1,
// §7.1, §7.2, §7.3. DeliverSubmission takes a delivering (or uncertain)
// submission, performs ONE outbound POST outside any transaction, maps
// the reply per the §7.2 table, and writes the outcome — with its
// SubmissionEvent, and the money move for settled / the reservation
// release for rejected / failed — in a single transaction that
// re-checks the pre-delivery state under the row lock. The uncertain
// recovery loop, review-window timeouts and publisher verdicts are
// WO-5b.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"time"

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
	sub, err := repository.FindSubmissionByID(ctx, pool, submissionID)
	if goerrors.Is(err, pgx.ErrNoRows) || sub == nil {
		return SubmissionView{}, errors.New(404, "NOT_FOUND", "Submission not found")
	}
	if err != nil {
		return SubmissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if sub.State != task.SubDelivering && sub.State != task.SubUncertain {
		return SubmissionViewByID(ctx, pool, submissionID)
	}

	t, err := repository.FindTaskByID(ctx, pool, sub.TaskID)
	if err != nil || t == nil {
		return SubmissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	contract, err := versionContract(ctx, pool, t.ID, sub.Version)
	if err != nil {
		return SubmissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
	}

	// async without a receiver goes straight to review (§5.4).
	if contract.Acceptance.Mode == task.ModeAsync && contract.Receiver.URL == "" {
		if err := writeDeliveryOutcome(ctx, pool, sub, task.EventNoReceiver, &repository.SetSubmissionStateOpts{
			ReviewDeadline: reviewDeadline(now, contract),
		}, nil, nil); err != nil {
			return SubmissionView{}, err
		}
		return SubmissionViewByID(ctx, pool, submissionID)
	}

	// §7.1 request, delivered outside any transaction.
	body, err := json.Marshal(map[string]json.RawMessage{
		"submission_id": json.RawMessage(fmt.Sprintf(`%d`, sub.SubmissionID)),
		"task_code":     json.RawMessage(`"` + t.Code + `"`),
		"version":       json.RawMessage(fmt.Sprintf(`%d`, sub.Version)),
		"agent_ref":     json.RawMessage(`"` + AgentRef(agentRefKey, t.Code, sub.AgentID) + `"`),
		"payload":       sub.Payload,
	})
	if err != nil {
		return SubmissionView{}, errors.New(500, "INTERNAL_ERROR", "Internal error")
	}
	res := delivery.PostJSON(ctx, contract.Receiver.URL, body, map[string]string{
		"Idempotency-Key":     fmt.Sprintf("%d", sub.SubmissionID),
		"Kungfu-Task":         t.Code,
		"Kungfu-Task-Version": fmt.Sprintf("%d", sub.Version),
	}, delivery.AgentSubmitErrorConfig())

	outcome := mapReply(res, contract)
	if outcome.event == task.EventDeliver202 {
		outcome.opts.ReviewDeadline = reviewDeadline(now, contract)
	}
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

	// §7.3 receiver-fault governance.
	if outcome.event == task.EventDeliveryFailed {
		if err := maybePauseForReceiverFault(ctx, pool, sub.TaskID); err != nil {
			return SubmissionView{}, err
		}
	}
	return SubmissionViewByID(context.WithoutCancel(ctx), pool, submissionID)
}

// SubmissionViewByID loads a submission (and its task code) and
// projects the view.
func SubmissionViewByID(ctx context.Context, pool *pg.Pool, submissionID int64) (SubmissionView, error) {
	sub, err := repository.FindSubmissionByID(ctx, pool, submissionID)
	if err != nil || sub == nil {
		return SubmissionView{}, errors.New(500, "INTERNAL_ERROR", "Database error")
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

// mapReply implements the §7.2 table.
func mapReply(res delivery.PostResult, contract task.Contract) replyOutcome {
	failed := func(reason string) replyOutcome {
		return replyOutcome{event: task.EventDeliveryFailed,
			opts: &repository.SetSubmissionStateOpts{Failure: &reason}}
	}

	if res.ResponseCode != nil {
		code := *res.ResponseCode
		switch {
		case code == 202:
			if contract.Acceptance.Mode == task.ModeAsync {
				return replyOutcome{event: task.EventDeliver202, opts: &repository.SetSubmissionStateOpts{
					ReviewDeadline: nil, // filled by the caller with now+window
				}}
			}
			return failed("RECEIVER_PROTOCOL") // 202 于 sync 任务
		case code >= 200 && code < 300:
			verdict, _ := json.Marshal(task.Verdict{Accepted: true, Retryable: true, Source: "receiver"})
			return replyOutcome{event: task.EventDeliver2XX,
				opts: &repository.SetSubmissionStateOpts{Verdict: verdict}}
		case code >= 400 && code < 500:
			body := []byte{}
			if res.ResponseBody != nil {
				body = []byte(*res.ResponseBody)
			}
			if verdict, err := task.ParseVerdict(body, contract.Acceptance.Criteria); err == nil && !verdict.Accepted {
				verdict.Source = "receiver"
				raw, _ := json.Marshal(verdict)
				return replyOutcome{event: task.EventDeliver4XX,
					opts: &repository.SetSubmissionStateOpts{Verdict: raw}}
			}
			return failed("RECEIVER_PROTOCOL") // 4xx 且响应体不是有效驳回 Verdict
		case code >= 500:
			return failed("RECEIVER_FAULT")
		default: // 1xx / 3xx
			return failed("RECEIVER_PROTOCOL")
		}
	}
	if delivery.IsDefinitiveNotDelivered(res) {
		return failed("RECEIVER_UNREACHABLE") // 连接被拒 / DNS / SSRF 拦截
	}
	// 超时 / 连接中途断开 → uncertain（预留保持）
	return replyOutcome{event: task.EventTimeout}
}

// reviewDeadline is now + acceptance.review_window (present on valid
// async contracts).
func reviewDeadline(now time.Time, contract task.Contract) *time.Time {
	window := int64(0)
	if contract.Acceptance.ReviewWindow != nil {
		window = *contract.Acceptance.ReviewWindow
	}
	d := now.Add(time.Duration(window) * time.Second)
	return &d
}

// writeDeliveryOutcome applies one delivery outcome atomically: state +
// event via SetSubmissionState, plus the settlement or reservation
// release, under the submission row lock, only if the state did not
// change since delivery started (a concurrent writer wins and its
// outcome stands).
func writeDeliveryOutcome(ctx context.Context, pool *pg.Pool, pre *repository.SubmissionRow,
	event string, opts *repository.SetSubmissionStateOpts,
	settle, release func(ctx context.Context, tx pgx.Tx) error) error {

	// The delivery POST may have exhausted the caller's context (e.g. a
	// request deadline); the outcome write still MUST complete.
	writeCtx := context.WithoutCancel(ctx)
	tx, err := pool.TxBegin(writeCtx)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()

	current, err := repository.FindSubmissionByIDForUpdate(writeCtx, tx, pre.SubmissionID)
	if err != nil || current == nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if current.State != pre.State {
		// someone else already wrote an outcome — keep theirs
		return nil
	}
	if err := repository.SetSubmissionState(writeCtx, tx, current.SubmissionID, pre.State, event, opts); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if settle != nil {
		if err := settle(writeCtx, tx); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
	}
	if release != nil {
		if err := release(writeCtx, tx); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
	}
	if err := tx.Commit(writeCtx); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return nil
}

// maybePauseForReceiverFault is §7.3: when the task's five most recent
// terminal submissions are ALL failed, the platform pauses the task
// with paused_reason = RECEIVER_FAULT (only from open).
func maybePauseForReceiverFault(ctx context.Context, pool *pg.Pool, taskID int64) error {
	govCtx := context.WithoutCancel(ctx)
	states, err := repository.RecentTerminalStates(govCtx, pool, taskID, receiverFaultThreshold)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if len(states) < receiverFaultThreshold {
		return nil
	}
	for _, s := range states {
		if s != task.SubFailed {
			return nil
		}
	}
	tx, err := pool.TxBegin(govCtx)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := repository.FindTaskByIDForUpdate(govCtx, tx, taskID)
	if err != nil || t == nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if t.Status != task.TaskOpen {
		return nil // already paused/closed — nothing to do
	}
	reason := receiverFaultReason
	if err := repository.ApplyTaskStatus(govCtx, tx, taskID, task.TaskOpen, task.EventPlatformPause, &reason); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(govCtx); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return nil
}

// versionContract loads a task version's contract.
func versionContract(ctx context.Context, pool *pg.Pool, taskID int64, version int32) (task.Contract, error) {
	v, err := repository.FindTaskVersion(ctx, pool, taskID, version)
	if err != nil || v == nil {
		return task.Contract{}, err
	}
	var contract task.Contract
	if err := json.Unmarshal(v.Contract, &contract); err != nil {
		return task.Contract{}, err
	}
	return contract, nil
}
