package task

import "fmt"

// ErrIllegalTransition is returned by every Transition function for
// a (from, event) pair that the spec's state machines do not allow.
// It is the single guard behind all state writes: the repository
// refuses to persist a transition the kernel rejects.
var ErrIllegalTransition = fmt.Errorf("illegal state transition")

// Task events — spec §4 transition table. open / pause / close are
// the publisher lifecycle; platform_pause (§4 平台暂停, receiver-fault
// pause) and platform_close (§4 平台关闭, governance close) are the
// platform rows of the same table. create / update / fund / refund
// never change the status and are therefore not events here.
const (
	EventOpen          = "open"
	EventPause         = "pause"
	EventClose         = "close"
	EventPlatformPause = "platform_pause"
	EventPlatformClose = "platform_close"
)

// Claim events — spec §5.2 table. work_claim creates a Claim in
// active (an INSERT, not a transition). renew keeps the Claim
// active and only extends expires_at (a legal self-transition).
const (
	EventClaimRenew   = "renew"
	EventClaimRelease = "release"
	EventClaimExpire  = "expire"
	EventClaimUse     = "use"
)

// Submission events — spec §5.4 state machine, edge labels mapped
// to identifiers. The §7.2 reply table is the delivery-outcome
// authority behind the deliver_* events; the failure reason values
// (RECEIVER_UNREACHABLE / RECEIVER_FAULT / RECEIVER_PROTOCOL /
// DELIVERY_UNRESOLVED) are carried by the failure column, not by
// the event.
const (
	// Creation: the first SubmissionEvent (from_state NULL → delivering).
	EventSubmit = "submit"

	// Outcomes of a delivery attempt, from delivering or from a
	// uncertain re-delivery (§5.4 "重投得到结果 → 同上").
	EventDeliver2XX = "deliver_2xx" // 2xx → settled
	EventDeliver4XX = "deliver_4xx" // 4xx valid Verdict → rejected
	EventDeliver202 = "deliver_202" // 202 on async → under_review

	// Async task without a receiver: straight to under_review.
	EventNoReceiver = "no_receiver"

	// Timeout / connection broken mid-request → uncertain.
	EventTimeout = "timeout"

	// Unreachable / receiver fault / protocol error → failed.
	EventDeliveryFailed = "delivery_failed"

	// uncertain unresolved for 24h → failed (DELIVERY_UNRESOLVED).
	EventUnresolved = "unresolved"

	// under_review outcomes (§5.4, §6.2): publisher verdict or
	// review-window expiry (source = timeout → accepted).
	EventAccept        = "accept"
	EventReject        = "reject"
	EventReviewTimeout = "review_timeout"
)

// taskTransitions is the §4 edge set:
//
//	draft ──open──▶ open ──pause──▶ paused ──open──▶ open
//	  │               │                │
//	  └─────close─────┴─────close──────┴──▶ closed (terminal)
//
// platform_pause applies to an open task; platform_close may close
// any non-closed task (platform governance).
var taskTransitions = map[[2]string]string{
	{TaskDraft, EventOpen}:           TaskOpen,
	{TaskPaused, EventOpen}:          TaskOpen,
	{TaskOpen, EventPause}:           TaskPaused,
	{TaskOpen, EventPlatformPause}:   TaskPaused,
	{TaskDraft, EventClose}:          TaskClosed,
	{TaskOpen, EventClose}:           TaskClosed,
	{TaskPaused, EventClose}:         TaskClosed,
	{TaskDraft, EventPlatformClose}:  TaskClosed,
	{TaskOpen, EventPlatformClose}:   TaskClosed,
	{TaskPaused, EventPlatformClose}: TaskClosed,
}

// claimTransitions is the §5.2 edge set: everything happens from
// active; used / expired / released are terminal.
var claimTransitions = map[[2]string]string{
	{ClaimActive, EventClaimRenew}:   ClaimActive,
	{ClaimActive, EventClaimRelease}: ClaimReleased,
	{ClaimActive, EventClaimExpire}:  ClaimExpired,
	{ClaimActive, EventClaimUse}:     ClaimUsed,
}

// submissionTransitions is the §5.4 edge set. settled / rejected /
// failed are terminal. From uncertain, only a re-delivery result
// (or the 24h unresolved limit) leaves the state; another timeout
// keeps it uncertain without a state write, so (uncertain, timeout)
// is not an edge.
var submissionTransitions = map[[2]string]string{
	{SubDelivering, EventDeliver2XX}:     SubSettled,
	{SubDelivering, EventDeliver4XX}:     SubRejected,
	{SubDelivering, EventDeliver202}:     SubUnderReview,
	{SubDelivering, EventNoReceiver}:     SubUnderReview,
	{SubDelivering, EventTimeout}:        SubUncertain,
	{SubDelivering, EventDeliveryFailed}: SubFailed,

	{SubUncertain, EventDeliver2XX}: SubSettled,
	{SubUncertain, EventDeliver4XX}: SubRejected,
	{SubUncertain, EventDeliver202}: SubUnderReview,
	{SubUncertain, EventUnresolved}: SubFailed,

	{SubUnderReview, EventAccept}:        SubSettled,
	{SubUnderReview, EventReject}:        SubRejected,
	{SubUnderReview, EventReviewTimeout}: SubSettled,
}

// TaskTransition returns the §4 status after `event` fires on `from`.
// Illegal pairs return ErrIllegalTransition wrapping the pair.
func TaskTransition(from, event string) (string, error) {
	return transition("task", taskTransitions, from, event)
}

// ClaimTransition returns the §5.2 status after `event` fires on `from`.
func ClaimTransition(from, event string) (string, error) {
	return transition("claim", claimTransitions, from, event)
}

// SubmissionTransition returns the §5.4 state after `event` fires on `from`.
// EventSubmit is creation (no from-state) and is never legal here.
func SubmissionTransition(from, event string) (string, error) {
	return transition("submission", submissionTransitions, from, event)
}

func transition(machine string, edges map[[2]string]string, from, event string) (string, error) {
	if to, ok := edges[[2]string{from, event}]; ok {
		return to, nil
	}
	return "", fmt.Errorf("%w: %s %s --%s-->", ErrIllegalTransition, machine, from, event)
}
