package mcpserver

// Publisher registry tools (WO-7b) — §8.1 publisher column over the
// Task 1.0 service layer. The caller IS the publisher. Publisher tools
// return the same §8.2 envelope; next_action is always null (there is
// no executor next action for a publisher operation).

import (
	"context"
	"encoding/json"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

// contractArg mirrors the §3 contract JSON in tool arguments.
type contractArg = task.Contract

func handleTaskCreate(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	// §11: 20 creates per hour per publisher. Publisher tools carry a
	// null next_action (WO-7b), so NoAction overrides RATE_LIMIT→wait.
	if !deps.limiter().CheckAgent(agent.ID, "task_create") {
		retry := deps.limiter().CheckAgentWithDetails(agent.ID, "task_create").RetryAfter
		return ToolResult{NoAction: true}, &ToolError{Code: "RATE_LIMIT", Message: "Rate limit exceeded",
			Details: map[string]any{"retry_after": retry}}
	}
	var in struct {
		Contract contractArg `json:"contract"`
		Budget   int64       `json:"budget"`
		Open     bool        `json:"open"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	view, err := service.CreateTask(ctx, deps.Pool, agent.ID, in.Contract, in.Budget)
	if err != nil {
		return ToolResult{}, err
	}
	if in.Open {
		// §8.1: open in the same call; a failed open leaves the task a
		// draft with the budget locked (task_close + task_refund
		// recover it) and returns that error.
		code, _ := view["code"].(string)
		view, err = service.OpenTask(ctx, deps.Pool, agent.ID, code)
		if err != nil {
			return ToolResult{}, err
		}
	}
	return dataView(view)
}

func handleTaskUpdate(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code     string      `json:"code"`
		Contract contractArg `json:"contract"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code and contract are required")
	}
	view, err := service.UpdateTask(ctx, deps.Pool, agent.ID, in.Code, in.Contract)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskOpen(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	code, err := codeOnly(args)
	if err != nil {
		return ToolResult{}, err
	}
	view, err := service.OpenTask(ctx, deps.Pool, agent.ID, code)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskPause(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	code, err := codeOnly(args)
	if err != nil {
		return ToolResult{}, err
	}
	view, err := service.PauseTask(ctx, deps.Pool, agent.ID, code)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskClose(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	code, err := codeOnly(args)
	if err != nil {
		return ToolResult{}, err
	}
	view, err := service.CloseTask(ctx, deps.Pool, agent.ID, code)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskFund(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code   string `json:"code"`
		Amount int64  `json:"amount"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" || in.Amount == 0 {
		return ToolResult{}, argError("code and amount are required")
	}
	view, err := service.FundTask(ctx, deps.Pool, agent.ID, in.Code, in.Amount)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskRefund(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	code, err := codeOnly(args)
	if err != nil {
		return ToolResult{}, err
	}
	view, err := service.RefundTask(ctx, deps.Pool, agent.ID, code)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	code, err := codeOnly(args)
	if err != nil {
		return ToolResult{}, err
	}
	view, err := service.GetTask(ctx, deps.Pool, agent.ID, code)
	if err != nil {
		return ToolResult{}, err
	}
	return dataView(view)
}

func handleTaskList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	rows, err := service.ListTasks(ctx, deps.Pool, agent.ID)
	if err != nil {
		return ToolResult{}, err
	}
	return data(map[string]any{"tasks": rows, "total": len(rows)})
}

func handleTaskSubmissions(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code     string `json:"code"`
		State    string `json:"state"`
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	if in.Page < 1 {
		in.Page = 1
	}
	if in.PageSize < 1 || in.PageSize > 100 {
		in.PageSize = 20
	}
	rows, total, err := service.ListSubmissionsForPublisher(ctx, deps.Pool, agent.ID, in.Code, in.State, in.Page, in.PageSize, deps.AgentRefKey)
	if err != nil {
		return ToolResult{}, err
	}
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		m, err := asMap(rows[i])
		if err != nil {
			return ToolResult{}, err
		}
		items = append(items, m)
	}
	return data(map[string]any{"submissions": items, "total": total})
}

func handleTaskVerdict(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		SubmissionID service.WireID  `json:"submission_id"` // string or integer on the wire
		Verdict      json.RawMessage `json:"verdict"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, argError("submission_id must be an integer or a numeric string")
	}
	if in.SubmissionID == 0 || len(in.Verdict) == 0 {
		return ToolResult{}, argError("submission_id and verdict are required")
	}
	view, err := service.SubmitVerdict(ctx, deps.Pool, agent.ID, in.SubmissionID.Int64(), in.Verdict, time.Now())
	if err != nil {
		return ToolResult{}, err
	}
	m, err := asMap(view)
	if err != nil {
		return ToolResult{}, err
	}
	return data(m) // publisher tools: next_action is always null
}

func codeOnly(args json.RawMessage) (string, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == "" {
		return "", argError("code is required")
	}
	return in.Code, nil
}

// dataView projects any service view into a NoAction ToolResult
// (publisher tools never carry a next_action).
func dataView(v any) (ToolResult, error) {
	m, err := asMap(v)
	if err != nil {
		return ToolResult{}, err
	}
	return data(m)
}
