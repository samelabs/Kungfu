package mcpserver

// Memory tools. Pure adapter code: every tool resolves the
// verified identity from the Bearer mechanism, applies the business
// rate-limit authority through the injected limiter, calls the
// EXISTING service authorities directly (no facade, no alternate
// implementations), and projects their results into typed MCP output
// DTOs (contracts_memory_work.go). No SQL, no Credits, no
// transactions, no PostAPI.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/service"
)

// ContentLimits is the narrow typed projection of the existing Config
// values the memory tools need. Supplied by the production server;
// no MCP defaults, no MCP env vars.
type ContentLimits struct {
	MaxTitleLength       int
	MaxTags              int
	MaxTagLength         int
	MaxDescriptionLength int
	MaxContentSize       int
}

func addMemoryTools(s *mcp.Server, deps Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_list",
		Description: "List your stored Kungfu memories.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Limit  int `json:"limit,omitempty" jsonschema:"page size (default 50, max 100)"`
		Offset int `json:"offset,omitempty" jsonschema:"pagination offset (min 0, max 10000)"`
	}) (*mcp.CallToolResult, MemoryListOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryListOutput{}, err
		}
		if !deps.limiter().CheckAgent(bot.ID, "list") {
			return nil, MemoryListOutput{}, rateLimited()
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		limit = clampInt(limit, 1, 100)
		offset := clampInt(in.Offset, 0, 10000)
		result, err := service.ListKungfusForBot(ctx, deps.Pool, bot.ID, limit, offset)
		if err != nil {
			return nil, MemoryListOutput{}, mapAppError(err)
		}
		out, perr := projectMemoryList(result)
		if perr != nil {
			return nil, MemoryListOutput{}, mapProjErr(perr)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_get",
		Description: "Get one memory by code. Owners read their own memories; other agents may read shared public memories.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, MemoryGetOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryGetOutput{}, err
		}
		if !deps.limiter().CheckAgent(bot.ID, "get") {
			return nil, MemoryGetOutput{}, rateLimited()
		}
		result, err := service.GetKungfuForBot(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, MemoryGetOutput{}, mapAppError(err)
		}
		out, perr := projectMemoryGet(result)
		if perr != nil {
			return nil, MemoryGetOutput{}, mapProjErr(perr)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_put",
		Description: "Create (no code) or update (with code) an owned memory.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code        string   `json:"code,omitempty" jsonschema:"existing memory code to update; omit to create"`
		Title       string   `json:"title" jsonschema:"short title (at most 128 characters)"`
		Tags        []string `json:"tags" jsonschema:"up to 10 tags, each at most 32 characters"`
		Description string   `json:"description,omitempty" jsonschema:"optional summary (at most 500 characters)"`
		Content     string   `json:"content" jsonschema:"the memory body: at least 50 characters, at most 100 KB; never include keys or secrets"`
	}) (*mcp.CallToolResult, MemoryPutOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryPutOutput{}, err
		}
		if !deps.limiter().CheckAgent(bot.ID, "push") {
			return nil, MemoryPutOutput{}, rateLimited()
		}
		// Adapter ONLY reshapes typed MCP input into the existing
		// service input representation ([]string -> []interface{},
		// matching JSON decoding); all validation stays in
		// service.Push with the Config limits.
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
		result, err := service.Push(ctx, deps.Pool, bot.ID, input,
			deps.Limits.MaxTitleLength, deps.Limits.MaxTags, deps.Limits.MaxTagLength,
			deps.Limits.MaxDescriptionLength, deps.Limits.MaxContentSize)
		if err != nil {
			return nil, MemoryPutOutput{}, mapAppError(err)
		}
		return nil, MemoryPutOutput{
			Code:       result.Code,
			Title:      result.Title,
			Action:     result.Action,
			Checksum:   result.Checksum,
			Visibility: result.Visibility,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_share",
		Description: "Make one of your memories publicly readable. Idempotent.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   false,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, MemoryVisibilityOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryVisibilityOutput{}, err
		}
		result, err := service.Share(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, MemoryVisibilityOutput{}, mapAppError(err)
		}
		out, perr := projectVisibility(result)
		if perr != nil {
			return nil, MemoryVisibilityOutput{}, mapProjErr(perr)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_unshare",
		Description: "Revoke public access to one of your memories. Idempotent.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   false,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, MemoryVisibilityOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryVisibilityOutput{}, err
		}
		result, err := service.Unshare(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, MemoryVisibilityOutput{}, mapAppError(err)
		}
		out, perr := projectVisibility(result)
		if perr != nil {
			return nil, MemoryVisibilityOutput{}, mapProjErr(perr)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_delete",
		Description: "Soft-delete one of your memories.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, MemoryDeleteOutput, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, MemoryDeleteOutput{}, err
		}
		result, err := service.Delete(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, MemoryDeleteOutput{}, mapAppError(err)
		}
		out, perr := projectDelete(result)
		if perr != nil {
			return nil, MemoryDeleteOutput{}, mapProjErr(perr)
		}
		return nil, out, nil
	})
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
