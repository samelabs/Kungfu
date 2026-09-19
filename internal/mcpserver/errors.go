package mcpserver

import (
	"context"

	mcpsdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	apperr "kungfu.md/internal/errors"

	"kungfu.md/internal/model"
)

// toolError is a safe, tool-visible application error. Only its code
// and message cross the MCP boundary — never SQL errors, stack
// traces, filesystem paths, cookies, keys, or secrets.
type toolError struct {
	httpStatus int
	code       string
	message    string
}

func (e *toolError) Error() string { return e.code + ": " + e.message }

// mapAppError converts existing AppError values into safe tool
// errors, exposing the stable application code/message; anything else
// collapses to the canonical INTERNAL_ERROR.
func mapAppError(err error) error {
	if ae, ok := apperr.IsAppError(err); ok {
		return &toolError{httpStatus: ae.HTTPCode, code: ae.Code, message: ae.Message}
	}
	return &toolError{httpStatus: 500, code: "INTERNAL_ERROR", message: "An internal error occurred"}
}

// resolveVerified resolves the bot identity from the credential
// already verified by the outer auth middleware. The MCP context
// carries the verified *model.Bot; if absent (miswiring), fail closed.
// resolveVerified reads the bot identity the official bearer
// middleware stamped into the request context (the SDK propagates the
// HTTP request context into tool handlers). Fail closed if absent.
func (d *Deps) resolveVerified(ctx context.Context) (*model.Bot, error) {
	if ti := mcpsdkauth.TokenInfoFromContext(ctx); ti != nil {
		if bot, ok := ti.Extra["verified_bot"].(*model.Bot); ok && bot != nil {
			return bot, nil
		}
	}
	return nil, &toolError{httpStatus: 401, code: "INVALID_KEY", message: "API Key is invalid or expired"}
}
