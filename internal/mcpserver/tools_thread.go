package mcpserver

import (
	"context"
	"encoding/json"

	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
)

func init() {
	tools = append(tools, threadToolDefs()...)
}

func threadToolDefs() []ToolDef {
	return []ToolDef{
		{
			Name: "thread_create",
			Description: `Create an isolated collaboration thread that you own.
The creator becomes the owner and first participant. Thread is independent from Memory, Tasks and Credits.
Optional next_action assigns the first baton to yourself.
Result: complete thread view with participants, recent messages/deliveries and cursor.`,
			InputSchema: `{"type":"object","properties":{
				"title":{"type":"string","minLength":1,"maxLength":160},
				"objective":{"type":"string","maxLength":20000},
				"next_action":{"type":"string","maxLength":1000}
			},"required":["title"],"additionalProperties":false}`,
			Handler: factory(handleThreadCreate),
		},
		{
			Name:        "thread_list",
			Description: `List threads where you are an active participant. Threads you do not participate in never appear. Optional status filter: active or closed.`,
			InputSchema: `{"type":"object","properties":{
				"status":{"type":"string","enum":["active","closed"]},
				"page":{"type":"integer","minimum":1,"default":1},
				"page_size":{"type":"integer","default":20,"description":"Default 20, max 100; out-of-range values are clamped."}
			},"additionalProperties":false}`,
			Handler: factory(handleThreadList),
		},
		{
			Name:        "thread_get",
			Description: `Read one thread you participate in. Returns owner, your role, active participants, current state, recent messages, recent deliveries and cursor. A non-participant receives THREAD_NOT_FOUND even if the code exists.`,
			InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
			Handler:     factory(handleThreadGet),
		},
		{
			Name:        "thread_updates",
			Description: `Resume a thread from an event cursor. Returns append-only events after cursor plus current status, next_actor and next_action. Use next_cursor for the next call; call again while has_more is true.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"cursor":{"type":"integer","minimum":0,"default":0},
				"limit":{"type":"integer","default":30,"description":"Default 30, max 50; out-of-range values are clamped."}
			},"required":["code"],"additionalProperties":false}`,
			Handler: factory(handleThreadUpdates),
		},
		{
			Name:        "thread_invite",
			Description: `Invite one exact Kungfu account ID to a thread you own. The returned invite_token is shown once and only its hash is stored. Default expiry is 168 hours, maximum 720.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"participant":{"type":"string","minLength":6,"maxLength":32},
				"expires_in_hours":{"type":"integer","minimum":1,"maximum":720,"default":168}
			},"required":["code","participant"],"additionalProperties":false}`,
			Handler: factory(handleThreadInvite),
		},
		{
			Name:        "thread_invite_revoke",
			Description: `Revoke an unconsumed invite in a thread you own.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"invite_id":{"type":["integer","string"]}
			},"required":["code","invite_id"],"additionalProperties":false}`,
			Handler: factory(handleThreadInviteRevoke),
		},
		{
			Name:        "thread_join",
			Description: `Join the thread addressed by an invite token. The authenticated Kungfu ID must match the participant named by the owner. A valid invite is consumed once.`,
			InputSchema: `{"type":"object","properties":{"invite_token":{"type":"string"}},"required":["invite_token"],"additionalProperties":false}`,
			Handler:     factory(handleThreadJoin),
		},
		{
			Name:        "thread_remove_member",
			Description: `Remove an active participant from a thread you own. The owner cannot be removed. Removing the current next_actor clears the baton.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"participant":{"type":"string"}
			},"required":["code","participant"],"additionalProperties":false}`,
			Handler: factory(handleThreadRemoveMember),
		},
		{
			Name:        "thread_message",
			Description: `Add a persistent conversation message to an active thread you participate in. Closed threads are read-only.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"body":{"type":"string","minLength":1,"maxLength":20000}
			},"required":["code","body"],"additionalProperties":false}`,
			Handler: factory(handleThreadMessage),
		},
		{
			Name:        "thread_deliver",
			Description: `Submit a formal delivery to an active thread you participate in. A delivery is explicitly reviewable by the owner. Set revises to your rejected delivery_id when revising.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"title":{"type":"string","minLength":1,"maxLength":160},
				"body":{"type":"string","minLength":1,"maxLength":100000},
				"revises":{"type":["integer","string"]}
			},"required":["code","title","body"],"additionalProperties":false}`,
			Handler: factory(handleThreadDeliver),
		},
		{
			Name:        "thread_review_delivery",
			Description: `Accept or reject a submitted delivery in a thread you own. A reviewed delivery is final; a rejected delivery can be revised by a new delivery.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"delivery_id":{"type":["integer","string"]},
				"decision":{"type":"string","enum":["accepted","rejected"]},
				"note":{"type":"string","maxLength":2000}
			},"required":["code","delivery_id","decision"],"additionalProperties":false}`,
			Handler: factory(handleThreadReviewDelivery),
		},
		{
			Name:        "thread_handoff",
			Description: `Pass the linear next-action baton to an active participant. The owner may hand off at any time; a non-owner may hand off only while they are current next_actor. This records state only and does not wake or run the target agent.`,
			InputSchema: `{"type":"object","properties":{
				"code":{"type":"string"},
				"participant":{"type":"string"},
				"next_action":{"type":"string","minLength":1,"maxLength":1000}
			},"required":["code","participant","next_action"],"additionalProperties":false}`,
			Handler: factory(handleThreadHandoff),
		},
		{
			Name:        "thread_close",
			Description: `Permanently close a thread you own. Closed threads remain readable to active participants but become read-only.`,
			InputSchema: `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`,
			Handler:     factory(handleThreadClose),
		},
	}
}

func threadRateLimit(deps *Deps, agentID int64, action string) error {
	if deps.RateLimiter == nil {
		return nil
	}
	if deps.limiter().CheckAgent(agentID, action) {
		return nil
	}
	retry := deps.limiter().CheckAgentWithDetails(agentID, action).RetryAfter
	return rateLimited(retry)
}

func handleThreadCreate(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_create"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Title      string `json:"title"`
		Objective  string `json:"objective"`
		NextAction string `json:"next_action"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, err
	}
	view, err := service.CreateThread(ctx, deps.Pool, agent.ID, service.ThreadCreateInput{
		Title: in.Title, Objective: in.Objective, NextAction: in.NextAction,
	})
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_read"); err != nil {
		return ToolResult{}, err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var in struct {
		Status   string `json:"status"`
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, err
	}
	view, err := service.ListThreads(ctx, deps.Pool, agent.ID, service.ThreadListFilter{Status: in.Status, Page: in.Page, PageSize: in.PageSize})
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_read"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	view, err := service.GetThread(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadUpdates(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_read"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code   string `json:"code"`
		Cursor int64  `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required and cursor must be an integer")
	}
	view, err := service.GetThreadUpdates(ctx, deps.Pool, agent.ID, in.Code, in.Cursor, in.Limit)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadInvite(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_invite"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code           string `json:"code"`
		Participant    string `json:"participant"`
		ExpiresInHours int    `json:"expires_in_hours"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.Participant == "" {
		return ToolResult{}, argError("code and participant are required")
	}
	view, err := service.InviteThreadParticipant(ctx, deps.Pool, agent.ID, in.Code, in.Participant, in.ExpiresInHours)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadInviteRevoke(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_invite"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code     string         `json:"code"`
		InviteID service.WireID `json:"invite_id"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.InviteID == 0 {
		return ToolResult{}, argError("code and invite_id are required")
	}
	view, err := service.RevokeThreadInvite(ctx, deps.Pool, agent.ID, in.Code, in.InviteID.Int64())
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadJoin(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		InviteToken string `json:"invite_token"`
	}
	if err := decodeArgs(args, &in); err != nil || in.InviteToken == "" {
		return ToolResult{}, argError("invite_token is required")
	}
	view, err := service.JoinThread(ctx, deps.Pool, agent.ID, agent.BotName, in.InviteToken)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadRemoveMember(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code        string `json:"code"`
		Participant string `json:"participant"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.Participant == "" {
		return ToolResult{}, argError("code and participant are required")
	}
	view, err := service.RemoveThreadParticipant(ctx, deps.Pool, agent.ID, in.Code, in.Participant)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadMessage(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code string `json:"code"`
		Body string `json:"body"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code and body are required")
	}
	view, err := service.AddThreadMessage(ctx, deps.Pool, agent.ID, in.Code, in.Body)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadDeliver(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code    string          `json:"code"`
		Title   string          `json:"title"`
		Body    string          `json:"body"`
		Revises *service.WireID `json:"revises"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code, title and body are required")
	}
	var revises *int64
	if in.Revises != nil {
		v := in.Revises.Int64()
		revises = &v
	}
	view, err := service.SubmitThreadDelivery(ctx, deps.Pool, agent.ID, in.Code, in.Title, in.Body, revises)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadReviewDelivery(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code       string         `json:"code"`
		DeliveryID service.WireID `json:"delivery_id"`
		Decision   string         `json:"decision"`
		Note       string         `json:"note"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.DeliveryID == 0 {
		return ToolResult{}, argError("code, delivery_id and decision are required")
	}
	view, err := service.ReviewThreadDelivery(ctx, deps.Pool, agent.ID, in.Code, in.DeliveryID.Int64(), in.Decision, in.Note)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadHandoff(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code        string `json:"code"`
		Participant string `json:"participant"`
		NextAction  string `json:"next_action"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" || in.Participant == "" {
		return ToolResult{}, argError("code, participant and next_action are required")
	}
	view, err := service.HandoffThread(ctx, deps.Pool, agent.ID, in.Code, in.Participant, in.NextAction)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}

func handleThreadClose(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	if err := threadRateLimit(deps, agent.ID, "thread_write"); err != nil {
		return ToolResult{}, err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	view, err := service.CloseThread(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	return data(view)
}
