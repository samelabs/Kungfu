package mcpserver

// Account + memory registry tools (WO-7d): the same ToolDef registry
// as the task tools, same §8.2 envelope, both channels (MCP and
// POST /api/v1). account_register is Public (the only anonymous tool);
// every other tool resolves the verified identity from the Bearer
// credential. Handlers reuse the existing service authorities and the
// legacy projection functions in contracts_memory_work.go — no
// duplicated business rules.

import (
	"context"
	"encoding/json"

	"kungfu.md/internal/model"
	"kungfu.md/internal/service"
)

func handleAccountRegister(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Name == "" || in.Password == "" {
		return ToolResult{}, argError("name and password are required")
	}
	if err := deps.limitRegister(ctx); err != nil {
		return ToolResult{}, err
	}
	reg := deps.Register
	if reg == nil {
		reg = service.Register
	}
	res, err := reg(ctx, deps.Pool, in.Name, in.Password, deps.requestIP(ctx))
	if err != nil {
		return ToolResult{}, err
	}
	// Onboarding pointers ride along with the one-time key (WO-18):
	// where to point an MCP client, the plain-HTTP alternative, the
	// docs, and what to do when the key is lost.
	return data(map[string]any{
		"bot_name":     res.BotName,
		"api_key":      res.Key, // one-time disclosure — never logged
		"mcp_endpoint": "https://kungfu.md/mcp",
		"api_base":     "https://kungfu.md/api/v1/",
		"docs":         "https://kungfu.md/llms.txt",
		"message":      res.Message,
		"key_recovery": "Store the key now. If it is lost, the owner signs in at /owner/key and resets it.",
	})
}

func handleAccountStatus(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	// The schema declares no arguments: {} (or none) is fine, anything
	// else is VALIDATION_FAILED naming the field.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var in struct{}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, err
	}
	bot := agent
	if bot == nil {
		var err error
		if bot, err = deps.resolveVerified(ctx); err != nil {
			return ToolResult{}, err
		}
	}
	if deps.AccountStatus == nil {
		return ToolResult{}, &ToolError{Code: "INTERNAL_ERROR", Message: internalErrorMessage}
	}
	st, err := deps.AccountStatus(ctx, deps.Pool, bot.ID)
	if err != nil {
		return ToolResult{}, err
	}
	return data(map[string]any{
		"bot_id":   st.BotID,
		"bot_name": st.BotName,
		"balance":  st.Balance,
		"status":   st.Status,
	})
}

// -- memory --

func handleMemoryList(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return ToolResult{}, argError("arguments must match the tool schema")
	}
	if !deps.limiter().CheckAgent(agent.ID, "list") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "list").RetryAfter)
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	limit = clampInt(limit, 1, 100)
	offset := clampInt(in.Offset, 0, 10000)
	result, err := service.ListKungfusForBot(ctx, deps.Pool, agent.ID, limit, offset)
	if err != nil {
		return ToolResult{}, err
	}
	out, perr := projectMemoryList(result)
	if perr != nil {
		return ToolResult{}, mapProjErr(perr)
	}
	return data(asMapMust(out))
}

func handleMemoryGet(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	if !deps.limiter().CheckAgent(agent.ID, "get") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "get").RetryAfter)
	}
	result, err := service.GetKungfuForBot(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	out, perr := projectMemoryGet(result)
	if perr != nil {
		return ToolResult{}, mapProjErr(perr)
	}
	return data(asMapMust(out))
}

func handleMemoryPut(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code        string   `json:"code"`
		Title       string   `json:"title"`
		Tags        []string `json:"tags"`
		Description string   `json:"description"`
		Content     string   `json:"content"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Title == "" {
		return ToolResult{}, argError("title and content are required")
	}
	if !deps.limiter().CheckAgent(agent.ID, "push") {
		return ToolResult{}, rateLimited(deps.limiter().CheckAgentWithDetails(agent.ID, "push").RetryAfter)
	}
	tags := make([]interface{}, len(in.Tags))
	for i, t := range in.Tags {
		tags[i] = t
	}
	input := map[string]interface{}{
		"title":       in.Title,
		"tags":        tags,
		"description": in.Description,
		"content":     in.Content,
	}
	if in.Code != "" {
		input["code"] = in.Code
	}
	result, err := service.Push(ctx, deps.Pool, agent.ID, input,
		deps.Limits.MaxTitleLength, deps.Limits.MaxTags, deps.Limits.MaxTagLength,
		deps.Limits.MaxDescriptionLength, deps.Limits.MaxContentSize)
	if err != nil {
		return ToolResult{}, err
	}
	return data(map[string]any{
		"code":       result.Code,
		"title":      result.Title,
		"action":     result.Action,
		"checksum":   result.Checksum,
		"visibility": result.Visibility,
	})
}

func handleMemoryShare(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	result, err := service.Share(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	out, perr := projectVisibility(result)
	if perr != nil {
		return ToolResult{}, mapProjErr(perr)
	}
	return data(asMapMust(out))
}

func handleMemoryUnshare(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	result, err := service.Unshare(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	out, perr := projectVisibility(result)
	if perr != nil {
		return ToolResult{}, mapProjErr(perr)
	}
	return data(asMapMust(out))
}

func handleMemoryDelete(ctx context.Context, deps *Deps, agent *model.Bot, args json.RawMessage) (ToolResult, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(args, &in); err != nil || in.Code == "" {
		return ToolResult{}, argError("code is required")
	}
	result, err := service.Delete(ctx, deps.Pool, agent.ID, in.Code)
	if err != nil {
		return ToolResult{}, err
	}
	out, perr := projectDelete(result)
	if perr != nil {
		return ToolResult{}, mapProjErr(perr)
	}
	return data(asMapMust(out))
}

// asMapMust projects a DTO into the envelope payload map.
func asMapMust(v any) map[string]any {
	m, err := asMap(v)
	if err != nil {
		return map[string]any{}
	}
	return m
}
