package mcpserver

// M2 memory + work tools. Pure adapter code: every tool resolves the
// verified identity from the M1 Bearer mechanism, applies the SAME
// RateLimiter actions as REST, and delegates to the existing service
// authorities. No SQL, no Credits, no transactions, no PostAPI.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/service"
)

// -- shared seams (injected from server composition; never repository) --

// listKungfusFn matches service.ListKungfusForBot.
type listKungfusFn = func(ctx context.Context, q pg.Querier, botID int64, limit, offset int) (map[string]interface{}, error)

// getKungfuFn matches service.GetKungfuForBot.
type getKungfuFn = func(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error)

// pushKungfuFn matches service.Push.
type pushKungfuFn = func(ctx context.Context, pool *pg.Pool, botID int64, input map[string]interface{},
	maxTitleLen, maxTags, maxTagLen, maxDescLen, maxContentSize int) (*service.KungfuPushResult, error)

// createTaskFn matches service.CreateTask.
type createTaskFn = func(ctx context.Context, pool *pg.Pool, botID int64, cfg *service.OwnerTaskConfig, input *service.CreateTaskInput) (map[string]interface{}, error)

// checkAPIFn matches ratelimit.Limiter.CheckAPI (same actions as REST).
type checkAPIFn = func(botID int64, action string) bool

// ContentLimits are the existing Config values supplied by the server
// composition layer — no MCP-specific configuration exists.
type ContentLimits struct {
	MaxTitleLength       int
	MaxTags              int
	MaxTagLength         int
	MaxDescriptionLength int
	MaxContentSize       int
}

// MemoryWorkDeps extends Deps with the service seams M2 needs. All are
// existing service functions injected by internal/server; mcpserver
// keeps zero repository dependency.
type MemoryWorkDeps struct {
	ListKungfus   listKungfusFn
	GetKungfu     getKungfuFn
	PushKungfu    pushKungfuFn
	ShareKungfu   func(ctx context.Context, q pg.Querier, botID int64, code string) (map[string]interface{}, error)
	UnshareKungfu func(ctx context.Context, q pg.Querier, botID int64, code string) (map[string]interface{}, error)
	DeleteKungfu  func(ctx context.Context, q pg.Querier, botID int64, code string) (map[string]interface{}, error)

	ListOpenTasks func(ctx context.Context, pool *pg.Pool) (map[string]interface{}, error)
	GetOpenTask   func(ctx context.Context, pool *pg.Pool, code string) (map[string]interface{}, error)
	SubmitTask    func(ctx context.Context, pool *pg.Pool, taskCode string, botID int64, input map[string]interface{}) (*service.TaskSubmitResult, error)
	CreateTask    createTaskFn

	// CheckAPI is the existing RateLimiter authority (same actions as
	// REST: list/push/get/task_submit).
	CheckAPI checkAPIFn

	// Limits come from existing Config.
	Limits ContentLimits
}

// -- memory tools --

type memoryListInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"page size (default 50, max 100)"`
	Offset int `json:"offset,omitempty" jsonschema:"pagination offset (min 0, max 10000)"`
}

func addMemoryTools(s *mcp.Server, deps Deps, mw MemoryWorkDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_list",
		Description: "List your stored Kungfu memories.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in memoryListInput) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		if mw.CheckAPI != nil && !mw.CheckAPI(bot.ID, "list") {
			return nil, nil, &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Rate limit exceeded"}
		}
		// Same pagination contract as REST.
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		limit = clampInt(limit, 1, 100)
		offset := clampInt(in.Offset, 0, 10000)
		result, err := mw.ListKungfus(ctx, deps.Pool, bot.ID, limit, offset)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
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
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		if mw.CheckAPI != nil && !mw.CheckAPI(bot.ID, "get") {
			return nil, nil, &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Rate limit exceeded"}
		}
		result, err := mw.GetKungfu(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
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
		Title       string   `json:"title"`
		Tags        []string `json:"tags,omitempty"`
		Description string   `json:"description,omitempty"`
		Content     string   `json:"content"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		if mw.CheckAPI != nil && !mw.CheckAPI(bot.ID, "push") {
			return nil, nil, &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Rate limit exceeded"}
		}
		// Adapter ONLY reshapes typed MCP input into the existing
		// service input representation ([]string -> []interface{},
		// matching what JSON decoding produces for REST); all
		// validation stays in service.Push with the injected Config
		// limits.
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
		result, err := mw.PushKungfu(ctx, deps.Pool, bot.ID, input,
			mw.Limits.MaxTitleLength, mw.Limits.MaxTags, mw.Limits.MaxTagLength,
			mw.Limits.MaxDescriptionLength, mw.Limits.MaxContentSize)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, map[string]interface{}{
			"code":       result.Code,
			"title":      result.Title,
			"action":     result.Action,
			"checksum":   result.Checksum,
			"visibility": result.Visibility,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_share",
		Description: "Make one of your memories publicly readable.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  false,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		result, err := mw.ShareKungfu(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_unshare",
		Description: "Revoke public access to one of your memories.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  false,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		result, err := mw.UnshareKungfu(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
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
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		result, err := mw.DeleteKungfu(ctx, deps.Pool, bot.ID, in.Code)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
	})
}

// -- work tools --

func addWorkTools(s *mcp.Server, deps Deps, mw MemoryWorkDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_list",
		Description: "List currently open and fundable work.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, map[string]interface{}, error) {
		if _, err := deps.resolveVerified(ctx); err != nil {
			return nil, nil, err
		}
		result, err := mw.ListOpenTasks(ctx, deps.Pool)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_get",
		Description: "Get one open work item by code. No assignment or reservation is created.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code string `json:"code"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		if _, err := deps.resolveVerified(ctx); err != nil {
			return nil, nil, err
		}
		result, err := mw.GetOpenTask(ctx, deps.Pool, in.Code)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_submit",
		Description: "Submit your completed work result for a task. The result is delivered to the task owner's configured PostAPI endpoint.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true), // sends data to an external endpoint
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Code    string                 `json:"code"`
		Payload map[string]interface{} `json:"payload" jsonschema:"your task result body"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		if mw.CheckAPI != nil && !mw.CheckAPI(bot.ID, "task_submit") {
			return nil, nil, &toolError{httpStatus: 429, code: "RATE_LIMIT", message: "Rate limit exceeded"}
		}
		// The payload is the Agent's result body, passed as-is to the
		// existing Submit authority. delivery.BuildPayload (inside the
		// service) remains the sole component that adds task_code.
		result, err := mw.SubmitTask(ctx, deps.Pool, in.Code, bot.ID, in.Payload)
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, map[string]interface{}{
			"task_code": result.TaskCode,
			"post":      result.Post,
			"billing":   result.Billing,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_publish",
		Description: "Publish new work to the task market, funded from your own account balance.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Title        string  `json:"title"`
		Requirements string  `json:"requirements"`
		PostAPI      string  `json:"postapi" jsonschema:"https URL that receives completed work results"`
		Budget       float64 `json:"budget"`
		Price        float64 `json:"price" jsonschema:"credits paid per successful submission"`
		OpenNow      bool    `json:"open_now" jsonschema:"open immediately (fundable) or keep pending"`
	}) (*mcp.CallToolResult, map[string]interface{}, error) {
		bot, err := deps.resolveVerified(ctx)
		if err != nil {
			return nil, nil, err
		}
		// Ownership derives ONLY from the verified credential — any
		// bot_id in the request is ignored (not even parsed into the
		// typed input). service.CreateTask owns validation, status
		// selection, lock_task debit, and the transaction.
		result, err := mw.CreateTask(ctx, deps.Pool, bot.ID,
			&service.OwnerTaskConfig{MaxTitleLength: mw.Limits.MaxTitleLength},
			&service.CreateTaskInput{
				Title:        in.Title,
				Requirements: in.Requirements,
				PostAPI:      in.PostAPI,
				Budget:       in.Budget,
				Price:        in.Price,
				OpenNow:      in.OpenNow,
			})
		if err != nil {
			return nil, nil, mapAppError(err)
		}
		return nil, result, nil
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
