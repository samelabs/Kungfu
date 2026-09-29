package mcpserver

// Table-driven coverage of the §8.3 next_action table and the §8.4
// executor-side error rows, plus the protocol-layer code→HTTP status
// table.

import (
	"net/http"
	"testing"

	"kungfu.md/internal/task"
)

func TestNextActionStates(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		errCode    string
		wantAction string
		wantRetry  *int
	}{
		// §8.3 state rows
		{"settled done", task.SubSettled, "", "done", nil},
		{"delivering poll 5s", task.SubDelivering, "", "poll", iptr(5)},
		{"uncertain poll 30s", task.SubUncertain, "", "poll", iptr(30)},
		{"failed stop", task.SubFailed, "", "stop", nil},         // the publisher's side failed
		{"rejected revise", task.SubRejected, "", "revise", nil}, // stop once no rejections are left (handler)
		// §8.3 error rows (executor-side §8.4 codes)
		{"RATE_LIMIT wait", "", "RATE_LIMIT", "wait", nil}, // retry_after = limiter remainder
		{"CLAIM_INVALID retry 0s", "", "CLAIM_INVALID", "retry", iptr(0)},
		{"SCHEMA_MISMATCH revise", "", "SCHEMA_MISMATCH", "revise", nil},
		{"CREDENTIAL_IN_PAYLOAD revise", "", "CREDENTIAL_IN_PAYLOAD", "revise", nil},
		{"PAYLOAD_TOO_LARGE revise", "", "PAYLOAD_TOO_LARGE", "revise", nil},
		{"IDEMPOTENCY_CONFLICT revise", "", "IDEMPOTENCY_CONFLICT", "revise", nil},
		{"INVALID_REQUEST_KEY revise", "", "INVALID_REQUEST_KEY", "revise", nil},
		{"INVALID_REVISES revise", "", "INVALID_REVISES", "revise", nil},
		{"TASK_NOT_OPEN stop", "", "TASK_NOT_OPEN", "stop", nil},
		{"SLOTS_EXHAUSTED stop", "", "SLOTS_EXHAUSTED", "stop", nil},
		{"SUBMISSION_LIMIT stop", "", "SUBMISSION_LIMIT", "stop", nil},
		{"OWN_TASK stop", "", "OWN_TASK", "stop", nil},
		{"TASK_NOT_FOUND stop", "", "TASK_NOT_FOUND", "stop", nil},
		// uncovered → null
		{"unknown code null", "", "SOMETHING_ELSE", "", nil},
		{"no state null", "", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, retry := NextAction(tc.state, tc.errCode)
			if action != tc.wantAction {
				t.Fatalf("action = %q, want %q", action, tc.wantAction)
			}
			switch {
			case tc.wantRetry == nil && retry != nil:
				t.Fatalf("retry_after = %v, want nil", *retry)
			case tc.wantRetry != nil && retry == nil:
				t.Fatalf("retry_after = nil, want %d", *tc.wantRetry)
			case tc.wantRetry != nil && *retry != *tc.wantRetry:
				t.Fatalf("retry_after = %d, want %d", *retry, *tc.wantRetry)
			}
		})
	}
}

func TestHTTPStatusTable(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"UNAUTHORIZED", 401},
		{"RATE_LIMIT", 429},
		{"TASK_NOT_FOUND", 404},
		{"SUBMISSION_NOT_FOUND", 404},
		{"HARNESS_REF_NOT_FOUND", 404},
		{"UNKNOWN_TOOL", 404},
		{"OWN_TASK", 403},
		{"NOT_OWNER", 403},
		{"INSUFFICIENT_CREDITS", 402},
		{"PAYLOAD_TOO_LARGE", 413},
		{"TASK_NOT_OPEN", 409},
		{"SUBMISSION_LIMIT", 409},
		{"CLAIM_REQUIRED", 409},
		{"CLAIM_INVALID", 409},
		{"IDEMPOTENCY_CONFLICT", 409},
		{"INVALID_STATE", 409},
		{"HAS_RESERVATIONS", 409},
		{"NOTHING_TO_REFUND", 409},
		{"SCHEMA_MISMATCH", 422},
		{"CREDENTIAL_IN_PAYLOAD", 422},
		{"INVALID_REVISES", 422},
		{"INVALID_REQUEST_KEY", 422},
		{"VALIDATION_FAILED", 422},
		{"INTERNAL_ERROR", 500},
		{"SOMETHING_UNLISTED", 500}, // everything unlisted collapses to 500
	}
	for _, tc := range cases {
		if got := HTTPStatusFor(tc.code); got != tc.want {
			t.Fatalf("HTTPStatusFor(%s) = %d, want %d", tc.code, got, tc.want)
		}
	}
	// the only 429: the single source table must not map anything
	// else to 429 — verify by sampling every code we know of
	for _, code := range []string{"UNAUTHORIZED", "RATE_LIMIT", "TASK_NOT_FOUND", "OWN_TASK",
		"VALIDATION_FAILED", "INTERNAL_ERROR", "NAME_TAKEN", "PRIVATE_KUNGFU"} {
		got := HTTPStatusFor(code)
		if got == http.StatusTooManyRequests && code != "RATE_LIMIT" {
			t.Fatalf("%s also maps to 429; RATE_LIMIT must be the only one", code)
		}
	}
}

func TestNormalizeToolErrorMasksInternals(t *testing.T) {
	te := normalizeToolError(errString("boom: connection refused at 10.0.0.1"))
	if te.Code != "INTERNAL_ERROR" || te.Message != "An internal error occurred" {
		t.Fatalf("unlisted error leaked: %+v", te)
	}
}

func errString(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }
