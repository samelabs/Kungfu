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
//              AgentLookup seam and dispatches into CallExecutorTool.
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

// ToolHandler runs one tool for a verified agent over its raw JSON
// arguments. The result map is merged into the §8.2 envelope; errors
// carry a stable application code (see normalizeToolError). Handlers
// may set the reserved "_"-prefixed keys (_action, _retry_after,
// _verdict, _rate_retry_after) to steer the envelope; they never
// cross the wire.
type ToolHandler func(ctx context.Context, agent *model.Bot, args json.RawMessage) (map[string]any, error)

// ToolDef is one registry entry; Handler binds the server's Deps.
type ToolDef struct {
	Name        string
	Description string
	InputSchema string // JSON Schema (draft 2020-12), root object
	Handler     func(deps *Deps) ToolHandler
}

// bind returns the Deps-bound handler.
func (t ToolDef) bind(deps *Deps) ToolHandler { return t.Handler(deps) }

// factory adapts a (deps, agent, args) handler into the factory form.
func factory(fn func(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error)) func(*Deps) ToolHandler {
	return func(deps *Deps) ToolHandler {
		return func(ctx context.Context, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
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

// httpStatusByCode is the ONE protocol-layer code→HTTP status table
// (§8.4); service-internal AppError HTTP codes map ONLY through it.
var httpStatusByCode = map[string]int{
	"UNAUTHORIZED":          http.StatusUnauthorized,
	"RATE_LIMIT":            http.StatusTooManyRequests, // the only 429
	"TASK_NOT_FOUND":        http.StatusNotFound,
	"SUBMISSION_NOT_FOUND":  http.StatusNotFound,
	"HARNESS_REF_NOT_FOUND": http.StatusNotFound,
	"UNKNOWN_TOOL":          http.StatusNotFound,
	"OWN_TASK":              http.StatusForbidden,
	"NOT_OWNER":             http.StatusForbidden,
	"INSUFFICIENT_CREDITS":  http.StatusPaymentRequired,
	"PAYLOAD_TOO_LARGE":     http.StatusRequestEntityTooLarge,
	"TASK_NOT_OPEN":         http.StatusConflict,
	"SLOTS_EXHAUSTED":       http.StatusConflict,
	"SUBMISSION_LIMIT":      http.StatusConflict,
	"CLAIM_REQUIRED":        http.StatusConflict,
	"CLAIM_INVALID":         http.StatusConflict,
	"IDEMPOTENCY_CONFLICT":  http.StatusConflict,
	"INVALID_STATE":         http.StatusConflict,
	"HAS_RESERVATIONS":      http.StatusConflict,
	"NOT_UNDER_REVIEW":      http.StatusConflict,
	"NOTHING_TO_REFUND":     http.StatusConflict,
	"SCHEMA_MISMATCH":       http.StatusUnprocessableEntity,
	"CREDENTIAL_IN_PAYLOAD": http.StatusUnprocessableEntity,
	"INVALID_REVISES":       http.StatusUnprocessableEntity,
	"INVALID_REQUEST_KEY":   http.StatusUnprocessableEntity,
	"VALIDATION_FAILED":     http.StatusUnprocessableEntity,
	"VERDICT_INVALID":       http.StatusUnprocessableEntity,
	"TEST_DELIVERY_FAILED":  http.StatusUnprocessableEntity,
}

// HTTPStatusFor is the protocol-layer mapping; codes outside the table
// are INTERNAL_ERROR → 500 with a fixed message (no internal detail
// ever crosses the boundary).
func HTTPStatusFor(code string) int {
	if status, ok := httpStatusByCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
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
	if _, listed := httpStatusByCode[te.Code]; !listed {
		te = &ToolError{Code: "INTERNAL_ERROR", Message: internalErrorMessage}
	}
	return te
}

// executorTools is the registry.
var executorTools = []ToolDef{
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
		InputSchema: `{"type":"object","properties":{"claim_id":{"type":"integer"}},"required":["claim_id"],"additionalProperties":false}`,
		Handler:     factory(handleWorkClaimRenew),
	},
	{
		Name: "work_release",
		Description: `Release your active claim; the reserved price returns to the task.
Preconditions: the claim is yours and active (else CLAIM_INVALID).
Result: the claim view with status "released".
next_action: pick other work with work_list.`,
		InputSchema: `{"type":"object","properties":{"claim_id":{"type":"integer"}},"required":["claim_id"],"additionalProperties":false}`,
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
			"claim_id":{"type":"integer"},
			"revises":{"type":"integer"}
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
			"submission_id":{"type":"integer"},
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
}

// ExecutorTool returns the registry definition by name.
func ExecutorTool(name string) (ToolDef, bool) {
	for _, t := range executorTools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolDef{}, false
}

// ExecutorToolNames lists the registry (tests, parity checks).
func ExecutorToolNames() []string {
	out := make([]string, 0, len(executorTools))
	for _, t := range executorTools {
		out = append(out, t.Name)
	}
	return out
}

// CallExecutorTool runs one registry tool and builds the §8.2
// envelope. It returns the envelope and the HTTP status (200 on ok;
// the protocol table otherwise). The MCP channel uses the same
// envelope as structuredContent and mirrors not-accepted calls with
// isError = true.
func CallExecutorTool(ctx context.Context, deps *Deps, name string, agent *model.Bot, rawArgs json.RawMessage) (map[string]any, int) {
	def, ok := ExecutorTool(name)
	if !ok {
		return notAcceptedEnvelope("UNKNOWN_TOOL", "Unknown tool "+name, nil), HTTPStatusFor("UNKNOWN_TOOL")
	}
	result, err := def.bind(deps)(ctx, agent, rawArgs)
	return buildEnvelope(result, err), envelopeStatus(err)
}

// buildEnvelope assembles the flat §8.2 object.
func buildEnvelope(result map[string]any, err error) map[string]any {
	env := map[string]any{
		"ok":          err == nil,
		"error":       nil,
		"next_action": nil,
		"retry_after": nil,
	}

	var state string
	var verdict *task.Verdict
	var errCode string
	var retryOverride *int
	var actionOverride string
	var rateRetry any

	if result != nil {
		if s, ok := result["state"].(string); ok {
			state = s
		}
		if v, ok := result["_verdict"].(*task.Verdict); ok {
			verdict = v
		}
		if a, ok := result["_action"].(string); ok {
			actionOverride = a
		}
		if r, ok := result["_retry_after"].(*int); ok {
			retryOverride = r
		}
		if r, ok := result["_rate_retry_after"]; ok {
			rateRetry = r
		}
	}

	if err != nil {
		te := normalizeToolError(err)
		errCode = te.Code
		env["error"] = map[string]any{
			"code":    te.Code,
			"message": te.Message,
			"details": te.Details,
		}
	}

	action, retryAfter := NextAction(state, verdict, errCode)
	if actionOverride != "" {
		action = actionOverride
	}
	if retryOverride != nil {
		retryAfter = retryOverride
	}
	if errCode == "RATE_LIMIT" && rateRetry != nil {
		retryAfter = intPtrOf(rateRetry)
	}
	if action != "" {
		env["next_action"] = action
	}
	if retryAfter != nil {
		env["retry_after"] = *retryAfter
	}

	for k, v := range result {
		if len(k) > 0 && k[0] == '_' {
			continue // reserved plumbing keys never cross the wire
		}
		env[k] = v
	}
	return env
}

func intPtrOf(v any) *int {
	switch t := v.(type) {
	case int:
		return &t
	case int64:
		i := int(t)
		return &i
	case float64:
		i := int(t)
		return &i
	}
	return nil
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
	if action, retry := NextAction("", nil, code); action != "" {
		env["next_action"] = action
		if retry != nil {
			env["retry_after"] = *retry
		}
	}
	return env
}
