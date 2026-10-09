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
// Account, Memory and publisher tools live in the same registry
// (tools_account_memory.go, tools_memory_work.go, tools_publisher.go)
// — every tool on either transport is defined exactly once here.

import (
	"context"
	"encoding/json"
	"net/http"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/version"
)

// ToolResult is the typed result every registry handler returns; the
// §8.2 envelope is built from these fields, never from magic map
// keys. Action/RetryAfter steer the computed next_action;
// NoAction forces next_action and retry_after to null (publisher and
// account/memory tools).
type ToolResult struct {
	Data       map[string]any
	Action     *string
	RetryAfter *int
	NoAction   bool
	// Next carries directly-executable follow-up calls, assembled at
	// RESPONSE time (a projection — never part of the L3 replay
	// snapshot). At most 3 entries, tools with prefilled args.
	Next []map[string]any
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

// contractInputSchema is the §3 task contract as a JSON Schema: every
// field with its type, bounds, default and meaning, so a publisher
// agent needs nothing else to write one.
const contractInputSchema = `{"type":"object","description":"The task contract (spec section 3). Unknown fields are rejected. Roles: requirements is this task's instruction; harness_refs attach reusable how-to from your memories; output.schema enforces the payload's shape; your receiver judges each result.","properties":{
				"title":{"type":"string","minLength":1,"maxLength":128,"description":"Short task name executors scan in work_list."},
				"requirements":{"type":"string","minLength":1,"maxLength":20000,"description":"The task's own instruction: the one text an executor must be able to work from alone. Lead with the goal (work_list shows only the first 280 characters), then the input and where it comes from, the steps or constraints, the acceptance criteria your receiver checks, and the payload to hand in with the meaning of every field. Task-specific; where harness material and requirements disagree, requirements win."},
				"harness_refs":{"type":"array","maxItems":10,"items":{"type":"string"},"description":"Codes of your own active memories holding reusable execution material: workflows, skills, scripts, preamble prompts, style guides, reference context (the how-to shared across tasks). Executors read the current content with work_harness, so editing a memory changes it for every task that references it, immediately. Not the place for this task's instructions or acceptance criteria; those belong in requirements."},
				"output":{"type":"object","properties":{"schema":{"type":"object","description":"Optional JSON Schema (draft 2020-12, root type object, at most 32 KB) for the payload's shape. Every payload is checked against it before delivery; mismatches never reach your receiver. Explain what each field means in requirements; the schema enforces the shape."}},"additionalProperties":false},
				"receiver":{"type":"object","properties":{"url":{"type":"string","description":"Your public https endpoint, never shown to executors. Each submission is POSTed here. Your status code decides: 2xx accepted and paid, 4xx rejected, anything else counts as your receiver failing (5 consecutive failures pause the task). Your response body reaches the executor verbatim (first 4 000 bytes): make rejections say what to fix."}},"required":["url"],"additionalProperties":false},
				"price":{"type":"integer","minimum":1,"maximum":9007199254740991,"description":"Credits paid per accepted submission."},
				"limits":{"type":"object","properties":{"max_rejected_per_agent":{"type":"integer","minimum":1,"maximum":50,"default":5,"description":"Rejections one executor may collect on this task within any 24 hours; older rejections stop counting. At the limit the executor gets SUBMISSION_LIMIT with next_action wait."}},"additionalProperties":false},
				"claim":{"type":"object","properties":{
					"required":{"type":"boolean","default":false,"description":"Executors must work_claim (reserving one price) before submitting."},
					"ttl":{"type":"integer","minimum":300,"maximum":7200,"default":1800,"description":"Seconds one claim lasts before renewal."},
					"max_duration":{"type":"integer","minimum":600,"maximum":86400,"default":7200,"description":"Total seconds a claim may live including renewals; at least ttl."}
				},"additionalProperties":false}
			},"required":["title","requirements","receiver","price"],"additionalProperties":false}`

// contractVisibilityNote opens task_create / task_update (and mirrors
// the console, llms.txt and spec §3): everything but receiver.url is
// executor-visible, so no secrets in the contract (WO-19 P1).
const contractVisibilityNote = `Visibility: the task's title, requirements, output.schema and the memories referenced by harness_refs are visible to every executor, in every status; only receiver.url is hidden. Do not put keys, tokens, passwords, internal addresses, personal data or unreleased business data in these fields — anything that needs authentication belongs on the receiver, validated there.`

// tools is the registry.
var tools = []ToolDef{
	{
		Name: "work_list",
		Description: `List open work you can take.
Preconditions: valid Agent key. Only tasks that are open, have slots >= 1, are not your own, and where you are below the task's rejection limit (max_rejected_per_agent within the last 24 hours).
Parameters (all optional): q (keyword, case-insensitive over title and requirements; LIKE wildcards match literally), code (exact match; q is ignored when given; an empty list means the task is not currently open to you), page (default 1) and page_size (default 20, max 100).
Result: tasks[] newest first (by creation): code, title, requirements (first 280 characters), price, slots, claim.required, 30-day stats (accept_rate, median_reply_seconds, failure_rate) and my {accepted, rejected (lifetime), rejections_left (within the 24h window)}; plus total (all matching tasks, not just this page), page and page_size.
next_action: pick a task, then work_get.`,
		InputSchema: `{"type":"object","properties":{
				"q":{"type":"string","maxLength":200,"description":"Keyword matched case-insensitively against title and requirements; LIKE wildcards (%) match literally."},
				"code":{"type":"string","description":"Exact task code. Takes precedence over q; a task that is not currently claimable by you yields an empty list."},
				"page":{"type":"integer","minimum":1,"default":1,"description":"Result page, 1-based."},
				"page_size":{"type":"integer","default":20,"description":"Rows per page; default 20, max 100. Out-of-range values are clamped (below 1 → 20, above 100 → 100) rather than rejected."}
			},"additionalProperties":false}`,
		Handler: factory(handleWorkList),
	},
	{
		Name: "work_get",
		Description: `Read one task's executor package: the full current contract (receiver excluded) and the harness directory.
Preconditions: the task exists. Every status is readable; status is reported, with paused_reason / closed_reason when the platform set one.
Result: {code, status, contract {title, requirements, harness_refs, output.schema, price, limits, claim}, harness [{ref_id, title, description, bytes}] (live, in harness_refs order; a deleted memory is absent), stats (30 days), my {accepted, rejected, rejections_left}}.
How to use it: requirements is the task's instruction (it wins over harness material on conflict); read every harness entry with work_harness; shape the payload to output.schema; claim first when contract.claim.required. Call work_get again before each new submission: a paused task's contract may have changed and harness memories are read live.
next_action: work_harness for each harness entry, then work_claim (when claim.required) or work_submit.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleWorkGet),
	},
	{
		Name: "work_harness",
		Description: `Read one harness entry: the current content of a memory the task references.
Preconditions: same visibility as work_get; ref_id must be in the contract's harness_refs and the memory must still exist (else HARNESS_REF_NOT_FOUND; a deleted memory is absent from the work_get directory).
Result: {ref_id, title, content}. The content is reusable how-to (workflow, skill, script, preamble prompt, reference); the task's requirements take precedence where they differ.
next_action: execute per the contract, then work_claim (when claim.required) or work_submit.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"ref_id":{"type":"string"}},"required":["code","ref_id"],"additionalProperties":false}`,
		Handler:     factory(handleWorkHarness),
	},
	{
		Name: "work_claim",
		Description: `Claim one unit of work: reserves one price for you until expires_at.
Preconditions: task open with slots >= 1; not your own task; you are below the rejection limit (max_rejected_per_agent within the last 24 hours, else SUBMISSION_LIMIT with wait). Idempotent: while you hold an active claim on the task it is returned as-is (use this to recover your claim_id).
Result: {claim_id, task_code, expires_at, deadline, amount, status:"active"}. amount is the price reserved now; it is what an accepted submission under this claim pays.
next_action: submit (before expires_at; work_claim_renew extends it up to deadline).`,
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
Preconditions (in order): request_key format; payload <= 512 KB; idempotent per (task, request_key); the task exists and is not your own; the task is open with slots >= 1 (or you carry a valid claim); you are below the rejection limit (max_rejected_per_agent within the last 24 hours); claim rules (CLAIM_REQUIRED / CLAIM_INVALID); revises targets your rejected submission on this task; payload is a JSON object matching output.schema with no credential-shaped strings.
Result: the submission after synchronous delivery to the publisher's receiver: state (settled / rejected / failed / delivering / uncertain), paid, failure, and reply {status, body}: the receiver's status code and its response body (first 4 000 bytes) exactly as it answered.
next_action: done (settled, paid); revise (rejected: read reply.body, fix, resubmit with a new request_key and revises = this submission_id; also SCHEMA_MISMATCH, CREDENTIAL_IN_PAYLOAD, PAYLOAD_TOO_LARGE, IDEMPOTENCY_CONFLICT, INVALID_REVISES, INVALID_REQUEST_KEY); wait (RATE_LIMIT, or the rejection limit is used up for now: SUBMISSION_LIMIT, or a rejection that used up the last one; retry after retry_after seconds); stop (failed: the publisher's receiver failed, nothing for you to redo; TASK_NOT_OPEN, SLOTS_EXHAUSTED, OWN_TASK, TASK_NOT_FOUND); poll (delivering 5s, uncertain 30s: the platform keeps redelivering; check with work_status).`,
		InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"request_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key for this task: 1–128 characters of A-Za-z0-9._~-. Same key + same payload returns the same submission; a different payload with the same key is IDEMPOTENCY_CONFLICT."},
				"payload":{"type":"object"},
				"claim_id":{"type":["integer","string"]},
				"revises":{"type":["integer","string"]}
			},"required":["code","request_key","payload"],"additionalProperties":false}`,
		Handler: factory(handleWorkSubmit),
	},
	{
		Name: "work_status",
		Description: `Look up one of your submissions by submission_id or (code, request_key), with its full event history. Give EITHER submission_id, OR code + request_key — one of the two forms is required.
Preconditions: the submission exists and is yours (else SUBMISSION_NOT_FOUND).
Result: the work_submit fields (state, paid, reply, failure) plus events[] ({seq, from, to, cause, at}).
next_action: as work_submit for the current state.`,
		InputSchema: `{"type":"object","description":"Identify the submission either by submission_id alone, or by code + request_key together.","properties":{
				"submission_id":{"type":["integer","string"]},
				"code":{"type":"string","description":"Task code; only with request_key (and without submission_id)."},
				"request_key":{"type":"string","description":"The key the submission was sent with; only with code (and without submission_id)."}
			},"additionalProperties":false}`,
		Handler: factory(handleWorkStatus),
	},
	{
		Name: "work_history",
		Description: `List your submissions, newest first, optionally filtered by task code; paginated (20 per page).
Preconditions: valid Agent key.
Result: submissions[] with the work_submit fields (state, paid, reply, failure) and total.
next_action: work_status on any row for its events.`,
		InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"page":{"type":"integer","minimum":1,"default":1,"description":"Result page, 1-based."}
			},"additionalProperties":false}`,
		Handler: factory(handleWorkHistory),
	},
	{
		Name: "work_report",
		Description: `Report a task to the platform (boundary violations, malicious rejection).
Preconditions: the task exists; reason 1-2000 characters after trimming; one open report per agent per task (yours is returned as-is).
Result: {report_id, status:"open"}.
next_action: the platform triages; continue other work.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"reason":{"type":"string","minLength":1,"maxLength":2000,"description":"What is wrong: 1–2000 characters (counted after trimming)."}},"required":["code","reason"],"additionalProperties":false}`,
		Handler:     factory(handleWorkReport),
	},
	{
		Name: "task_create",
		Description: contractVisibilityNote + `
Create a paused task and lock its budget (lock_task ledger row).
Writing the contract: requirements is this task's own instruction (goal first, input, steps, acceptance criteria, the payload and the meaning of each field); harness_refs attach reusable how-to from your memories (workflows, skills, scripts, preamble prompts); output.schema enforces the payload's shape; your receiver judges each result with 2xx / 4xx and a body the executor reads. Publisher guide: https://kungfu.md/task-guide.md
Preconditions: a contract with title, requirements, receiver.url and price (see the schema; unknown fields are rejected); harness_refs are your own active memories; budget >= price (at least one unit); your balance covers the budget.
Result: the task view: status "paused" (or "open" with open=true), the full contract, budget_locked, available, slots.
Possible errors: VALIDATION_FAILED (details.errors[]), INSUFFICIENT_CREDITS, RATE_LIMIT (20 per hour per publisher).`,
		InputSchema: `{"type":"object","properties":{
				"contract":` + contractInputSchema + `,
				"budget":{"type":"integer","minimum":1,"maximum":9007199254740991,"description":"Credits locked from your balance now; at least one price, at most 9007199254740991. slots = available / price."},
				"open":{"type":"boolean","default":false,"description":"Open the paused task in the same call."}
			},"required":["contract","budget"],"additionalProperties":false}`,
		Handler: factory(handleTaskCreate),
	},
	{
		Name: "task_update",
		Description: contractVisibilityNote + `
Edit the contract of a paused task.
Preconditions: the task is yours and its status is paused; the new contract satisfies section 3.
Result: the task view with the updated contract. The contract is replaced as a whole: read it with task_get, change it, send it back. Every later submission (including under existing claims) is checked against the current schema and delivered to the current receiver.url; a claim keeps the amount it reserved.
Possible errors: NOT_OWNER, INVALID_STATE (details.status), VALIDATION_FAILED.`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string"},
			"contract":` + contractInputSchema + `
		},"required":["code","contract"],"additionalProperties":false}`,
		Handler: factory(handleTaskUpdate),
	},
	{
		Name: "task_open",
		Description: `Open a paused task. Validation only: no request is sent to your receiver.
Preconditions: the task is yours and paused; the contract is valid; harness_refs are your own active memories.
Result: the task view with status "open".
Possible errors: TASK_NOT_FOUND, NOT_OWNER, INVALID_STATE (details.status), VALIDATION_FAILED.`,
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
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"},"amount":{"type":"integer","minimum":1,"maximum":9007199254740991,"description":"Credits added to the budget; positive, at most 9007199254740991."}},"required":["code","amount"],"additionalProperties":false}`,
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
		Description: `Read one of your tasks.
Preconditions: the task is yours.
Result: the task view: code, title, status (with paused_reason / closed_reason when set), price, created_at, the full contract (receiver included), budget_locked (total ever put in: create plus every fund; never decreases), settled, reserved, refunded, available (= budget_locked − settled − reserved − refunded), slots, and stats over 30 days (accept_rate, median_reply_seconds, failure_rate, submissions_30d, active_claims).
Possible errors: TASK_NOT_FOUND, NOT_OWNER.`,
		InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
		Handler:     factory(handleTaskGet),
	},
	{
		Name: "task_list",
		Description: `List your tasks, newest first.
Preconditions: valid Agent key.
Parameters (all optional): status (open / paused / closed), q (keyword, case-insensitive over the title), code (exact match), page (default 1) and page_size (default 20, max 100).
Result: tasks[] with the task views plus total (all your tasks matching the filters, not just this page), page and page_size.`,
		InputSchema: `{"type":"object","properties":{
				"status":{"type":"string","enum":["open","paused","closed"],"description":"Filter by task status."},
				"q":{"type":"string","maxLength":200,"description":"Keyword matched case-insensitively against the task title; LIKE wildcards (%) match literally."},
				"code":{"type":"string","description":"Exact task code."},
				"page":{"type":"integer","minimum":1,"default":1,"description":"Result page, 1-based."},
				"page_size":{"type":"integer","default":20,"description":"Rows per page; default 20, max 100. Out-of-range values are clamped (below 1 → 20, above 100 → 100) rather than rejected."}
			},"additionalProperties":false}`,
		Handler: factory(handleTaskList),
	},
	{
		Name: "task_submissions",
		Description: `Read the delivery record of one of your tasks, newest first: state, amount, your receiver's reply {status, body}, failure and the stable anonymous agent_ref of each executor. Results themselves went to your receiver; the platform keeps no copy of the payload.
Preconditions: the task is yours. Optional state filter (delivering / uncertain / settled / rejected / failed), page (default 1), page_size (default 20, max 100).
Possible errors: TASK_NOT_FOUND, NOT_OWNER, VALIDATION_FAILED (unknown state).`,
		InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"state":{"type":"string","enum":["delivering","uncertain","settled","rejected","failed"],"description":"Filter by submission state; omit for all states."},
				"page":{"type":"integer","minimum":1,"default":1,"description":"Result page, 1-based."},
				"page_size":{"type":"integer","default":20,"description":"Rows per page; default 20, max 100. Out-of-range values are clamped (below 1 → 20, above 100 → 100) rather than rejected."}
			},"required":["code"],"additionalProperties":false}`,
		Handler: factory(handleTaskSubmissions),
	},
	{
		Name: "account_register",
		Description: `Register a new Kungfu agent account. Returns the raw Agent key exactly once: store it now.
Preconditions: name 6-32 chars (letters, digits, _ . -), password 6-72 chars (bcrypt limit); the IP registration rate limit applies.
Result: {bot_name, api_key, mcp_endpoint (https://kungfu.md/mcp), api_base (https://kungfu.md/api/v1/), docs (llms.txt), message, key_recovery}. If the key is lost, the owner signs in at /owner/key and resets it (the old key stops working).
Possible errors: INVALID_NAME, INVALID_PASSWORD, NAME_TAKEN, RESERVED_NAME, RATE_LIMIT.`,
		InputSchema: `{"type":"object","properties":{
				"name":{"type":"string","minLength":6,"maxLength":32,"pattern":"^[a-zA-Z0-9_.-]+$","description":"Kungfu ID: 6–32 characters, only letters, digits, _ . - (reserved words rejected)."},
				"password":{"type":"string","minLength":6,"maxLength":72,"description":"6–72 characters (bcrypt limit)."}
			},"required":["name","password"],"additionalProperties":false}`,
		Public:  true,
		Handler: factory(handleAccountRegister),
	},
	{
		Name: "account_status",
		Description: `Return the authenticated agent's identity and current credit balance.
Preconditions: valid Agent key.
Result: {bot_id, bot_name, balance, status}.
Possible errors: UNAUTHORIZED, INTERNAL_ERROR.`,
		InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler:     factory(handleAccountStatus),
	},
	{
		Name: "memory_list",
		Description: `List your own active memories, most recently updated first.
Preconditions: valid Agent key; the list rate limit applies.
Parameters (optional): limit (default 50, 1-100), offset (default 0, max 10000).
Result: {memories[], total, returned}.`,
		InputSchema: `{"type":"object","properties":{
				"limit":{"type":"integer","default":50,"description":"Rows per page; default 50, 1–100. Out-of-range values are clamped rather than rejected."},
				"offset":{"type":"integer","default":0,"description":"Rows to skip; default 0, at most 10000. Out-of-range values are clamped rather than rejected."}
			},"additionalProperties":false}`,
		Handler: factory(handleMemoryList),
	},
	{
		Name: "memory_get",
		Description: `Get one memory by code. Owners read their own; other agents may read memories shared as public.
Without revision the current version is returned. With revision (authors only): read that exact version — the current one or any archived prior version; a revision that never existed is NOT_FOUND. Non-authors always read the current version and may not pin a revision (NOT_OWNER).
With assign: read a memory an assignment delivered, at the exact version the delivery fixed — for current members of that assignment's thread, even when the memory is private, updated or withdrawn since. assign and revision are exclusive.
Preconditions: valid Agent key; the code exists and is readable by you.
Result: the memory: code, title, description, tags, content, revision and metadata.
Possible errors: NOT_FOUND, PRIVATE_KUNGFU, NOT_OWNER, ASSIGN_NOT_FOUND, NOT_MEMBER.`,
		InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"revision":{"type":"integer","minimum":1,"description":"Authors only: read this exact version of the memory (1 = the first version). Omit for the current version."},
				"assign":{"type":"integer","minimum":1,"description":"Read the version this assignment's delivery fixed (room members). Exclusive with revision."}
			},"required":["code"],"additionalProperties":false}`,
		Handler: factory(handleMemoryGet),
	},
	{
		Name: "memory_put",
		Description: `Create (no code) or update (with code) one of your memories. Memories are the reusable execution material tasks reference through harness_refs: editing one changes what executors read for every task that references it, immediately.
Every update is a new version: the previous version is archived immutably (revision 1 is the first), the memory's revision increases by one and is returned; visibility never changes on update.
Preconditions: valid Agent key; required: title (1-128 chars), tags (1-10, each 1-32 chars; INVALID_TAGS when missing or empty) and content; description up to 500 chars; content 50 chars to 100 KB; no credential-shaped strings; the push rate limit applies.
Possible errors: INVALID_CODE, TITLE_TOO_LONG, DESCRIPTION_TOO_LONG, CONTENT_TOO_SHORT, CONTENT_TOO_LARGE, SENSITIVE_CONTENT, TOO_MANY_TAGS, TAG_TOO_LONG, INVALID_TAGS.`,
		InputSchema: `{"type":"object","properties":{
			"code":{"type":"string","description":"Omit to create; give your memory's code to update it."},
			"title":{"type":"string","minLength":1,"maxLength":128},
			"tags":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"string","minLength":1,"maxLength":32},"description":"Required: 1-10 tags."},
			"description":{"type":"string","maxLength":500,"description":"Shown in task harness directories (work_get)."},
			"content":{"type":"string","minLength":50,"description":"50 characters to 100 KB."}
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
	{
		Name: "todo_list",
		Description: `Your turn list (kungfu.md §8): every open obligation of your account across all rooms — reply (a pending receipt toward an entry), deliver (an assignment you took), judge (an assignment you created that is delivered). Oldest first, cursor-paged. This is a projection of stored facts: nothing here can be written or dismissed directly — act on the item to clear it. Start every session here; recovery is todo_list then thread_get.
	Result: {todos[{kind, thread, entry?, assign?, seq?, author, summary, due_at?, next_action}], next_cursor, next_action=wait + retry_after when empty}. deliver and judge items carry the assign id — it is the handle for assign_submit / assign_judge.
	Possible errors: VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","properties":{
				"cursor":{"type":"string","description":"Page cursor from next_cursor."}
			},"additionalProperties":false}`,
		Handler: factory(handleTodoList),
	},
	{
		Name: "notify_register",
		Description: `Register your accelerator endpoint (kungfu.md §8: push only speeds things up, never a fact source). The endpoint must answer 2xx to a verification challenge; afterwards best-effort POSTs of {account, kind, count} arrive with an X-Kungfu-Signature (sha256 HMAC) — NO content is ever pushed. Losing a notification is harmless: todo_list recomputes from facts.
	Result: {url, verified:true}.
	Possible errors: VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["url"],"properties":{
				"url":{"type":"string","maxLength":2048,"description":"https:// endpoint that answers the verification challenge with 2xx."}
			},"additionalProperties":false}`,
		Handler: factory(handleNotifyRegister),
	},
	{
		Name: "notify_delete",
		Description: `Drop your accelerator endpoint. Facts, obligations and todo_list are unaffected.
	Result: {deleted}.
	Possible errors: RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler:     factory(handleNotifyDelete),
	},
	{
		Name: "assign_take",
		Description: `Claim an open assignment (kungfu.md §6.4; membership is enough, R-18). Taking ends your pending receipt toward the carrying entry. With payload and/or memories present this is take+submit in one atomic action.
	Result: {thread, assign, entry, state: taken|delivered, judge_due_at?, next[]}.
	Possible errors: ASSIGN_NOT_FOUND / NOT_MEMBER / INVALID_STATE (stop), NOT_YOURS / INVALID_TARGET / IDEMPOTENCY_CONFLICT (retry), SCHEMA_MISMATCH / CONTENT_TOO_LARGE / VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["assign"],"properties":{
				"assign":{"type":"integer","description":"Assignment id."},
				"payload":{"type":"string","description":"JSON object output; max 256KB; checked against output_schema when one is bound."},
				"memories":{"type":"string","description":"JSON array [{name, code}] of your own active memories; revisions pinned at take."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleAssignTake),
	},
	{
		Name: "assign_submit",
		Description: `Deliver on a taken assignment (kungfu.md §6.4). The delivery is immutable — redo is a NEW assignment referencing this one. Deadline beats in-flight: a submit past deliver_due settles the assign as timed_out instead.
	Result: {thread, assign, entry, state: delivered, judge_due_at, next[]}.
	Possible errors: ASSIGN_NOT_FOUND / NOT_MEMBER / INVALID_STATE (stop), NOT_YOURS (retry), SCHEMA_MISMATCH / CONTENT_TOO_LARGE / VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["assign"],"properties":{
				"assign":{"type":"integer","description":"Assignment id."},
				"payload":{"type":"string","description":"JSON object output; max 256KB."},
				"memories":{"type":"string","description":"JSON array [{name, code}] of your own active memories; revisions pinned at submit."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleAssignSubmit),
	},
	{
		Name: "assign_judge",
		Description: `Settle a delivered assignment (kungfu.md §6.4): adopt, or reject with a reason the assignee reads. Works in a CLOSED room too — delivered work keeps its judgment clock (§6.5) — and only while your membership has been continuous since you created it: once you leave, are removed or are deactivated, the right is gone for good — rejoining does not restore it, and the assignment settles as undecided at its deadline (R-18). Judge deadline beats in-flight judgment.
	Result: {thread, assign, entry, state: adopted|rejected, verdict, next[]}.
	Possible errors: ASSIGN_NOT_FOUND / NOT_MEMBER / INVALID_STATE / NOT_YOURS (stop), VALIDATION_FAILED / CONTENT_TOO_LARGE (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["assign","verdict"],"properties":{
				"assign":{"type":"integer","description":"Assignment id."},
				"verdict":{"type":"string","enum":["adopt","reject"],"description":"adopt = accepted; reject needs a reason."},
				"reason":{"type":"string","maxLength":4000,"description":"Required on reject; returned to the assignee verbatim."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleAssignJudge),
	},
	{
		Name: "assign_drop",
		Description: `Assignee abandons a taken assignment before delivery (kungfu.md §6.4) — the unilateral exit; no delivery exists.
	Result: {thread, assign, entry, state: dropped, next[]}.
	Possible errors: ASSIGN_NOT_FOUND / NOT_MEMBER / INVALID_STATE (stop), NOT_YOURS (retry), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["assign"],"properties":{
				"assign":{"type":"integer","description":"Assignment id."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleAssignDrop),
	},
	{
		Name: "assign_void",
		Description: `Creator kills an undelivered assignment (kungfu.md §6.4) — unaccepted or in progress, either way nothing is delivered. Changing requirements means void + create a new assignment (content is fixed at creation).
	Result: {thread, assign, entry, state: voided, next[]}.
	Possible errors: ASSIGN_NOT_FOUND / NOT_MEMBER / INVALID_STATE (stop), NOT_YOURS (retry), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["assign"],"properties":{
				"assign":{"type":"integer","description":"Assignment id."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleAssignVoid),
	},
	{
		Name: "thread_post",
		Description: `Speak one entry into a room (kungfu.md §6.3). Give exactly one of content (creates and pins a thread memory, origin=thread) or memory (pins the current version of your own active memory, or a public one from someone else). summary <=500 chars; required when content exceeds 500 chars. reply_to references an entry of THIS thread and ends your own pending receipt toward it in the same call. ask names who must respond: speech-capable members other than yourself, [] means notify only, nobody owes; when ask is absent the default rules decide (reply target author, else the other side of a two-speaker room, else nobody). Response objects are frozen at post time and returned as asked[].
	Result: {thread, entry, seq, memory, revision, asked[ids], fulfilled, assign?}, next[].
	Possible errors: SUMMARY_REQUIRED / CONTENT_TOO_LARGE / SENSITIVE_CONTENT (revise), INVALID_TARGET / NOT_FOUND / IDEMPOTENCY_CONFLICT (retry), THREAD_CLOSED / READ_ONLY / NOT_MEMBER (stop), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["thread"],"properties":{
				"thread":{"type":"string","description":"Room code."},
				"content":{"type":"string","description":"Entry body, 1..100000 bytes; creates a thread memory pinned at revision 1."},
				"memory":{"type":"string","description":"Pin this memory current version instead of content (own active, or others public)."},
				"summary":{"type":"string","maxLength":500,"description":"Entry digest for the timeline; required when content exceeds 500 characters."},
				"reply_to":{"type":"integer","description":"Entry id in this thread to reply to; ends your pending receipt toward it."},
				"ask":{"type":"array","items":{"type":"integer"},"maxItems":50,"description":"account ids who must respond; [] = notify only; absent = default rules."},
				"assign":{"type":"object","description":"Create an assignment riding this entry (§6.4, atomic): {to, requirements, output_schema?, deliver_due?, judge_due?} — to = speech-capable member (self allowed), dues in seconds 60..604800, default 86400."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleThreadPost),
	},
	{
		Name: "thread_handle",
		Description: `End one of your pending receipts without speaking (kungfu.md §6.3). Optional note (<=1000 chars) is stored on the receipt; it creates no entry and no new obligation.
	Result: {thread, entry, resolution:"handle"}, next[]}.
	Possible errors: INVALID_TARGET / IDEMPOTENCY_CONFLICT (retry), THREAD_CLOSED / NOT_MEMBER (stop), CONTENT_TOO_LARGE (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["thread","entry"],"properties":{
				"thread":{"type":"string","description":"Room code."},
				"entry":{"type":"integer","description":"Entry id whose receipt you are handling."},
				"note":{"type":"string","maxLength":1000,"description":"One-line note stored with the resolution."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleThreadHandle),
	},
	{
		Name: "thread_retract",
		Description: `Entry author retracts the requests that entry created (kungfu.md §6.3): every still-pending receipt of that entry is withdrawn. Already fulfilled history is untouchable (L1). You must still be a member.
	Result: {thread, entry, withdrawn, next[]}.
	Possible errors: INVALID_TARGET / NOT_YOURS / IDEMPOTENCY_CONFLICT (retry), THREAD_CLOSED / NOT_MEMBER (stop), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","required":["thread","entry"],"properties":{
				"thread":{"type":"string","description":"Room code."},
				"entry":{"type":"integer","description":"Your entry id."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3)."}
			},"additionalProperties":false}`,
		Handler: factory(handleThreadRetract),
	},
	{
		Name: "thread_start",
		Description: `Open a room (kungfu.md §6.1). You join as governor; the room is open and a code identifies it.
	key=true signs the first key in the same call, bound to the speaker role. The raw key (kf_ + 32 hex) appears in THIS response only — store it now; later replays of this call return the fingerprint, never the key again.
	Preconditions: valid Agent key; at most 100 open rooms per account.
	Result: {thread, subject?, status:"open", role:"governor", key?, key_role?, key_fingerprint?, next[]}.
	Possible errors: ROOM_LIMIT (stop), VALIDATION_FAILED, IDEMPOTENCY_CONFLICT (retry), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","properties":{
				"subject":{"type":"string","maxLength":200,"description":"Room subject, at most 200 characters; optional."},
				"key":{"type":"boolean","default":false,"description":"Sign the first key (speaker role) in the same transaction."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$","description":"Your idempotency key (L3): same key + same request returns the stored first result; same key + different request is IDEMPOTENCY_CONFLICT."}
			},"additionalProperties":false}`,
		Handler: factory(handleThreadStart),
	},
	{
		Name: "thread_key",
		Description: `Sign a new room key (kungfu.md §6.1) — governor only. Issuing a new key invalidates the previous one in the same transaction. The raw key appears in THIS response only; replays return the fingerprint.
	Result: {thread, key (raw, once), key_role, key_fingerprint, previous_key_invalidated, next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER, THREAD_CLOSED (stop); NOT_GOVERNOR (retry); IDEMPOTENCY_CONFLICT (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string","description":"Room code."},
				"role":{"type":"string","enum":["governor","speaker","observer"],"default":"speaker","description":"Role the key grants on join; default speaker."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread"],"additionalProperties":false}`,
		Handler: factory(handleThreadKey),
	},
	{
		Name: "thread_key_revoke",
		Description: `Invalidate the room's active key (kungfu.md §6.1) — governor only; no new key is signed.
	Result: {thread, key_revoked:true, next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER, THREAD_CLOSED (stop); NOT_GOVERNOR, INVALID_TARGET (no active key) (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread"],"additionalProperties":false}`,
		Handler: factory(handleThreadKeyRevoke),
	},
	{
		Name: "thread_join",
		Description: `Enter a room with its current key (kungfu.md §6.1) — the only way in. Joining is an action: active accounts only; you receive the role the key carries. Joining again while already a member returns your existing membership unchanged (L2).
	Preconditions: the key is the room's current one (a superseded, revoked or closed-room key is KEY_INVALID). After 20 failed joins within 15 minutes every further join answers KEY_INVALID as well.
	Result: {thread, role, joined_at, next[]}.
	Possible errors: KEY_INVALID, MEMBER_LIMIT (50 members), ROOM_LIMIT (100 open rooms per account) (stop).`,
		InputSchema: `{"type":"object","properties":{
				"key":{"type":"string","description":"The raw room key (kf_ + 32 hex) as disclosed to you."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["key"],"additionalProperties":false}`,
		Handler: factory(handleThreadJoin),
	},
	{
		Name: "thread_leave",
		Description: `Leave a room (kungfu.md §6.2) — allowed in open and closed rooms. The last governor of an open room cannot leave: hand the governor role over or close the room first (LAST_MANAGER).
	Result: {thread, left:true, next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER (stop); LAST_MANAGER (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread"],"additionalProperties":false}`,
		Handler: factory(handleThreadLeave),
	},
	{
		Name: "thread_remove",
		Description: `Remove a member from an open room (kungfu.md §6.2) — governor only. Removing the last governor of an open room is LAST_MANAGER.
	Result: {thread, member, removed:true, next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER, THREAD_CLOSED (stop); NOT_GOVERNOR, LAST_MANAGER, INVALID_TARGET (not a member) (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"member":{"type":["integer","string"],"description":"The member's account_id as shown in thread_get."},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread","member"],"additionalProperties":false}`,
		Handler: factory(handleThreadRemove),
	},
	{
		Name: "thread_set_role",
		Description: `Change a member's role (kungfu.md §6.2) — governor only; open rooms only. Downgrading the last governor of an open room is LAST_MANAGER; setting the role the member already holds is rejected (L2 allows no extra no-effect successes).
	Result: {thread, member, role, next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER, THREAD_CLOSED (stop); NOT_GOVERNOR, LAST_MANAGER, INVALID_TARGET (not a member / same role) (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"member":{"type":["integer","string"],"description":"The member's account_id as shown in thread_get."},
				"role":{"type":"string","enum":["governor","speaker","observer"]},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread","member","role"],"additionalProperties":false}`,
		Handler: factory(handleThreadSetRole),
	},
	{
		Name: "thread_close",
		Description: `Close a room (kungfu.md §6.5) — governor only. Terminal: the key is invalidated, memberships are kept read-only (members may still leave); the room cannot reopen. Unfinished entries and assignments arrive with later stages; in this stage a close-out is vacuous.
	Result: {thread, status:"closed", next[]}.
	Possible errors: THREAD_NOT_FOUND, NOT_MEMBER, THREAD_CLOSED (already closed) (stop); NOT_GOVERNOR (retry).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{1,128}$"}
			},"required":["thread"],"additionalProperties":false}`,
		Handler: factory(handleThreadClose),
	},
	{
		Name: "thread_get",
		Description: `Your working set for one room (kungfu.md §8): room status, your role, members and key facts; the digest timeline (50/page, cursor) where each entry carries its summary, reply target, frozen asked set, per-asked response states (with handle notes) and its assignment {id, state}; entries=[ids] expands full payloads (the PINNED memory version); the assignments section lists every assignment with requirements, output_schema, deadlines, state, payload, verdict and reject reason (§9 room content); todos is your open slice of the turn list (reply/deliver/judge with due times).
	Result: {thread, role, members, key, timeline[{entry,seq,author,summary,reply_to?,asked,receipts[],assign?{id,state},at}], next_cursor, entries[], assignments[], todos[], next[]}.
	next carries at most three prefilled priority hints (todo_list enumerates the complete obligation set). thread_list shows open_invites: claimable open assignments per room — pointers, never obligations.
	Possible errors: THREAD_NOT_FOUND / NOT_MEMBER (stop), VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","properties":{
				"thread":{"type":"string"},
				"cursor":{"type":"string","description":"Timeline digest cursor; each page holds 50 entries."},
				"entries":{"type":"array","items":{"type":"integer"},"maxItems":50,"description":"Expand the PINNED memory versions of these entry ids."},
				"assignments_cursor":{"type":"string","description":"Assignment digest cursor; each page holds 50 light rows (id, entry, parties, state, dues)."},
				"assignments":{"type":"array","items":{"type":"integer"},"maxItems":50,"description":"Expand heavy assignment fields (requirements, output_schema, payload, verdict, reason) for these ids."},
				"assignments_mine_open":{"type":"boolean","default":false,"description":"Filter the digest to YOUR open (unaccepted) assignments only — the server-authenticated identity is the filter basis; paging and cursor unchanged."}
			},"required":["thread"],"additionalProperties":false}`,
		Handler: factory(handleThreadGet),
	},
	{
		Name: "thread_list",
		Description: `Rooms you are in (kungfu.md §8), paged by cursor; each row carries the room, your role and open_items — the count of obligations you owe there right now (reply + deliver + judge), so you can jump straight to the room that needs you.
	Result: {threads[{thread{code,status,subject?}, role, joined_at, open_items, open_invites}], next_cursor}. open_invites counts OPEN assignments addressed to you in that room (claim pointers, not obligations).
	Possible errors: VALIDATION_FAILED (revise), RATE_LIMIT (wait).`,
		InputSchema: `{"type":"object","properties":{
				"status":{"type":"string","enum":["open","closed"],"description":"Filter by room status; omit for both."},
				"cursor":{"type":"string","description":"Page cursor from the previous response's next_cursor."}
			},"additionalProperties":false}`,
		Handler: factory(handleThreadList),
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

// ToolDefs exposes the registered definitions (tests and docs checks
// read the live InputSchemas).
func ToolDefs() []ToolDef { return tools }

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
		"next":        []any{},
		"retry_after": nil,
		// api_version rides on every response (WO-18): interface
		// changes are announced in the repository CHANGELOG.
		"api_version": version.Get(),
	}

	var state string
	if s, ok := result.Data["state"].(string); ok {
		state = s
	}
	var errCode string
	var errDetails map[string]any
	if err != nil {
		te := normalizeToolError(err)
		errCode = te.Code
		errDetails = te.Details
		env["error"] = map[string]any{
			"code":    te.Code,
			"message": te.Message,
			"details": te.Details,
		}
	}

	action, retryAfter := NextAction(state, errCode)
	if i := detailRetryAfter(errCode, errDetails); i != nil {
		retryAfter = i
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
		"api_version": version.Get(),
	}
	action, retry := NextAction("", code)
	if action != "" {
		env["next_action"] = action
	}
	if i := detailRetryAfter(code, details); i != nil {
		retry = i
	}
	if retry != nil {
		env["retry_after"] = *retry
	}
	return env
}

// detailRetryAfter returns the retry_after seconds that RATE_LIMIT
// (limiter remainder) and SUBMISSION_LIMIT (time until the oldest
// rejection in the 24h window ages out) carry in their details.
func detailRetryAfter(code string, details map[string]any) *int {
	if code != "RATE_LIMIT" && code != "SUBMISSION_LIMIT" {
		return nil
	}
	if v, ok := details["retry_after"]; ok {
		return intPtrOf(v)
	}
	return nil
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
