package mcpserver

// Thread tools (D2) — kungfu.md §6.1/§6.2/§6.5 room skeleton over the
// thread kernel service. Thin adapters: decode → service → envelope.
// Every write tool takes idempotency_key (L3); the envelope carries
// the PRD §2 error-code → next_action mapping (set here, because the
// thread codes are not in the shared NextAction table). The join
// failure limiter (§2: 20 failures / 15 min, surfaced uniformly as
// KEY_INVALID) is a package-local window in the tool layer.
//
import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
)

// idemKeyPattern is the idempotency-key grammar (same shape as the
// deployed request_key).
var idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// threadNextActionFor implements the PRD §2 mapping for the thread
// tool codes (the shared NextAction table does not know them).
func threadNextActionFor(code string) *string {
	switch code {
	case "THREAD_NOT_FOUND", "ASSIGN_NOT_FOUND", "NOT_MEMBER", "THREAD_CLOSED",
		"READ_ONLY", "KEY_INVALID", "MEMBER_LIMIT", "ROOM_LIMIT", "NOT_YOURS":
		s := "stop"
		return &s
	case "NOT_GOVERNOR", "LAST_MANAGER", "INVALID_TARGET", "IDEMPOTENCY_CONFLICT":
		s := "retry"
		return &s
	case "SUMMARY_REQUIRED", "CONTENT_TOO_LARGE", "SENSITIVE_CONTENT":
		s := "revise"
		return &s
	case "RATE_LIMIT":
		s := "wait"
		return &s
	}
	return nil
}

// threadErrResult keeps the service error and attaches the mapped
// next_action so the envelope can steer the caller.
func threadErrResult(err error) (ToolResult, error) {
	if err == nil {
		return ToolResult{}, nil
	}
	code := ""
	if ae, ok := apperr.IsAppError(err); ok {
		code = ae.Code
	} else if te, ok := err.(*ToolError); ok {
		code = te.Code
	}
	return ToolResult{Action: threadNextActionFor(code)}, err
}

func threadData(m map[string]any) (ToolResult, error) {
	return ToolResult{Data: m, NoAction: true}, nil
}

func threadIdemKey(raw string) (string, error) {
	if raw == "" {
		return "", nil // optional: no key, no replayable receipt
	}
	if !idemKeyPattern.MatchString(raw) {
		return "", argError("idempotency_key must be 1-128 characters of A-Za-z0-9._~-")
	}
	return raw, nil
}

// -- join failure window (§2: 20 failures / 15 min → KEY_INVALID) --

const (
	joinFailureLimit     = 20
	joinFailureWindowSec = 15 * 60
)

type joinFailureWindow struct {
	mu   sync.Mutex
	hits map[int64][]int64 // account → unix seconds of failed joins
}

var joinFailures = &joinFailureWindow{hits: map[int64][]int64{}}

func (w *joinFailureWindow) blocked(botID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := time.Now().Unix() - joinFailureWindowSec
	hits := w.hits[botID]
	kept := hits[:0]
	for _, ts := range hits {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	w.hits[botID] = kept
	return len(kept) >= joinFailureLimit
}

func (w *joinFailureWindow) record(botID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hits[botID] = append(w.hits[botID], time.Now().Unix())
}

// -- handlers --

func handleThreadStart(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Subject        string `json:"subject"`
		Key            bool   `json:"key"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, err
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_start") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_start").RetryAfter)
	}
	res, err := service.ThreadStart(ctx, deps.Pool, agent.ID, in.Subject, in.Key, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadKey(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		Role           string `json:"role"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadIssueKey(ctx, deps.Pool, agent.ID, in.Thread, in.Role, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadKeyRevoke(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadRevokeKey(ctx, deps.Pool, agent.ID, in.Thread, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadJoin(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Key            string `json:"key"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Key == "" {
		return ToolResult{}, argError("key is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	// §2: after 20 failed joins within 15 minutes every further join
	// fails with the same uniform KEY_INVALID (no key-validity leak).
	if joinFailures.blocked(agent.ID) {
		return threadErrResult(&ToolError{Code: "KEY_INVALID", Message: "Key is invalid"})
	}
	res, err := service.ThreadJoin(ctx, deps.Pool, agent.ID, in.Key, idemKey)
	if err != nil {
		if ae, ok := apperr.IsAppError(err); ok && ae.Code == "KEY_INVALID" {
			joinFailures.record(agent.ID)
		}
		return threadErrResult(err)
	}
	return threadData(res)
}

// -- D3: speaking (kungfu.md §6.3) --

// -- D4: assignments (kungfu.md §6.4) --

func handleAssignTake(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Assign         int64  `json:"assign"`
		Payload        string `json:"payload"`
		Memories       string `json:"memories"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Assign == 0 {
		return ToolResult{}, argError("assign is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.AssignTake(ctx, deps.Pool, agent.ID, in.Assign, in.Payload, in.Memories, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleAssignSubmit(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Assign         int64  `json:"assign"`
		Payload        string `json:"payload"`
		Memories       string `json:"memories"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Assign == 0 {
		return ToolResult{}, argError("assign is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.AssignSubmit(ctx, deps.Pool, agent.ID, in.Assign, in.Payload, in.Memories, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleAssignJudge(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Assign         int64  `json:"assign"`
		Verdict        string `json:"verdict"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Assign == 0 {
		return ToolResult{}, argError("assign is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.AssignJudge(ctx, deps.Pool, agent.ID, in.Assign, in.Verdict, in.Reason, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleAssignDrop(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Assign         int64  `json:"assign"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Assign == 0 {
		return ToolResult{}, argError("assign is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.AssignDrop(ctx, deps.Pool, agent.ID, in.Assign, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleAssignVoid(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Assign         int64  `json:"assign"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Assign == 0 {
		return ToolResult{}, argError("assign is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.AssignVoid(ctx, deps.Pool, agent.ID, in.Assign, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadPost(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string              `json:"thread"`
		Content        string              `json:"content"`
		Memory         string              `json:"memory"`
		Summary        string              `json:"summary"`
		ReplyTo        *int64              `json:"reply_to"`
		Ask            []int64             `json:"ask"`
		Assign         *service.AssignSpec `json:"assign"`
		IdempotencyKey string              `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	// encoding/json keeps ask tri-state: nil = absent (default rules
	// apply), [] = explicit "notify only" (§6.3 rule 1), [ids…] = named.
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadPost(ctx, deps.Pool, agent.ID, in.Thread,
		in.Content, in.Memory, in.Summary, in.ReplyTo, in.Ask, in.Assign, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadHandle(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		Entry          int64  `json:"entry"`
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" || in.Entry == 0 {
		return ToolResult{}, argError("thread and entry are required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadHandle(ctx, deps.Pool, agent.ID, in.Thread, in.Entry, in.Note, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadRetract(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		Entry          int64  `json:"entry"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" || in.Entry == 0 {
		return ToolResult{}, argError("thread and entry are required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadRetract(ctx, deps.Pool, agent.ID, in.Thread, in.Entry, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadLeave(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadLeave(ctx, deps.Pool, agent.ID, in.Thread, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadRemove(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string         `json:"thread"`
		Member         service.WireID `json:"member"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	if in.Member == 0 {
		return ToolResult{}, argError("member is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadRemoveMember(ctx, deps.Pool, agent.ID, in.Thread, in.Member.Int64(), idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadSetRole(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string         `json:"thread"`
		Member         service.WireID `json:"member"`
		Role           string         `json:"role"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	if in.Member == 0 || in.Role == "" {
		return ToolResult{}, argError("member and role are required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadSetRole(ctx, deps.Pool, agent.ID, in.Thread, in.Member.Int64(), in.Role, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadClose(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread         string `json:"thread"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	idemKey, err := threadIdemKey(in.IdempotencyKey)
	if err != nil {
		return ToolResult{}, err
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.ThreadClose(ctx, deps.Pool, agent.ID, in.Thread, idemKey)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Thread              string  `json:"thread"`
		Cursor              string  `json:"cursor"`
		Entries             []int64 `json:"entries"`
		AssignmentsCursor   string  `json:"assignments_cursor"`
		Assignments         []int64 `json:"assignments"`
		AssignmentsMineOpen bool    `json:"assignments_mine_open"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Thread == "" {
		return ToolResult{}, argError("thread is required")
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_read") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_read").RetryAfter)
	}
	res, err := service.ThreadGet(ctx, deps.Pool, agent.ID, in.Thread, in.Cursor, in.Entries, in.AssignmentsCursor, in.Assignments, in.AssignmentsMineOpen)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleThreadList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Status string `json:"status"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_read") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_read").RetryAfter)
	}
	res, err := service.ThreadList(ctx, deps.Pool, agent.ID, in.Status, in.Cursor)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleTodoList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	if !deps.limiter().CheckAgent(agent.ID, "thread_read") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_read").RetryAfter)
	}
	res, err := service.TodoList(ctx, deps.Pool, agent.ID, 0, in.Cursor)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleNotifyRegister(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := decodeArgs(args, &in); err != nil || in.URL == "" {
		return ToolResult{}, argError("url is required")
	}
	if !deps.limiter().CheckAgent(agent.ID, "notify_register") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "notify_register").RetryAfter)
	}
	res, err := service.NotifyRegister(ctx, deps.Pool, agent.ID, in.URL)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}

func handleNotifyDelete(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if !deps.limiter().CheckAgent(agent.ID, "thread_write") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "thread_write").RetryAfter)
	}
	res, err := service.NotifyDelete(ctx, deps.Pool, agent.ID)
	if err != nil {
		return threadErrResult(err)
	}
	return threadData(res)
}
