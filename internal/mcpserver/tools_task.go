package mcpserver

// Registry handlers for the ten executor tools — thin adapters over
// the Task 1.0 service layer (WO-3..WO-6). No SQL, no business rules.

import (
	"context"
	"encoding/json"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func argError(message string) error {
	return &ToolError{Code: "VALIDATION_FAILED", Message: message}
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

// submissionPayload projects a submission view into the §8.2 flat
// fields plus the internal verdict hook for NextAction.
func submissionPayload(v any, verdictJSON []byte) (ToolResult, error) {
	m, err := asMap(v)
	if err != nil {
		return ToolResult{}, err
	}
	res := ToolResult{Data: m}
	if len(verdictJSON) > 0 {
		var verdict task.Verdict
		if json.Unmarshal(verdictJSON, &verdict) == nil {
			res.Verdict = &verdict
		}
	}
	return res, nil
}

// -- discovery --

func handleWorkList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	items, err := service.ListWork(ctx, deps.Pool, agent.ID, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	return data(map[string]any{"tasks": items})
}

func handleWorkGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
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
		Code  string `json:"code"`
		RefID string `json:"ref_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" || in.RefID == "" {
		return ToolResult{}, argError("code and ref_id are required")
	}
	m, err := service.GetHarness(ctx, deps.Pool, agent.ID, in.Code, in.RefID)
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
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
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
	if err := json.Unmarshal(args, &in); err != nil {
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
	if err := json.Unmarshal(args, &in); err != nil {
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
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	view, err := service.SubmitWork(ctx, deps.Pool, agent.ID, in, deps.AgentRefKey, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	return submissionPayload(view, view.Verdict)
}

func handleWorkStatus(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		SubmissionID *service.WireID `json:"submission_id"` // string or integer on the wire
		Code         string          `json:"code"`
		RequestKey   string          `json:"request_key"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, argError("submission_id must be an integer or a numeric string")
	}
	if in.SubmissionID == nil && (in.Code == "" || in.RequestKey == "") {
		return ToolResult{}, argError("give submission_id or code + request_key")
	}
	m, err := service.GetSubmissionStatus(ctx, deps.Pool, agent.ID, in.SubmissionID.Int64Ptr(), in.Code, in.RequestKey)
	if err != nil {
		return ToolResult{}, err
	}
	// events[] carries per-event times; the verdict drives NextAction
	res := ToolResult{Data: m, NoAction: false}
	if v, ok := m["verdict"].([]byte); ok {
		_ = v
	}
	if raw, ok := m["verdict"]; ok && raw != nil {
		if b, err := json.Marshal(raw); err == nil {
			var verdict task.Verdict
			if json.Unmarshal(b, &verdict) == nil {
				res.Verdict = &verdict
			}
		}
	}
	return res, nil
}

func handleWorkHistory(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
		Page int    `json:"page"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
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
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" || in.Reason == "" {
		return ToolResult{}, argError("code and reason are required")
	}
	m, err := service.ReportTask(ctx, deps.Pool, agent.ID, in.Code, in.Reason)
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}
