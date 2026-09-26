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
func submissionPayload(v any, verdictJSON []byte) (map[string]any, error) {
	m, err := asMap(v)
	if err != nil {
		return nil, err
	}
	if len(verdictJSON) > 0 {
		var verdict task.Verdict
		if json.Unmarshal(verdictJSON, &verdict) == nil {
			m["_verdict"] = &verdict
		}
	}
	return m, nil
}

// -- discovery --

func handleWorkList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	items, err := service.ListWork(ctx, deps.Pool, agent.ID, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{"tasks": items}, nil
}

func handleWorkGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
		return nil, argError("code is required")
	}
	return service.GetWork(ctx, deps.Pool, agent.ID, in.Code, time.Now())
}

func handleWorkHarness(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Code  string `json:"code"`
		RefID string `json:"ref_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" || in.RefID == "" {
		return nil, argError("code and ref_id are required")
	}
	return service.GetHarness(ctx, deps.Pool, agent.ID, in.Code, in.RefID)
}

// -- claims --

func handleWorkClaim(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
		return nil, argError("code is required")
	}
	view, err := service.ClaimTask(ctx, deps.Pool, agent.ID, in.Code, time.Now())
	if err != nil {
		return nil, err
	}
	m, err := asMap(view)
	if err != nil {
		return nil, err
	}
	m["_action"] = "submit" // §8.3: claim active → submit
	return m, nil
}

func handleWorkClaimRenew(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		ClaimID int64 `json:"claim_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.ClaimID == 0 {
		return nil, argError("claim_id is required")
	}
	view, err := service.RenewClaim(ctx, deps.Pool, agent.ID, in.ClaimID, time.Now())
	if err != nil {
		return nil, err
	}
	m, err := asMap(view)
	if err != nil {
		return nil, err
	}
	m["_action"] = "submit"
	return m, nil
}

func handleWorkRelease(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		ClaimID int64 `json:"claim_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.ClaimID == 0 {
		return nil, argError("claim_id is required")
	}
	view, err := service.ReleaseClaim(ctx, deps.Pool, agent.ID, in.ClaimID, time.Now())
	if err != nil {
		return nil, err
	}
	return asMap(view)
}

// -- submissions --

func handleWorkSubmit(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	// §11: 120 submissions per 60s per agent (the existing limiter
	// action; RATE_LIMIT carries the limiter's remaining seconds).
	if !deps.limiter().CheckAgent(agent.ID, "task_submit") {
		retry := deps.limiter().CheckAgentWithDetails(agent.ID, "task_submit").RetryAfter
		return map[string]any{"_rate_retry_after": retry},
			&ToolError{Code: "RATE_LIMIT", Message: "Rate limit exceeded",
				Details: map[string]any{"retry_after": retry}}
	}

	var in service.SubmitInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, argError("arguments must match the tool schema")
	}
	view, err := service.SubmitWork(ctx, deps.Pool, agent.ID, in, deps.AgentRefKey, time.Now())
	if err != nil {
		return nil, err
	}
	return submissionPayload(view, view.Verdict)
}

func handleWorkStatus(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		SubmissionID *int64 `json:"submission_id"`
		Code         string `json:"code"`
		RequestKey   string `json:"request_key"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, argError("arguments must match the tool schema")
	}
	if in.SubmissionID == nil && (in.Code == "" || in.RequestKey == "") {
		return nil, argError("give submission_id or code + request_key")
	}
	return service.GetSubmissionStatus(ctx, deps.Pool, agent.ID, in.SubmissionID, in.Code, in.RequestKey)
}

func handleWorkHistory(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Code string `json:"code"`
		Page int    `json:"page"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, argError("arguments must match the tool schema")
	}
	if in.Page < 1 {
		in.Page = 1
	}
	rows, total, err := service.ListHistory(ctx, deps.Pool, agent.ID, in.Code, in.Page, 20)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, err := asMap(r)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return map[string]any{"submissions": items, "total": total}, nil
}

func handleWorkReport(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" || in.Reason == "" {
		return nil, argError("code and reason are required")
	}
	return service.ReportTask(ctx, deps.Pool, agent.ID, in.Code, in.Reason)
}
