package mcpserver

// Registry handlers for the ten executor tools — thin adapters over
// the Task 1.0 service layer (WO-3..WO-6). No SQL, no business rules.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func argError(message string) error {
	return &ToolError{Code: "VALIDATION_FAILED", Message: message}
}

// decodeArgs strictly decodes tool arguments into in: an unknown
// argument is VALIDATION_FAILED naming the field, never silently
// dropped — every InputSchema promises additionalProperties:false and
// this is the decode that keeps that promise on both surfaces (/mcp
// and /api/v1 run these same handlers).
func decodeArgs(args json.RawMessage, in any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			field := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
			return &ToolError{Code: "VALIDATION_FAILED",
				Message: "Unknown argument " + field,
				Details: map[string]any{"errors": []map[string]string{
					{"field": field, "message": "unknown argument (see the tool input schema)"},
				}}}
		}
		return argError("arguments must match the tool schema")
	}
	return nil
}

// data builds a ToolResult with just its payload fields.
func data(m map[string]any) (ToolResult, error) {
	return ToolResult{Data: m, NoAction: true}, nil
}

func asMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// submissionResult wraps a submission's flat fields (§8.2). A rejected
// submission is "revise" — unless the agent has used up the rejection
// limit within the rolling 24h window: then it is "wait" with the
// seconds until the oldest counted rejection ages out (§8.3). Every
// other state follows NextAction.
func submissionResult(ctx context.Context, deps *Deps, agentID int64, m map[string]any) (ToolResult, error) {
	res := ToolResult{Data: m}
	if state, _ := m["state"].(string); state == task.SubRejected {
		code, _ := m["task_code"].(string)
		limited, wait, err := service.RejectionLimitWait(ctx, deps.Pool, agentID, code, time.Now())
		if err != nil {
			return ToolResult{}, err
		}
		if limited {
			action := "wait"
			res.Action = &action
			res.RetryAfter = &wait
		}
	}
	return res, nil
}

// -- discovery --

func handleWorkList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Q        string `json:"q"`
		Code     string `json:"code"`
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`) // a tools/call with no arguments at all
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	filter := service.WorkListFilter{Q: in.Q, Code: in.Code, Page: in.Page, PageSize: in.PageSize}
	filter.Normalize()
	items, total, err := service.ListWork(ctx, deps.Pool, agent.ID, time.Now(), filter)
	if err != nil {
		return ToolResult{}, err
	}
	// total = ALL tasks matching the filters (not just this page), so a
	// client can page or tell "exactly these" without counting itself.
	return data(map[string]any{"tasks": items, "total": total, "page": filter.Page, "page_size": filter.PageSize})
}

func handleWorkGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	m, err := service.GetWork(ctx, deps.Pool, agent.ID, in.Code, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}

func handleWorkHarness(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code    string          `json:"code"`
		RefID   string          `json:"ref_id"`
		ClaimID *service.WireID `json:"claim_id"` // optional: serve the claim's pinned revision
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.RefID == "" {
		return ToolResult{}, argError("code and ref_id are required")
	}
	m, err := service.GetHarness(ctx, deps.Pool, agent.ID, in.Code, in.RefID, in.ClaimID.Int64Ptr(), time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}

// -- claims --

func handleWorkClaim(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	view, err := service.ClaimTask(ctx, deps.Pool, agent.ID, in.Code, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	m, err := asMap(view)
	if err != nil {
		return ToolResult{}, err
	}
	submit := "submit"
	return ToolResult{Data: m, Action: &submit}, nil
}

func handleWorkClaimRenew(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		ClaimID service.WireID `json:"claim_id"` // string or integer on the wire
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("claim_id must be an integer or a numeric string")
	}
	if in.ClaimID == 0 {
		return ToolResult{}, argError("claim_id is required")
	}
	view, err := service.RenewClaim(ctx, deps.Pool, agent.ID, in.ClaimID.Int64(), time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	m, err := asMap(view)
	if err != nil {
		return ToolResult{}, err
	}
	submit := "submit"
	return ToolResult{Data: m, Action: &submit}, nil
}

func handleWorkRelease(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		ClaimID service.WireID `json:"claim_id"` // string or integer on the wire
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("claim_id must be an integer or a numeric string")
	}
	if in.ClaimID == 0 {
		return ToolResult{}, argError("claim_id is required")
	}
	view, err := service.ReleaseClaim(ctx, deps.Pool, agent.ID, in.ClaimID.Int64(), time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	m, err := asMap(view)
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}

// -- submissions --

func handleWorkSubmit(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	// §11: 120 submissions per 60s per agent (the existing limiter
	// action; RATE_LIMIT carries the limiter's remaining seconds).
	if !deps.limiter().CheckAgent(agent.ID, "task_submit") {
		retry := deps.limiter().CheckAgentWithDetails(agent.ID, "task_submit").RetryAfter
		return ToolResult{}, &ToolError{Code: "RATE_LIMIT", Message: "Rate limit exceeded",
			Details: map[string]any{"retry_after": retry}}
	}

	var in service.SubmitInput
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	view, err := service.SubmitWork(ctx, deps.Pool, agent.ID, in, deps.AgentRefKey, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	m, err := asMap(view)
	if err != nil {
		return ToolResult{}, err
	}
	return submissionResult(ctx, deps, agent.ID, m)
}

func handleWorkStatus(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		SubmissionID *service.WireID `json:"submission_id"` // string or integer on the wire
		Code         string          `json:"code"`
		RequestKey   string          `json:"request_key"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("submission_id must be an integer or a numeric string")
	}
	if in.SubmissionID == nil && (in.Code == "" || in.RequestKey == "") {
		return ToolResult{}, argError("give submission_id or code + request_key")
	}
	m, err := service.GetSubmissionStatus(ctx, deps.Pool, agent.ID, in.SubmissionID.Int64Ptr(), in.Code, in.RequestKey)
	if err != nil {
		return ToolResult{}, err
	}
	return submissionResult(ctx, deps, agent.ID, m)
}

func handleWorkHistory(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
		Page int    `json:"page"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	if in.Page < 1 {
		in.Page = 1
	}
	rows, total, err := service.ListHistory(ctx, deps.Pool, agent.ID, in.Code, in.Page, 20)
	if err != nil {
		return ToolResult{}, err
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, err := asMap(r)
		if err != nil {
			return ToolResult{}, err
		}
		items = append(items, m)
	}
	return data(map[string]any{"submissions": items, "total": total})
}

func handleWorkReport(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.Reason == "" {
		return ToolResult{}, argError("code and reason are required")
	}
	m, err := service.ReportTask(ctx, deps.Pool, agent.ID, in.Code, in.Reason)
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}
