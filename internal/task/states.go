// Package task is the Task 1.0 state-machine kernel (spec:
// docs/task-spec-1.0.md). It defines the state enums of the three
// state machines (§4 Task, §5.2 Claim, §5.4 Submission) and their
// legal transitions as pure functions — no IO, no repository
// dependency. The repository layer is the only writer of these
// states and validates every change through the Transition
// functions here.
package task

// Task statuses — spec §4.
const (
	TaskDraft  = "draft"
	TaskOpen   = "open"
	TaskPaused = "paused"
	TaskClosed = "closed"
)

// TaskStatuses is the complete §4 enum, for validation and tests.
var TaskStatuses = []string{TaskDraft, TaskOpen, TaskPaused, TaskClosed}

// Claim statuses — spec §5.2.
const (
	ClaimActive   = "active"
	ClaimUsed     = "used"
	ClaimExpired  = "expired"
	ClaimReleased = "released"
)

// ClaimStatuses is the complete §5.2 enum.
var ClaimStatuses = []string{ClaimActive, ClaimUsed, ClaimExpired, ClaimReleased}

// Submission states — spec §5.4.
const (
	SubDelivering = "delivering"
	SubUncertain  = "uncertain"
	SubSettled    = "settled"
	SubRejected   = "rejected"
	SubFailed     = "failed"
)

// SubmissionStates is the complete §5.4 enum.
var SubmissionStates = []string{
	SubDelivering, SubUncertain,
	SubSettled, SubRejected, SubFailed,
}

// SubmissionTerminal reports whether a §5.4 state is terminal
// (settled, rejected or failed leave no outgoing transition).
func SubmissionTerminal(state string) bool {
	switch state {
	case SubSettled, SubRejected, SubFailed:
		return true
	}
	return false
}
