package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// maxTaskResponseLogBytes caps how much of a task response body is persisted to the log.
const maxTaskResponseLogBytes = 4000

// ════════════════════════════════════════════════════════════════════
// Durable Task Submission — the single production authority.
//
// Fixed mechanism (PM-frozen):
//
//	1. SHORT tx: accept submission (snapshot + reserve budget) -> COMMIT
//	2. HTTP delivery happens OUTSIDE any DB transaction
//	3. durable remote outcome record (delivered / rejected / uncertain)
//	4. settlement in its own single atomic transaction
//	5. crash/timeout recovery from the durable submission rows
//
// Synchronous submission requests (MCP) and the recovery worker share the SAME
// primitives below; handlers and worker loops are protocol adapters only:
//
//	Submit / TestTaskDeliver — accept + drive processing synchronously
//	processSubmission        — claim/lease -> COMMIT -> HTTP outside tx
//	                           -> durable outcome -> settle/release
//	settleSubmission         — the ONLY settlement authority
//	RecoverPendingSubmissions — worker entry (same processSubmission)
//
// Network outcome classification (conservative, fail-safe):
//
//	SSRF/policy refusal, DNS failure, connect-phase failure
//	    -> definitive not-delivered (reservation released)
//	HTTP response received: 2xx = delivered; non-2xx = rejected
//	anything where the request MIGHT have reached the receiver
//	    -> uncertain (reservation retained, never auto-released)
// ════════════════════════════════════════════════════════════════════

// submissionLeaseDuration covers one HTTP delivery attempt (10s max)
// plus margin; an expired lease is how a crashed claimer's work gets
// picked back up by the recovery worker or a same-key retry.
const submissionLeaseDuration = 30 * time.Second

// submissionRetryBackoff is the simple, capped backoff for reserved rows
// the synchronous path left behind (crash between reserve and delivery).
const submissionRetryBackoff = 30 * time.Second

// requestKeyPattern is the client idempotency key syntax contract:
// 1-128 bytes, ASCII unreserved (A-Z a-z 0-9 . _ ~ -). No trimming —
// invalid forms are rejected, never silently normalized.
var requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// ValidateRequestKey enforces the client idempotency key contract at the
// single service boundary behind every caller.
func ValidateRequestKey(key string) error {
	if !requestKeyPattern.MatchString(key) {
		return errors.New(400, "INVALID_IDEMPOTENCY_KEY",
			"Idempotency-Key must be 1-128 ASCII characters from A-Z a-z 0-9 . _ ~ -")
	}
	return nil
}

// TaskSubmitResult is the synchronous submission response contract.
// State carries the durable submission state at response time; Billing is
// populated ONLY when state == settled — never fabricated.
type TaskSubmitResult struct {
	TaskCode     string                 `json:"task_code"`
	SubmissionID string                 `json:"submission_id"`
	State        string                 `json:"state"`
	Post         map[string]interface{} `json:"post"`
	Billing      map[string]interface{} `json:"billing,omitempty"`

	// ResponseBody is the receiver's response preview on the delivery
	// view (legacy TestTask API contract); masked only in DB sinks.
	ResponseBody string `json:"-"`
}

// Submit processes an agent task submission end-to-end: accept (short tx),
// then drive the durable delivery engine once synchronously. State tells
// the caller what durably happened:
//
//	settled   — delivered AND economically settled (billing populated)
//	uncertain — remote outcome unknown; same-key retry resumes the SAME
//	           submission (never a new one)
//	rejected  — definitive receiver rejection (error return carries it)
func Submit(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	requestKey string, input map[string]interface{}) (*TaskSubmitResult, error) {

	sub, outcome, err := acceptAndProcess(ctx, pool, taskCode, botID,
		repository.SubKindAgent, requestKey, input,
		delivery.AgentSubmitErrorConfig(), false)
	if err != nil {
		return nil, err
	}
	res := submitResult(sub)
	if outcome != nil && outcome.responseBody != "" {
		res.ResponseBody = outcome.responseBody
		if res.Post == nil {
			res.Post = map[string]interface{}{}
		}
		res.Post["response_body"] = outcome.responseBody
	}
	return res, nil
}

// TestTaskDeliver is the owner test path — SAME durable mechanism, owner
// economics (no earn_task). Only the settlement branch differs by kind.
func TestTaskDeliver(ctx context.Context, pool *pg.Pool, botID int64, taskCode string,
	requestKey string, input map[string]interface{}) (*TaskSubmitResult, error) {

	sub, outcome, err := acceptAndProcess(ctx, pool, taskCode, botID,
		repository.SubKindOwnerTest, requestKey, input,
		delivery.TestTaskErrorConfig(), true)
	if err != nil {
		return nil, err
	}
	res := submitResult(sub)
	if outcome != nil && outcome.responseBody != "" {
		res.ResponseBody = outcome.responseBody
		if res.Post == nil {
			res.Post = map[string]interface{}{}
		}
		res.Post["response_body"] = outcome.responseBody
	}
	return res, nil
}

// acceptAndProcess: shared entry for both kinds. Returns the freshest
// durable submission row for response projection.
func acceptAndProcess(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	kind, requestKey string, input map[string]interface{},
	errCfg delivery.ErrorConfig, ownerCheck bool) (*repository.SubmissionRow, *processOutcome, error) {

	if err := ValidateRequestKey(requestKey); err != nil {
		return nil, nil, err
	}

	// Canonical outbound bytes are produced ONCE here. Every retry (sync
	// retry or worker recovery) replays the persisted payload_body.
	payloadBytes := delivery.BuildSubmissionPayload(taskCode, input)
	payloadHash := hashPayload(payloadBytes)

	sub, created, err := acceptSubmission(ctx, pool, taskCode, botID, kind,
		requestKey, payloadHash, payloadBytes)
	if err != nil {
		return nil, nil, err
	}

	// Duplicate identity with a different payload is a contract violation.
	if !created && sub.PayloadHash != payloadHash {
		return nil, nil, errors.New(409, "IDEMPOTENCY_CONFLICT",
			"This Idempotency-Key was already used with a different payload for this task.")
	}

	// Terminal duplicates replay the durable fact — no new POST, no new
	// budget mutation, no new ledger row.
	if sub.State == repository.SubStateSettled || sub.State == repository.SubStateRejected {
		if sub.State == repository.SubStateRejected {
			// Replay the SAME durable rejection fact: the ORIGINAL
			// error code and response persisted on the row — never a
			// new generic semantic.
			errCode := ifEmpty(derefStr(sub.LastErrorCode), "TASK_POST_FAILED")
			return sub, nil, errors.NewWithDetails(424, errCode,
				"Task delivery failed. The submission was definitively rejected.",
				map[string]interface{}{
					"submission_id": sub.Code,
					"state":         repository.SubStateRejected,
					"post": map[string]interface{}{
						"delivered":     false,
						"response_code": derefInt(sub.ResponseCode),
					},
				})
		}
		return sub, nil, nil
	}

	// Non-terminal (reserved/uncertain/delivering): drive the SAME
	// processing primitive. The claim mechanism serializes this against
	// any concurrent request or worker on the same submission.
	pOutcome, pErr := processSubmission(ctx, pool, sub.ID, errCfg, true)
	if pErr != nil {
		// Business rejections (424) carry the submission identity; other
		// errors leave the row durable and recoverable — return the row
		// state alongside the error via the caller's error path.
		if ae, ok := errors.IsAppError(pErr); ok && ae.HTTPCode == 424 {
			return nil, nil, pErr
		}
		// Non-424 processing errors: report the durable state we have.
		fresh, ferr := repository.FindSubmissionByIdentity(ctx, pool, sub.TaskID, sub.BotID, sub.Kind, sub.ClientRequestKey)
		if ferr == nil && fresh != nil {
			return fresh, pOutcome, nil
		}
		return sub, pOutcome, nil
	}

	// The final state re-read must also survive a dying request ctx
	// (R21: cancelled request reporting its own uncertain outcome).
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer rcancel()
	fresh, err := repository.FindSubmissionByIdentity(rctx, pool, sub.TaskID, sub.BotID, sub.Kind, sub.ClientRequestKey)
	if err != nil || fresh == nil {
		return sub, pOutcome, nil
	}
	return fresh, pOutcome, nil
}

// acceptSubmission is the SHORT accept transaction:
//
//	BEGIN
//	lock task
//	-> duplicate identity lookup (UNIQUE converges a true race)
//	-> current task validation (status/postapi/price/available budget)
//	-> snapshot (postapi, price, payload bytes + hash)
//	-> INSERT submission (reserved) + reserved_budget += price
//	-> COMMIT
//
// No HTTP happens inside this transaction. Duplicate lookup runs UNDER
// the task lock, so a retry finds its own durable submission even if the
// task has since closed.
func acceptSubmission(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	kind, requestKey, payloadHash string, payloadBytes []byte) (*repository.SubmissionRow, bool, error) {

	for attempt := 0; attempt < 2; attempt++ {
		sub, created, err, retry := tryAccept(ctx, pool, taskCode, botID, kind, requestKey, payloadHash, payloadBytes)
		if retry {
			continue // UNIQUE race — converge by re-running under the lock
		}
		return sub, created, err
	}
	return nil, false, errors.New(500, "INTERNAL_ERROR", "Error creating submission")
}

func tryAccept(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	kind, requestKey, payloadHash string, payloadBytes []byte) (*repository.SubmissionRow, bool, error, bool) {

	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error retrieving task"), false
	}
	done := false
	defer func() {
		if !done {
			_ = pg.Rollback(tx)
		}
	}()

	task, err := repository.FindTaskWithReservedForUpdate(ctx, tx, taskCode)
	if err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error retrieving task"), false
	}
	if task == nil {
		// Deterministic 404 — no UNIQUE race to converge on.
		done = true
		_ = pg.Rollback(tx)
		return nil, false, errors.New(404, "NOT_FOUND", "Task not found"), false
	}

	// Duplicate identity FIRST (task lock held).
	existing, err := repository.FindSubmissionByIdentityForUpdate(ctx, tx, task.ID, botID, kind, requestKey)
	if err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error retrieving submission"), false
	}
	if existing != nil {
		done = true // read-only tx
		_ = pg.Rollback(tx)
		return existing, false, nil, false // duplicate: NOT created
	}

	postapi := ""
	if task.PostAPI != nil {
		postapi = strings.TrimSpace(*task.PostAPI)
	}

	// Kind-specific status gate (legacy contract preserved).
	if kind == repository.SubKindAgent {
		if task.Status != taskStatusOpen {
			done = true
			_ = pg.Rollback(tx)
			rule := RaiseRule("TASK_NOT_OPEN")
			insertTaskEventLog(ctx, pool, taskCode, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
			return nil, false, rule.ToAppError(), false
		}
	} else {
		if task.BotID != botID {
			return nil, false, errors.New(403, "NOT_OWNER", "Only the task owner can test this task"), false
		}
		if task.Status != taskStatusPending && task.Status != taskStatusOpen {
			return nil, false, errors.New(409, "TASK_NOT_OPEN", "Only pending or open tasks can be tested"), false
		}
	}
	if rule := ValidatePostapi(postapi, 2048); rule != nil {
		done = true
		_ = pg.Rollback(tx)
		insertTaskEventLog(ctx, pool, taskCode, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		return nil, false, rule.ToAppError(), false
	}
	if rule := ValidatePrice(task.Price); rule != nil {
		return nil, false, rule.ToAppError(), false
	}

	// Admission fundability: available = budget - reserved_budget.
	available := task.Budget - task.Reserved
	if !fundable(available, task.Price) {
		done = true
		_ = pg.Rollback(tx)
		rule := RaiseRule("TASK_BUDGET_EXHAUSTED")
		if kind == repository.SubKindAgent {
			insertTaskEventLog(ctx, pool, taskCode, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		}
		return nil, false, rule.ToAppError(), false
	}

	body := string(payloadBytes)
	sub := &repository.SubmissionRow{
		TaskID:           task.ID,
		TaskCode:         task.Code,
		BotID:            botID,
		Kind:             kind,
		ClientRequestKey: requestKey,
		PayloadHash:      payloadHash,
		PayloadBody:      &body,
		PostAPISnapshot:  postapi,
		PriceSnapshot:    task.Price,
		ReservedAmount:   task.Price,
		State:            repository.SubStateReserved,
	}
	sub.Code, err = GenerateUniqueSubmissionCode(ctx, tx)
	if err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error creating submission"), false
	}

	if err := repository.InsertSubmissionReserved(ctx, tx, sub); err != nil {
		if isUniqueViolation(err) {
			done = true
			_ = pg.Rollback(tx)
			return nil, false, nil, true // converge on retry
		}
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error creating submission"), false
	}
	// Capture the identity PK for the claim primitive (INSERT has no
	// RETURNING; the row is committed in this tx).
	if err := tx.QueryRow(ctx,
		`SELECT id FROM tb_task_submissions WHERE code = $1`, sub.Code).Scan(&sub.ID); err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error creating submission"), false
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, errors.New(500, "INTERNAL_ERROR", "Error creating submission"), false
	}
	done = true
	return sub, true, nil, false
}

// processSubmission is THE delivery engine, shared by the synchronous
// path and the recovery worker:
//
//	tx1: claim (state -> delivering, attempt++, lease) -> COMMIT
//	HTTP: outside any transaction, byte-identical replay with the stable
//	      Idempotency-Key (submission code)
//	tx2: durable outcome (delivered / rejected / uncertain) -> COMMIT
//	tx3 (2xx only): settlement — task mutation + earn_task + settled in
//	      one atomic COMMIT
//
// A crash anywhere leaves a row recovery can continue from; 'delivered'
// is durably written BEFORE settlement, so recovery settles WITHOUT
// another POST.
func processSubmission(ctx context.Context, pool *pg.Pool, submissionID int64,
	errCfg delivery.ErrorConfig, allowUncertainRetry bool) (*processOutcome, error) {

	// tx1: claim
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error claiming submission")
	}
	// allowUncertainRetry: the synchronous same-key retry may resume an
	// uncertain submission (client-driven, receiver idempotency key
	// protects the duplicate POST); the recovery worker never passes it.
	sub, err := repository.ClaimSubmissionForDelivery(ctx, tx, submissionID,
		time.Now().Add(submissionLeaseDuration), allowUncertainRetry)
	if err != nil {
		_ = pg.Rollback(tx)
		return nil, errors.New(500, "INTERNAL_ERROR", "Error claiming submission")
	}
	if sub == nil {
		// In flight elsewhere: report nothing changed.
		_ = pg.Rollback(tx)
		return &processOutcome{}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error claiming submission")
	}

	// Terminal-under-claim (a concurrent attempt's outcome committed
	// between our snapshot and claim): continue settlement if delivered.
	if sub.State == repository.SubStateDelivered || sub.State == repository.SubStateSettled {
		return settleIfPending(ctx, pool, sub)
	}

	// HTTP — outside any transaction. Byte-identical persisted payload to
	// the snapshot postapi with the stable receiver identity.
	result := delivery.PostJSONWithKey(ctx, sub.PostAPISnapshot,
		[]byte(derefStr(sub.PayloadBody)), sub.Code, errCfg)

	outcome := &processOutcome{}
	switch {
	case result.Success:
		// 2xx: durable 'delivered' FIRST — from here never POST again.
		outcome.delivered = true
		if err := recordDeliveredAndSettle(ctx, pool, sub, result); err != nil {
			return outcome, err
		}
		outcome.settled = true
		// Delivery view must be valid UTF-8 (raw bytes sanitized at the
		// presentation boundary only; the log sink masks + sanitizes
		// independently).
		outcome.responseBody = strings.ToValidUTF8(derefStr(result.ResponseBody), "")
		var loggedPayload map[string]interface{}
		if body := derefStr(sub.PayloadBody); body != "" {
			_ = json.Unmarshal([]byte(body), &loggedPayload)
		}
		insertTaskEventLog(ctx, pool, sub.TaskCode, sub.BotID, "post_succeeded", loggedPayload, true,
			result.ResponseCode, result.ResponseBody, "", "")
		return outcome, nil

	case result.ResponseCode != nil:
		// Definitive non-2xx: rejected + release reservation atomically.
		outcome.rejected = true
		stale, err := recordRejected(ctx, pool, sub, *result.ResponseCode, result.ResponseBody,
			ifEmpty(result.ErrorCode, "TASK_POST_FAILED"))
		if err != nil {
			return outcome, err
		}
		if stale {
			// This attempt's rejection was fenced off — a newer attempt
			// owns the submission. Project the CURRENT durable fact,
			// never the stale attempt's 424.
			return staleProjection(ctx, pool, sub, outcome)
		}
		return outcome, errors.NewWithDetails(424,
			ifEmpty(result.ErrorCode, "TASK_POST_FAILED"),
			"Task delivery failed. The submission was definitively rejected.",
			map[string]interface{}{
				"submission_id": sub.Code,
				"state":         repository.SubStateRejected,
				"post": map[string]interface{}{
					"delivered":     false,
					"response_code": *result.ResponseCode,
				},
			})

	default:
		// No reliable HTTP response: classify conservatively.
		//
		// The outcome write MUST outlive the request: a cancelled/timed-out
		// request is the canonical uncertain producer (deadline fires while
		// the receiver may hold the request). Use a detached context with a
		// bounded budget so the durable fact lands even as the caller's
		// ctx dies.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer dcancel()
		if delivery.IsDefinitiveNotDelivered(result) {
			po, perr := recordDefinitiveNotDelivered(dctx, pool, sub, result)
			if perr != nil && goerrors.Is(perr, errStaleProjection) {
				return staleProjection(dctx, pool, sub, outcome)
			}
			return po, perr
		}
		outcome.uncertain = true
		if err := recordUncertain(dctx, pool, sub, result); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
}

// errStaleProjection signals: this attempt's outcome write was fenced
// off; the caller must reread the durable row and project the CURRENT
// fact instead of the stale attempt's own result.
var errStaleProjection = goerrors.New("stale attempt: project current durable fact")

// staleProjection rereads the durable submission and projects its
// CURRENT fact. Called only after an ErrStaleAttempt fence: the newer
// attempt owns the state and may have delivered/settled/rejected.
func staleProjection(ctx context.Context, pool *pg.Pool,
	staleSub *repository.SubmissionRow, outcome *processOutcome) (*processOutcome, error) {
	current, err := repository.FindSubmissionByID(ctx, pool, staleSub.ID)
	if err != nil || current == nil {
		// Cannot reread: report a non-committal server error rather than
		// fabricating any business fact.
		return outcome, errors.New(500, "INTERNAL_ERROR", "Error reading submission state")
	}
	switch current.State {
	case repository.SubStateSettled, repository.SubStateDelivered:
		outcome.delivered = true
		outcome.settled = current.State == repository.SubStateSettled
		if current.State == repository.SubStateSettled {
			return outcome, nil // submitResult replays the settled fact
		}
		return outcome, nil // delivered; recovery settles; result replays delivered
	case repository.SubStateRejected:
		outcome.rejected = true
		errCode := "TASK_DELIVERY_FAILED"
		if current.LastErrorCode != nil {
			errCode = *current.LastErrorCode
		}
		return outcome, errors.NewWithDetails(424,
			errCode,
			"Task delivery failed. The submission was definitively rejected.",
			map[string]interface{}{
				"submission_id": current.Code,
				"state":         repository.SubStateRejected,
				"post": map[string]interface{}{
					"delivered":     false,
					"response_code": derefInt(current.ResponseCode),
				},
			})
	default:
		// reserved/delivering/uncertain under a newer attempt: in
		// flight elsewhere — nothing changed by THIS call.
		return &processOutcome{}, nil
	}
}

// processOutcome reports what the engine durably achieved.
type processOutcome struct {
	delivered    bool
	settled      bool
	rejected     bool
	uncertain    bool
	responseBody string // receiver response preview (success path)
}

// recordDeliveredAndSettle: tx2 (durable delivered) then tx3 (settlement).
// The gap between them is exactly the crash window recovery covers.
func recordDeliveredAndSettle(ctx context.Context, pool *pg.Pool,
	sub *repository.SubmissionRow, result delivery.PostResult) error {

	preview := previewOf(result.ResponseBody)
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery")
	}
	rc := 0
	if result.ResponseCode != nil {
		rc = *result.ResponseCode
	}
	if err := repository.RecordOutcomeDelivered(ctx, tx, sub.ID, sub.AttemptCount, rc, preview, time.Now()); err != nil {
		_ = pg.Rollback(tx)
		if goerrors.Is(err, repository.ErrStaleAttempt) {
			// A newer attempt owns this submission: our 2xx must not
			// rewrite it (e.g. settled back to delivered). No-op.
			return nil
		}
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery")
	}

	// Only the attempt that WON the delivered write performs settlement.
	_, err := SettleSubmissionByCode(ctx, pool, sub.Code)
	return err
}

// SettleSubmissionByCode is the ONLY settlement authority: looks up the
// row, locks TASK then submission (uniform lock order), performs the
// kind-appropriate atomic settlement:
//
//	agent:      budget -= reserved, reserved_budget -= reserved,
//	            earn_task +price (ref = submission code), settled facts
//	owner_test: same task mutations, NO earn_task
//
// Any commit failure leaves none of the three partially applied.
func SettleSubmissionByCode(ctx context.Context, pool *pg.Pool, submissionCode string) (*repository.SubmissionRow, error) {
	pre, err := repository.FindSubmissionByCode(ctx, pool, submissionCode)
	if err != nil || pre == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}

	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}
	done := false
	defer func() {
		if !done {
			_ = pg.Rollback(tx)
		}
	}()

	// Uniform lock order: TASK row first, then submission row.
	task, err := repository.FindTaskWithReservedForUpdate(ctx, tx, pre.TaskCode)
	if err != nil || task == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}
	sub, err := repository.FindSubmissionByCodeForUpdate(ctx, tx, submissionCode)
	if err != nil || sub == nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}

	if sub.State == repository.SubStateSettled {
		done = true
		_ = pg.Rollback(tx)
		return sub, nil // idempotent replay
	}
	if sub.State != repository.SubStateDelivered {
		done = true
		_ = pg.Rollback(tx)
		return sub, nil // not settleable now; recovery retries
	}

	// Task-side economic mutation (budget -= reserved, reserved_budget -=
	// reserved, existing auto-close rule on post-settlement state).
	if err := repository.DecrementTaskBudgetOnSettle(ctx, tx, task.ID, sub.ReservedAmount, MinOpenBudget); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling task budget")
	}

	var settledBalance int64
	if sub.Kind == repository.SubKindAgent {
		// earn_task in the SAME transaction — the settlement atomicity
		// point. ref_id is the stable submission code (audit anchor: one
		// earn_task row per settled submission).
		refType := "task_submission"
		refID := sub.Code
		settledBalance, err = credits.Record(ctx, pool, tx, sub.BotID, "earn_task",
			sub.PriceSnapshot, &refType, &refID)
		if err != nil {
			return nil, err
		}
	}

	var taskStatus string
	var budgetAfter int64
	if err := tx.QueryRow(ctx,
		`SELECT status, budget FROM tb_tasks WHERE id = $1`, task.ID).Scan(&taskStatus, &budgetAfter); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}

	if _, err := repository.SettleSubmission(ctx, tx, sub.ID, settledBalance, budgetAfter, taskStatus, time.Now()); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling submission")
	}
	done = true

	sub.State = repository.SubStateSettled
	sub.SettledBalance = &settledBalance
	sub.BudgetAfter = &budgetAfter
	sub.TaskStatusAfter = &taskStatus
	return sub, nil
}

// settleIfPending continues a delivered-but-unsettled row (crash window)
// — settles WITHOUT any POST.
func settleIfPending(ctx context.Context, pool *pg.Pool,
	sub *repository.SubmissionRow) (*processOutcome, error) {

	outcome := &processOutcome{delivered: true}
	if sub.State == repository.SubStateSettled {
		outcome.settled = true
		return outcome, nil
	}
	if _, err := SettleSubmissionByCode(ctx, pool, sub.Code); err != nil {
		return outcome, err
	}
	outcome.settled = true
	return outcome, nil
}

// recordRejected: rejected + reservation release, single transaction.
// Lock order: task then submission. Returns stale=true when THIS
// attempt's outcome write was fenced off (a newer attempt owns the
// submission): nothing was written and the caller must reread the
// durable row and project THAT fact — never the stale attempt's 424.
func recordRejected(ctx context.Context, pool *pg.Pool, sub *repository.SubmissionRow,
	responseCode int, responseBody *string, errCode string) (stale bool, err error) {

	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return false, errors.New(500, "INTERNAL_ERROR", "Error recording rejection")
	}
	preview := previewOf(responseBody)
	if _, err := repository.FindTaskWithReservedForUpdate(ctx, tx, sub.TaskCode); err != nil {
		_ = pg.Rollback(tx)
		return false, errors.New(500, "INTERNAL_ERROR", "Error recording rejection")
	}
	if err := repository.RecordOutcomeRejected(ctx, tx, sub.ID, sub.TaskID, sub.AttemptCount, responseCode, preview, errCode, ""); err != nil {
		_ = pg.Rollback(tx)
		if goerrors.Is(err, repository.ErrStaleAttempt) {
			return true, nil // stale attempt: no write, no release
		}
		return false, errors.New(500, "INTERNAL_ERROR", "Error recording rejection")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errors.New(500, "INTERNAL_ERROR", "Error recording rejection")
	}
	insertTaskEventLog(ctx, pool, sub.TaskCode, sub.BotID, "post_failed", nil, false,
		&responseCode, responseBody, errCode, "")
	return false, nil
}

// recordDefinitiveNotDelivered: provably-unsent request (SSRF refusal /
// DNS failure / connect failure) — rejected semantics WITHOUT pretending
// a receiver saw anything. Reservation released atomically.
func recordDefinitiveNotDelivered(ctx context.Context, pool *pg.Pool,
	sub *repository.SubmissionRow, result delivery.PostResult) (*processOutcome, error) {

	errCode := ifEmpty(result.ErrorCode, "TASK_POST_FAILED")
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error recording delivery failure")
	}
	if _, err := repository.FindTaskWithReservedForUpdate(ctx, tx, sub.TaskCode); err != nil {
		_ = pg.Rollback(tx)
		return nil, errors.New(500, "INTERNAL_ERROR", "Error recording delivery failure")
	}
	if err := repository.RecordOutcomeRejected(ctx, tx, sub.ID, sub.TaskID, sub.AttemptCount, 0, nil, errCode, ""); err != nil {
		_ = pg.Rollback(tx)
		if goerrors.Is(err, repository.ErrStaleAttempt) {
			// Stale attempt fenced off — project the current durable
			// fact (a newer attempt may already have delivered/settled).
			return nil, errStaleProjection
		}
		return nil, errors.New(500, "INTERNAL_ERROR", "Error recording delivery failure")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error recording delivery failure")
	}
	insertTaskEventLog(ctx, pool, sub.TaskCode, sub.BotID, "post_failed", nil, false,
		nil, nil, errCode, "")
	return &processOutcome{rejected: true}, errors.NewWithDetails(424, errCode,
		"Task delivery failed before reaching the receiver. The reservation was released; retry with the same Idempotency-Key.",
		map[string]interface{}{
			"submission_id": sub.Code,
			"state":         repository.SubStateRejected,
			"post": map[string]interface{}{
				"delivered": false,
			},
		})
}

// recordUncertain: unknown remote outcome. Reservation RETAINED, never
// auto-released. Same-key retry resumes THIS submission.
func recordUncertain(ctx context.Context, pool *pg.Pool,
	sub *repository.SubmissionRow, result delivery.PostResult) error {

	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery state")
	}
	if err := repository.RecordOutcomeUncertain(ctx, tx, sub.ID, sub.AttemptCount,
		ifEmpty(result.ErrorCode, "POSTAPI_UNCERTAIN"),
		normalizeTruncateUTF8(result.ErrorMessage, 400, true),
		time.Now().Add(submissionRetryBackoff)); err != nil {
		if goerrors.Is(err, repository.ErrStaleAttempt) {
			_ = pg.Rollback(tx)
			return nil // stale attempt: newer attempt owns the state
		}
		_ = pg.Rollback(tx)
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery state")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Error recording delivery state")
	}
	insertTaskEventLog(ctx, pool, sub.TaskCode, sub.BotID, "post_failed", nil, false,
		result.ResponseCode, nil, "POSTAPI_UNCERTAIN", "")
	return nil
}

// RecoverPendingSubmissions is the recovery-worker engine entry: it
// drives the SAME processSubmission/settle primitives for rows the
// synchronous path left behind:
//
//	reserved rows with an arrived next_attempt_at  -> deliver (first POST)
//	delivering rows with an EXPIRED lease           -> deliver (retry)
//	delivered rows not yet settled                  -> settle (NO POST)
//	uncertain rows                                  -> NEVER auto-retried
//
// The claim primitive (state-conditional UPDATE under row lock) provides
// mutual exclusion against in-flight synchronous requests and other
// workers; SKIP LOCKED keeps the scan itself contention-free.
func RecoverPendingSubmissions(ctx context.Context, pool *pg.Pool, limit int) error {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return txErr
	}
	ids, err := repository.ClaimRecoverableSubmissions(ctx, tx, limit)
	if err != nil {
		_ = pg.Rollback(tx)
		return fmt.Errorf("recovery scan: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		sub, ferr := repository.FindSubmissionByID(ctx, pool, id)
		if ferr != nil || sub == nil {
			continue
		}
		switch sub.State {
		case repository.SubStateDelivered:
			// Settle without POST.
			_, _ = SettleSubmissionByCode(ctx, pool, sub.Code)
		case repository.SubStateReserved, repository.SubStateDelivering:
			// Expired lease / stale reserve: drive the full engine. The
			// claim inside refuses while any live lease is held.
			cfg := delivery.AgentSubmitErrorConfig()
			if sub.Kind == repository.SubKindOwnerTest {
				cfg = delivery.TestTaskErrorConfig()
			}
			_, _ = processSubmission(ctx, pool, sub.ID, cfg, false)
		}
	}
	return nil
}

// submitResult builds the synchronous response from durable facts.
// Billing is populated ONLY for settled rows, replaying the settlement
// facts stored at settle time — never recomputed, never fabricated.
func submitResult(sub *repository.SubmissionRow) *TaskSubmitResult {
	res := &TaskSubmitResult{
		TaskCode:     sub.TaskCode,
		SubmissionID: sub.Code,
		State:        sub.State,
	}
	switch sub.State {
	case repository.SubStateSettled:
		var balance interface{}
		if sub.SettledBalance != nil {
			balance = *sub.SettledBalance
		}
		res.Post = map[string]interface{}{
			"delivered":     true,
			"response_code": derefInt(sub.ResponseCode),
		}
		if sub.Kind == repository.SubKindOwnerTest {
			// Legacy owner-test billing facts (cost/budget/status),
			// replayed from the durable settlement row.
			var budgetAfter interface{}
			if sub.BudgetAfter != nil {
				budgetAfter = *sub.BudgetAfter
			}
			var statusAfter interface{}
			if sub.TaskStatusAfter != nil {
				statusAfter = *sub.TaskStatusAfter
			}
			res.Billing = map[string]interface{}{
				"cost":   sub.PriceSnapshot,
				"budget": budgetAfter,
				"status": statusAfter,
			}
		} else {
			res.Billing = map[string]interface{}{
				"reward":  sub.PriceSnapshot,
				"balance": balance,
			}
		}
	case repository.SubStateRejected:
		res.Post = map[string]interface{}{
			"delivered":     false,
			"response_code": derefInt(sub.ResponseCode),
		}
	}
	return res
}

// --- helpers ---

func hashPayload(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// GenerateUniqueSubmissionCode returns a unique 12-hex submission code
// (same generator/shape as task codes; unpredictable, publicly safe).
func GenerateUniqueSubmissionCode(ctx context.Context, q pgx.Tx) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM tb_task_submissions WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

func previewOf(body *string) *string {
	if body == nil {
		return nil
	}
	p := normalizeTruncateUTF8(*body, maxTaskResponseLogBytes, true)
	return &p
}

// insertTaskEventLog writes a task delivery log entry (best-effort: the
// business flow is never failed by a log write, but failures are logged).
func insertTaskEventLog(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

	// Log-sink hardening: the persisted copies pass redaction BEFORE any
	// truncation — a secret spanning the truncation boundary would
	// otherwise survive as a partial unmasked key. The actual PostAPI
	// delivery payload/response is never touched.
	if payload != nil {
		payload = security.RedactSecrets(payload).(map[string]interface{})
	}
	if responseBody != nil {
		rb := normalizeTruncateUTF8(security.RedactSecrets(*responseBody).(string), maxTaskResponseLogBytes, true)
		responseBody = &rb
	}
	errorMessage = normalizeTruncateUTF8(security.RedactSecrets(errorMessage).(string), maxTaskResponseLogBytes, true)

	var payloadJSON *string
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err == nil {
			s := string(encoded)
			payloadJSON = &s
		}
	}

	if err := repository.InsertTaskLog(ctx, pool, repository.NewTaskLogInput{
		TaskCode:     taskCode,
		BotID:        &botID,
		Action:       action,
		PayloadJSON:  payloadJSON,
		ResponseCode: responseCode,
		ResponseBody: responseBody,
		Success:      success,
		ErrorCode:    strPtrOrNil(errorCode),
		ErrorMessage: strPtrOrNil(errorMessage),
	}); err != nil {
		log.Printf("task log write failed: task=%s action=%s err=%v", taskCode, action, err)
	}
}

// normalizeTruncateUTF8 normalizes invalid UTF-8, then truncates to the
// byte budget without cutting inside a rune. withMarker controls whether
// the "... [truncated]" suffix is appended.
func normalizeTruncateUTF8(value string, maxBytes int, withMarker bool) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	if withMarker {
		return value[:cut] + "... [truncated]"
	}
	return value[:cut]
}

// logOperation is a best-effort audit wrapper: the main business flow is
// never failed by an audit write, but a failed audit write is logged.
func logOperation(ctx context.Context, q pg.Querier, botID *int64, action string,
	targetType, targetID *string, requestData map[string]interface{}, success bool) {
	if err := repository.InsertOperationLog(ctx, q, repository.LogInsertData{
		BotID:       botID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		RequestData: requestData,
		Success:     success,
	}); err != nil {
		log.Printf("audit log write failed: action=%s target=%v err=%v", action, targetID, err)
	}
}

// Helper functions

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func strPtr(s string) *string { return &s }

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ifEmpty(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func derefInt(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
