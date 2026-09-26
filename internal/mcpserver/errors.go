package mcpserver

import (
	"context"

	mcpsdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"kungfu.md/internal/model"
)

// resolveVerified reads the bot identity the official bearer
// middleware stamped into the request context (the SDK propagates the
// HTTP request context into tool handlers). Fail closed if absent.
func (d *Deps) resolveVerified(ctx context.Context) (*model.Bot, error) {
	if ti := mcpsdkauth.TokenInfoFromContext(ctx); ti != nil {
		if bot, ok := ti.Extra["verified_bot"].(*model.Bot); ok && bot != nil {
			return bot, nil
		}
	}
	return nil, &ToolError{Code: "UNAUTHORIZED", Message: "Agent key is invalid or missing"}
}
