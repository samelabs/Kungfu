package service

import (
	"fmt"
	"net/url"
	"strings"

	"kungfu.md/internal/errors"
)

// TaskCheckRule defines the four-tuple for each validation rule.
// Each rule has: [http_code, error_code, agent_message, log_message]
// agent_message is what the agent sees; log_message is what goes to the DB log.
type TaskCheckRule struct {
	HTTPCode int
	Code     string
	AgentMsg string
	LogMsg   string
}

var taskCheckRules = map[string]TaskCheckRule{
	"POSTAPI_EMPTY":          {503, "TASK_NOT_CONFIGURED", "Task postapi is not configured", "Task check: Post API is empty"},
	"POSTAPI_TOO_LONG":       {500, "TASK_CONFIG_INVALID", "Task postapi exceeds maximum length", "Task check: Post API exceeds maximum length"},
	"POSTAPI_INVALID_URL":    {500, "TASK_CONFIG_INVALID", "Task postapi is not a valid URL", "Task check: Post API is not a valid URL"},
	"POSTAPI_INVALID_SCHEME": {500, "TASK_CONFIG_INVALID", "Task postapi must use http or https", "Task check: Post API must use http or https"},
	"PRICE_INVALID":          {500, "TASK_CONFIG_INVALID", "Task price must be greater than zero", "Task check: price must be greater than zero"},
	"TASK_NOT_OPEN":          {409, "TASK_NOT_OPEN", "Task is not open for submissions", "Task check: task is not open for submissions"},
	"TASK_BUDGET_EXHAUSTED":  {409, "TASK_BUDGET_EXHAUSTED", "Task budget is not enough for this submission", "Task check: task budget is not enough"},
}

// TaskCheckError wraps a rule with its details.
type TaskCheckError struct {
	Rule    TaskCheckRule
	Details map[string]interface{}
}

func (e *TaskCheckError) Error() string {
	return fmt.Sprintf("%s: %s", e.Rule.Code, e.Rule.AgentMsg)
}

// RaiseRule raises a TaskCheckError for the given rule ID.
func RaiseRule(ruleID string, details ...map[string]interface{}) *TaskCheckError {
	rule, ok := taskCheckRules[ruleID]
	if !ok {
		return &TaskCheckError{
			Rule: TaskCheckRule{
				HTTPCode: 500,
				Code:     "INTERNAL_ERROR",
				AgentMsg: "Task check rule is not configured",
				LogMsg:   "Task check: unknown rule " + ruleID,
			},
		}
	}
	var d map[string]interface{}
	if len(details) > 0 {
		d = details[0]
	}
	return &TaskCheckError{Rule: rule, Details: d}
}

// ToAppError converts a TaskCheckError to an AppError.
func (e *TaskCheckError) ToAppError() *errors.AppError {
	ae := errors.New(e.Rule.HTTPCode, e.Rule.Code, e.Rule.AgentMsg)
	ae.Details = e.Details
	return ae
}

// RunTaskCheck validates postapi and price, then calls the budget checker.
func RunTaskCheck(postapi string, price float64, budgetChecker func() *TaskCheckError) *TaskCheckError {
	if e := ValidatePostapi(postapi, 2048); e != nil {
		return e
	}
	if e := ValidatePrice(price); e != nil {
		return e
	}
	if e := budgetChecker(); e != nil {
		return e
	}
	return nil
}

// ValidatePostapi validates the postapi URL.
func ValidatePostapi(postapi string, maxLength int) *TaskCheckError {
	if postapi == "" {
		return RaiseRule("POSTAPI_EMPTY")
	}
	if len(postapi) > maxLength {
		return RaiseRule("POSTAPI_TOO_LONG")
	}

	// Use Go's url.Parse to validate the URL and require a host component.
	parsed, err := url.Parse(postapi)
	if err != nil || parsed.Host == "" {
		return RaiseRule("POSTAPI_INVALID_URL")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return RaiseRule("POSTAPI_INVALID_SCHEME")
	}

	return nil
}

// ValidatePrice validates that price is > 0.
func ValidatePrice(price float64) *TaskCheckError {
	if price <= 0 {
		return RaiseRule("PRICE_INVALID")
	}
	return nil
}
