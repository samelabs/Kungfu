package mcpserver

// NextAction — spec §8.3: the action an executor should take next is
// a pure function of the submission state (with its verdict) or the
// not-accepted error code. The retry_after values come from §11 and
// the work order decision record: delivering 5s, uncertain 30s,
// under_review 60s, failed 60s, CLAIM_INVALID 0s. RATE_LIMIT's
// retry_after is the limiter's remaining seconds and is filled in by
// the caller (NextAction returns nil there).

import (
	"kungfu.md/internal/task"
)

// Retry-after seconds (§11 / decision record).
const (
	retryAfterDelivering   = 5
	retryAfterUncertain    = 30
	retryAfterUnderReview  = 60
	retryAfterFailed       = 60
	retryAfterClaimInvalid = 0
)

func iptr(i int) *int { return &i }

// NextAction decides the §8.3 table. Exactly one of (state path) and
// (errCode path) applies: a non-empty errCode selects the error rows;
// otherwise the submission state decides. Uncovered combinations
// return ("", nil) — the field is null.
func NextAction(state string, verdict *task.Verdict, errCode string) (string, *int) {
	if errCode != "" {
		switch errCode {
		case "RATE_LIMIT":
			return "wait", nil // retry_after = limiter remainder (caller)
		case "CLAIM_INVALID":
			return "retry", iptr(retryAfterClaimInvalid)
		case "CLAIM_REQUIRED":
			return "retry", nil // §8.4: retry (claim first)
		case "SCHEMA_MISMATCH", "CREDENTIAL_IN_PAYLOAD", "PAYLOAD_TOO_LARGE",
			"IDEMPOTENCY_CONFLICT", "INVALID_REVISES", "INVALID_REQUEST_KEY":
			return "revise", nil
		case "TASK_NOT_OPEN", "SLOTS_EXHAUSTED", "SUBMISSION_LIMIT", "OWN_TASK", "TASK_NOT_FOUND":
			return "stop", nil
		}
		return "", nil
	}

	switch state {
	case task.SubSettled:
		return "done", nil
	case task.SubDelivering:
		return "poll", iptr(retryAfterDelivering)
	case task.SubUncertain:
		return "poll", iptr(retryAfterUncertain)
	case task.SubUnderReview:
		return "poll", iptr(retryAfterUnderReview)
	case task.SubFailed:
		return "retry", iptr(retryAfterFailed)
	case task.SubRejected:
		retryable := true // §6.1: retryable 缺省 true
		if verdict != nil {
			retryable = verdict.Retryable
		}
		if retryable {
			return "revise", nil
		}
		return "stop", nil
	}
	return "", nil
}
