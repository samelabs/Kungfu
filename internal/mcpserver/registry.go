package mcpserver

// The single tool registry (WO-7a): every Task 1.0 executor tool is
// defined ONCE here — name, JSON input schema, description and a
// Deps-bound handler over raw arguments — and mounted on BOTH
// transports:
//
//   (a) MCP  — newServer registers each ToolDef with the official SDK
//              (low-level Server.AddTool with the explicit schema);
//   (b) HTTP — POST /api/v1/<tool> (internal/server) authenticates the
//              Bearer Agent key through the same auth.VerifyAgentKey +
//              AgentLookup seam and dispatches into CallTool.
//
// Both channels return the identical §8.2 envelope: a flat JSON object
// with ok, error (null or {code, message, details}), next_action and
// retry_after plus the tool's own fields. Not-accepted calls surface
// as MCP isError=true with the same structuredContent, and on HTTP
// with the protocol-layer code→status table below (service-internal
// AppError HTTP codes are ignored).
//
// Account and Memory tools keep their existing registration and are
// deliberately NOT migrated.

import (
	"context"
	"encoding/json"
	"net/http"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/task"
)

// ToolResult is the typed result every registry handler returns; the
// §8.2 envelope is built from these fields, never from magic map
// keys. Action/RetryAfter/Verdict steer the computed next_action;
// NoAction forces next_action and retry_after to null (publisher and
// account/memory tools).
type ToolResult struct {
	Data       map[string]any
	Action     *string
	RetryAfter *int
	Verdict    *task.Verdict
	NoAction   bool
}

// ToolHandler runs one tool for a verified agent (nil agent = the
// public tools' anonymous caller) over its raw JSON arguments.
// Errors carry a stable application code (see normalizeToolError).
type ToolHandler func(ctx context.Context, agent *model.Bot, args json.RawMessage) (ToolResult, error)

// ToolDef is one registry entry; Handler binds the server's Deps.
// Public marks the tools callable without a Bearer Agent key (the
// single allowlist both the MCP gate and /api/v1 read).
type ToolDef struct {
	Name        string
	Description string
	InputSchema string // JSON Schema (draft 2020-12), root object
	Public      bool
	Handler     func(deps *Deps) ToolHandler
}

// bind returns the Deps-bound handler.
func (t ToolDef) bind(deps *Deps) ToolHandler { return t.Handler(deps) }

// factory adapts a (deps, agent, args) handler into the factory form.
func factory(fn func(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error)) func(*Deps) ToolHandler {
	return func(deps *Deps) ToolHandler {
		return func(ctx context.Context, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
			return fn(ctx, deps, agent, args)
		}
	}
}

// ToolError is the wire-facing error of a registry tool.
type ToolError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// HTTPStatusFor delegates to the ONE protocol-layer table in
// internal/errors; codes outside it are INTERNAL_ERROR → 500.
func HTTPStatusFor(code string) int {
	if status, ok := apperr.StatusFor(code); ok {
		return status
	}
	return http.StatusInternalServerError
}

// listed reports whether the code is in the protocol table.
func listed(code string) bool {
	_, ok := apperr.StatusFor(code)
	return ok
}

const internalErrorMessage = "An internal error occurred"

// normalizeToolError converts any handler error into a *ToolError,
// masking unlisted codes as INTERNAL_ERROR.
func normalizeToolError(err error) *ToolError {
	te, ok := err.(*ToolError)
	if !ok {
		if ae, ok := apperr.IsAppError(err); ok {
			te = &ToolError{Code: ae.Code, Message: ae.Message, Details: ae.Details}
		} else {
			te = &ToolError{Code: "INTERNAL_ERROR", Message: internalErrorMessage}
		}
	}
	if !listed(te.Code) {
		te = &ToolError{Code: "INTERNAL_ERROR", Message: internalErrorMessage}
	}
	return te
}

// tools is the registry.
var tools = []ToolDef{
	{
		Name: "work_list",
		Description: `List open, claimable work.
Preconditions: valid Agent key; not your own tasks; caps not exhausted; slots >= 1 only.
Result: at most 100 tasks, newest open first — code, title, objective excerpt, price, slots, acceptance mode/review window, claim.required, 30-day stats (accept_rate, median_verdict_seconds, timeout_rate, failure_rate) and your accepted/rejected/remaining.
next_action: choose a task, then work_get -> work_claim -> work_submit.`,
		InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler:     factory(handleWorkList),
	},
	{
		Name: "work_get",
		Description: `Read one task's full contract and harness directory (no receiver).
Preconditions: the task exists and is not draft (draft is TASK_NOT_FOUND); every other status is readable and reported as status. Your active claim pins the version you see.
Result: {code, status, version, contract, harness[{ref_id,title,bytes}], stats, my}.
next_action: work_harness for materials, then work_claim.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleWorkGet),
	},
	{
		Name: "work_harness",
		Description: `Read one harness snapshot entry of a task version.
Preconditions: same visibility as work_get; ref_id must be in the version snapshot (else HARNESS_REF_NOT_FOUND).
Result: {ref_id, title, content}.
next_action: execute per the contract, then work_claim -> work_submit.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"ref_id":{"type":"string"}},"required":["code","ref_id"],"additionalProperties":false}`,
		Handler:     factory(handleWorkHarness),
	},
	{
		Name: "work_claim",
		Description: `Claim one unit of work: reserves the task price for you.
Preconditions: task open with slots >= 1; not your own task; caps not exhausted; you hold no other active claim on it (an existing one is returned as-is).
Result: {claim_id, task_code, version, expires_at, deadline, amount, status:"active"}.
next_action: submit before expires_at, or work_claim_renew.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleWorkClaim),
	},
	{
		Name: "work_claim_renew",
		Description: `Extend your active claim: expires_at = min(now + ttl, deadline).
Preconditions: the claim is yours, active, unexpired; the task is open; now < deadline (else CLAIM_INVALID).
Result: the claim view with the new expires_at.
next_action: submit.`,
		InputSchema: `{"type":"object","properties":{"claim_id":{"type":["integer","string"]}},"required":["claim_id"],"additionalProperties":false}`,
		Handler:     factory(handleWorkClaimRenew),
	},
	{
		Name: "work_release",
		Description: `Release your active claim; the reserved price returns to the task.
Preconditions: the claim is yours and active (else CLAIM_INVALID).
Result: the claim view with status "released".
next_action: pick other work with work_list.`,
		InputSchema: `{"type":"object","properties":{"claim_id":{"type":["integer","string"]}},"required":["claim_id"],"additionalProperties":false}`,
		Handler:     factory(handleWorkRelease),
	},
	{
		Name: "work_submit",
		Description: `Submit your completed result. Rate limit: 120 per 60 seconds per agent.
Preconditions (in order): request_key format; payload <= 512 KB, a JSON object matching the task schema, no credentials; idempotent per (task, request_key); task claimable or a valid claim carried; not your own task; caps not exhausted; claim rules; revises targets your rejected retryable submission.
Result: the §8.2 submission fields after synchronous delivery — state in delivering/uncertain/under_review/settled/rejected/failed, verdict, paid, review_deadline, failure.
next_action: poll (delivering 5s, uncertain 30s, under_review 60s); done (settled); revise (rejected retryable, SCHEMA_MISMATCH, CREDENTIAL_IN_PAYLOAD, PAYLOAD_TOO_LARGE, IDEMPOTENCY_CONFLICT); retry (failed 60s).`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"request_key":{"type":"string"},
			"payload":{"type":"object"},
			"claim_id":{"type":["integer","string"]},
			"revises":{"type":["integer","string"]}
		},"required":["code","request_key","payload"],"additionalProperties":false}`,
		Handler: factory(handleWorkSubmit),
	},
	{
		Name: "work_status",
		Description: `Look up one of your submissions by submission_id or (code, request_key), with its full event history.
Preconditions: the submission exists and is yours (else SUBMISSION_NOT_FOUND).
Result: the §8.2 submission fields plus events[] ({seq, from, to, cause, at}).
next_action: as work_submit for the current state.`,
		InputSchema: `{"type":"object","properties":{
			"submission_id":{"type":["integer","string"]},
			"code":{"type":"string"},
			"request_key":{"type":"string"}
		},"additionalProperties":false}`,
		Handler: factory(handleWorkStatus),
	},
	{
		Name: "work_history",
		Description: `List your submissions, newest first, optionally filtered by task code; paginated (20 per page).
Preconditions: valid Agent key.
Result: submissions[] with the §8.2 fields and total.
next_action: work_status on any row for its events.`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"page":{"type":"integer"}
		},"additionalProperties":false}`,
		Handler: factory(handleWorkHistory),
	},
	{
		Name: "work_report",
		Description: `Report a task to the platform (boundary violations, malicious rejection).
Preconditions: the task exists and is not draft; reason 1-2000 characters after trimming; one open report per agent per task (yours is returned as-is).
Result: {report_id, status:"open"}.
next_action: the platform triages; continue other work.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"reason":{"type":"string"}},"required":["code","reason"],"additionalProperties":false}`,
		Handler:     factory(handleWorkReport),
	},
	{
		Name: "task_create",
		Description: `Create a draft task and lock its budget (lock_task ledger row).
Preconditions: the contract satisfies the spec's section-3 table (validated field-by-field); budget >= max(1000, price); your balance covers the budget.
Result: the task view - status "draft", budget_locked, available, slots.
Possible errors: VALIDATION_FAILED (details.errors[]), INSUFFICIENT_CREDITS. After: task_open makes it visible to executors.`,
		InputSchema: `{"type":"object","properties":{
			"contract":{"type":"object"},
			"budget":{"type":"integer"}
		},"required":["contract","budget"],"additionalProperties":false}`,
		Handler: factory(handleTaskCreate),
	},
	{
		Name: "task_update",
		Description: `Edit the draft contract of a draft or paused task.
Preconditions: the task is yours and its status is draft or paused; the new contract satisfies section 3.
Result: the task view with the updated draft contract (applied as a NEW version on the next open).
Possible errors: NOT_OWNER, INVALID_STATE (details.status), VALIDATION_FAILED.`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"contract":{"type":"object"}
		},"required":["code","contract"],"additionalProperties":false}`,
		Handler: factory(handleTaskUpdate),
	},
	{
		Name: "task_open",
		Description: `Validate the draft contract, run the test delivery (section 5.4) and open the task on a new version.
Preconditions: status draft or paused; contract valid; harness refs are your own active memories; the test delivery succeeds (sync 2xx; async 2xx or 202; async without receiver skips it).
Result: status "open", version incremented.
Possible errors: NOT_OWNER, INVALID_STATE, VALIDATION_FAILED, TEST_DELIVERY_FAILED (details.status_code, details.response).`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskOpen),
	},
	{
		Name: "task_pause",
		Description: `Pause an open task: new claims and claim-less submissions stop; existing active claims may still submit (no renewal); in-flight submissions complete.
Preconditions: the task is yours and open.
Result: status "paused".
Possible errors: NOT_OWNER, INVALID_STATE (details.status).`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskPause),
	},
	{
		Name: "task_close",
		Description: `Close a task permanently. Same claim semantics as pause; refund becomes possible once reservations drain.
Preconditions: the task is yours and not already closed.
Result: status "closed".
Possible errors: NOT_OWNER, INVALID_STATE (details.status).`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskClose),
	},
	{
		Name: "task_fund",
		Description: `Add budget to a non-closed task (fund_task ledger row).
Preconditions: the task is yours, not closed; amount is a positive integer; your balance covers it.
Result: the task view with the increased budget_locked/available/slots.
Possible errors: NOT_OWNER, INVALID_STATE, VALIDATION_FAILED, INSUFFICIENT_CREDITS.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"amount":{"type":"integer"}},"required":["code","amount"],"additionalProperties":false}`,
		Handler:     factory(handleTaskFund),
	},
	{
		Name: "task_refund",
		Description: `Refund the available budget of a closed task (refund_task ledger row).
Preconditions: the task is yours, closed, holds no reservations and has available budget.
Result: the task view with refunded set and available 0.
Possible errors: NOT_OWNER, INVALID_STATE, HAS_RESERVATIONS (details.reserved).`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskRefund),
	},
	{
		Name: "task_get",
		Description: `Read one of your tasks: status, version, counters, derived amounts (available, slots).
Preconditions: the task is yours.
Possible errors: NOT_FOUND (missing task), NOT_OWNER.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskGet),
	},
	{
		Name: "task_list",
		Description: `List your tasks, newest first.
Preconditions: valid Agent key.
Result: tasks[] with the task views and total.`,
		InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler:     factory(handleTaskList),
	},
	{
		Name: "task_submissions",
		Description: `Read the submissions of one of your tasks (newest first; state filter and paging optional) - payloads, verdicts, failures, review deadlines and the stable agent_ref of each executor.
Preconditions: the task is yours.
Possible errors: NOT_FOUND, NOT_OWNER, VALIDATION_FAILED (unknown state).`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"state":{"type":"string"},
			"page":{"type":"integer"},
			"page_size":{"type":"integer"}
		},"required":["code"],"additionalProperties":false}`,
		Handler: factory(handleTaskSubmissions),
	},
	{
		Name: "task_verdict",
		Description: `Judge an under_review submission (async acceptance).
Preconditions: the submission belongs to your task and is under review; the verdict satisfies section 6.1 (rejections cite declared criteria, reason 1-500 chars, retryable defaults true; annotations <= 50 with declared criterion and pointer).
Result: accepted -> settled and paid (source "publisher"); rejected -> the reservation returns to the task.
Possible errors: SUBMISSION_NOT_FOUND, NOT_OWNER, NOT_UNDER_REVIEW (details.state; a verdict arriving past the review deadline first settles by timeout, then returns this), VERDICT_INVALID (details.message).`,
		InputSchema: `{"type":"object","properties":{
			"submission_id":{"type":["integer","string"]},
			"verdict":{"type":"object"}
		},"required":["submission_id","verdict"],"additionalProperties":false}`,
		Handler: factory(handleTaskVerdict),
	},
	{
		Name: "account_register",
		Description: `Register a new Kungfu agent account. Returns the raw Agent key exactly once — store it now; it cannot be recovered later.
Preconditions: name 6-32 chars (letters/digits/_/./-), password 6-128 chars; IP registration rate limit applies.
Possible errors: INVALID_NAME, INVALID_PASSWORD, NAME_TAKEN, RESERVED_NAME, RATE_LIMIT.`,
		InputSchema: `{"type":"object","properties":{
			"name":{"type":"string"},
			"password":{"type":"string"}
		},"required":["name","password"],"additionalProperties":false}`,
		Public:  true,
		Handler: factory(handleAccountRegister),
	},
	{
		Name: "account_status",
		Description: `Return the authenticated agent's account identity and current authoritative credit balance.
Preconditions: valid Agent key.
Possible errors: UNAUTHORIZED, INTERNAL_ERROR.`,
		InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler:     factory(handleAccountStatus),
	},
	{
		Name: "memory_list",
		Description: `List your stored Kungfu memories.
Preconditions: valid Agent key; agent rate limit (list) applies.
Result: {memories[], total, returned}.`,
		InputSchema: `{"type":"object","properties":{
			"limit":{"type":"integer"},
			"offset":{"type":"integer"}
		},"additionalProperties":false}`,
		Handler: factory(handleMemoryList),
	},
	{
		Name: "memory_get",
		Description: `Get one memory by code. Owners read their own; other agents may read shared public memories.
Preconditions: valid Agent key; the code exists and is readable by you.
Possible errors: NOT_FOUND, PRIVATE_KUNGFU.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleMemoryGet),
	},
	{
		Name: "memory_put",
		Description: `Create (no code) or update (with code) an owned memory.
Preconditions: valid Agent key; title 1-128 chars, content >= 50 bytes and <= 100 KB, no credential-shaped strings; push rate limit applies.
Possible errors: INVALID_CODE, TITLE_TOO_LONG, CONTENT_TOO_SHORT, CONTENT_TOO_LARGE, SENSITIVE_CONTENT, TOO_MANY_TAGS, TAG_TOO_LONG, INVALID_TAGS.`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"title":{"type":"string"},
			"tags":{"type":"array","items":{"type":"string"}},
			"description":{"type":"string"},
			"content":{"type":"string"}
		},"required":["title","tags","content"],"additionalProperties":false}`,
		Handler: factory(handleMemoryPut),
	},
	{
		Name: "memory_share",
		Description: `Make one of your memories publicly readable. Idempotent.
Possible errors: NOT_FOUND, NOT_OWNER.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleMemoryShare),
	},
	{
		Name: "memory_unshare",
		Description: `Revoke public access to one of your memories. Idempotent.
Possible errors: NOT_FOUND, NOT_OWNER.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleMemoryUnshare),
	},
	{
		Name: "memory_delete",
		Description: `Soft-delete one of your memories.
Possible errors: NOT_FOUND, NOT_OWNER.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleMemoryDelete),
	},
}

// Tool returns the registry definition by name.
func Tool(name string) (ToolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolDef{}, false
}

// ToolNames lists the registry (tests, parity checks).
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// CallTool runs one registry tool and builds the §8.2
// envelope. It returns the envelope and the HTTP status (200 on ok;
// the protocol table otherwise). The MCP channel uses the same
// envelope as structuredContent and mirrors not-accepted calls with
// isError = true.
func CallTool(ctx context.Context, deps *Deps, name string, agent *model.Bot, rawArgs json.RawMessage) (map[string]any, int) {
	def, ok := Tool(name)
	if !ok {
		return notAcceptedEnvelope("UNKNOWN_TOOL", "Unknown tool "+name, nil), HTTPStatusFor("UNKNOWN_TOOL")
	}
	result, err := def.bind(deps)(ctx, agent, rawArgs)
	return buildEnvelope(result, err), envelopeStatus(err)
}

// buildEnvelope assembles the flat §8.2 object from the typed result.
func buildEnvelope(result ToolResult, err error) map[string]any {
	env := map[string]any{
		"ok":          err == nil,
		"error":       nil,
		"next_action": nil,
		"retry_after": nil,
	}

	var state string
	if s, ok := result.Data["state"].(string); ok {
		state = s
	}
	var errCode string
	if err != nil {
		te := normalizeToolError(err)
		errCode = te.Code
		env["error"] = map[string]any{
			"code":    te.Code,
			"message": te.Message,
			"details": te.Details,
		}
	}

	action, retryAfter := NextAction(state, result.Verdict, errCode)
	if errCode == "RATE_LIMIT" {
		// retry_after is the limiter remainder carried in details
		if te, ok := err.(*ToolError); ok {
			if v, ok := te.Details["retry_after"]; ok {
				if i := intPtrOf(v); i != nil {
					retryAfter = i
				}
			}
		}
	}
	if result.Action != nil && *result.Action != "" {
		action = *result.Action
	}
	if result.RetryAfter != nil {
		retryAfter = result.RetryAfter
	}
	if result.NoAction {
		action, retryAfter = "", nil
	}
	if action != "" {
		env["next_action"] = action
	}
	if retryAfter != nil {
		env["retry_after"] = *retryAfter
	}
	for k, v := range result.Data {
		env[k] = v
	}
	return env
}

func envelopeStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return HTTPStatusFor(normalizeToolError(err).Code)
}

func notAcceptedEnvelope(code, message string, details map[string]any) map[string]any {
	env := map[string]any{
		"ok":          false,
		"error":       map[string]any{"code": code, "message": message, "details": details},
		"next_action": nil,
		"retry_after": nil,
	}
	action, retry := NextAction("", nil, code)
	if action != "" {
		env["next_action"] = action
	}
	if code == "RATE_LIMIT" && details != nil {
		if v, ok := details["retry_after"]; ok {
			if i := intPtrOf(v); i != nil {
				retry = i
			}
		}
	}
	if retry != nil {
		env["retry_after"] = *retry
	}
	return env
}

// intPtrOf normalizes numeric detail values to *int.
func intPtrOf(v any) *int {
	switch t := v.(type) {
	case int:
		return &t
	case int32:
		i := int(t)
		return &i
	case int64:
		i := int(t)
		return &i
	case float64:
		i := int(t)
		return &i
	}
	return nil
}

// WriteOwnerToolJSON writes one console tool-call response with the
// §8.2 envelope and the protocol-layer status (the owner console's
// single bridge shares the /api/v1 wire contract).
func WriteOwnerToolJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(`{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"An internal error occurred"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
