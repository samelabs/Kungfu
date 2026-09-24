package service

import (
	"net/url"
	"strings"
)

// PostAPI structural validation — the single source of truth for what makes
// a PostAPI URL structurally valid. Both the Owner write paths (create/edit/
// open) and the agent-side TaskCheck consume classifyPostAPI; neither side
// parses the URL itself.
//
// This layer is PURE STRUCTURE: it classifies why a URL is invalid and says
// nothing about HTTP status codes or API-facing wording — callers translate
// the class into their own error contracts.

// postAPIClass is the stable classification of a structurally invalid PostAPI.
type postAPIClass string

const (
	postAPIClassEmpty         postAPIClass = "EMPTY"
	postAPIClassTooLong       postAPIClass = "TOO_LONG"
	postAPIClassInvalidURL    postAPIClass = "INVALID_URL"
	postAPIClassInvalidScheme postAPIClass = "INVALID_SCHEME"
)

// classifyPostAPI applies the structural rules:
//  1. non-empty
//  2. at most maxLength bytes
//  3. parses as a URL
//  4. host component present
//  5. scheme is http or https
//
// Returns ("", "") when the URL is structurally valid; otherwise the class
// and a short neutral reason (for logs/debug, not for API responses).
func classifyPostAPI(postapi string, maxLength int) (postAPIClass, string) {
	if postapi == "" {
		return postAPIClassEmpty, "postapi is empty"
	}
	if len(postapi) > maxLength {
		return postAPIClassTooLong, "postapi exceeds maximum length"
	}
	parsed, err := url.Parse(postapi)
	if err != nil || parsed.Host == "" {
		return postAPIClassInvalidURL, "postapi is not a valid URL"
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return postAPIClassInvalidScheme, "postapi must use http or https"
	}
	return "", ""
}
