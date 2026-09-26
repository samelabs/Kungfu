package mcpserver

// Memory tools. Pure adapter code: every tool resolves the
// verified identity from the Bearer mechanism, applies the business
// rate-limit authority through the injected limiter, calls the
// EXISTING service authorities directly (no facade, no alternate
// implementations), and projects their results into typed MCP output
// DTOs (contracts_memory_work.go). No SQL, no Credits, no
// transactions, no PostAPI.

import "context"

var _ = context.Background // context kept for helper parity

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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
