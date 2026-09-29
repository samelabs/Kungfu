package task

import (
	"errors"
	"testing"
)

// Table-driven coverage of the three spec state machines: every
// legal edge of docs/task-spec-1.0.md §4 / §5.2 / §5.4, plus at
// least one illegal edge per state (terminal states get several).

func TestTaskTransitionLegal(t *testing.T) {
	cases := []struct {
		from, event, want string
	}{
		// §4 diagram: draft ──open──▶ open ──pause──▶ paused ──open──▶ open
		{TaskPaused, EventOpen, TaskOpen},
		{TaskOpen, EventPause, TaskPaused},
		{TaskPaused, EventOpen, TaskOpen},
		// close from every non-terminal status → closed
		{TaskPaused, EventClose, TaskClosed},
		{TaskOpen, EventClose, TaskClosed},
		{TaskPaused, EventClose, TaskClosed},
		// §4 table platform rows
		{TaskOpen, EventPlatformPause, TaskPaused},
		{TaskPaused, EventPlatformClose, TaskClosed},
		{TaskOpen, EventPlatformClose, TaskClosed},
		{TaskPaused, EventPlatformClose, TaskClosed},
	}
	for _, c := range cases {
		got, err := TaskTransition(c.from, c.event)
		if err != nil {
			t.Errorf("TaskTransition(%s, %s): unexpected error %v", c.from, c.event, err)
			continue
		}
		if got != c.want {
			t.Errorf("TaskTransition(%s, %s) = %s, want %s", c.from, c.event, got, c.want)
		}
	}
}

func TestTaskTransitionIllegal(t *testing.T) {
	cases := []struct{ from, event string }{
		{TaskPaused, EventPause},         // draft cannot pause (§4)
		{TaskPaused, EventPlatformPause}, // platform pause hits open tasks
		{TaskOpen, EventOpen},            // already open
		{TaskPaused, EventPause},         // already paused
		{TaskClosed, EventOpen},          // closed is terminal
		{TaskClosed, EventPause},         // closed is terminal
		{TaskClosed, EventClose},         // closed is terminal
		{TaskClosed, EventPlatformClose}, // closed is terminal
		{TaskPaused, "fund"},             // fund never changes status
		{"bogus", EventOpen},             // unknown status
		{TaskOpen, "bogus"},              // unknown event
	}
	for _, c := range cases {
		if _, err := TaskTransition(c.from, c.event); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("TaskTransition(%s, %s): want ErrIllegalTransition, got %v", c.from, c.event, err)
		}
	}
}

func TestClaimTransitionLegal(t *testing.T) {
	cases := []struct {
		from, event, want string
	}{
		// §5.2: everything happens from active
		{ClaimActive, EventClaimRenew, ClaimActive}, // renew keeps active (extends expires_at)
		{ClaimActive, EventClaimRelease, ClaimReleased},
		{ClaimActive, EventClaimExpire, ClaimExpired},
		{ClaimActive, EventClaimUse, ClaimUsed},
	}
	for _, c := range cases {
		got, err := ClaimTransition(c.from, c.event)
		if err != nil {
			t.Errorf("ClaimTransition(%s, %s): unexpected error %v", c.from, c.event, err)
			continue
		}
		if got != c.want {
			t.Errorf("ClaimTransition(%s, %s) = %s, want %s", c.from, c.event, got, c.want)
		}
	}
}

func TestClaimTransitionIllegal(t *testing.T) {
	cases := []struct{ from, event string }{
		{ClaimUsed, EventClaimUse},      // used is terminal
		{ClaimUsed, EventClaimRelease},  // used is terminal
		{ClaimExpired, EventClaimRenew}, // expired is terminal
		{ClaimExpired, EventClaimUse},   // expired claims cannot submit (§5.2)
		{ClaimReleased, EventClaimExpire},
		{ClaimReleased, EventClaimUse},
		{ClaimActive, "claim"}, // creation is an INSERT, not a transition
		{ClaimActive, "bogus"},
	}
	for _, c := range cases {
		if _, err := ClaimTransition(c.from, c.event); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("ClaimTransition(%s, %s): want ErrIllegalTransition, got %v", c.from, c.event, err)
		}
	}
}

func TestSubmissionTransitionLegal(t *testing.T) {
	cases := []struct {
		from, event, want string
	}{
		// §5.4 from delivering
		{SubDelivering, EventDeliver2XX, SubSettled},
		{SubDelivering, EventDeliver4XX, SubRejected},
		{SubDelivering, EventTimeout, SubUncertain},
		{SubDelivering, EventDeliveryFailed, SubFailed},
		// from uncertain: re-delivery result ("重投得到结果 → 同上" —
		// including a definitive failure per §7.2) or the 24h limit
		{SubUncertain, EventDeliver2XX, SubSettled},
		{SubUncertain, EventDeliver4XX, SubRejected},
		{SubUncertain, EventDeliveryFailed, SubFailed},
		{SubUncertain, EventUnresolved, SubFailed},
	}
	for _, c := range cases {
		got, err := SubmissionTransition(c.from, c.event)
		if err != nil {
			t.Errorf("SubmissionTransition(%s, %s): unexpected error %v", c.from, c.event, err)
			continue
		}
		if got != c.want {
			t.Errorf("SubmissionTransition(%s, %s) = %s, want %s", c.from, c.event, got, c.want)
		}
	}
}

func TestSubmissionTransitionIllegal(t *testing.T) {
	cases := []struct{ from, event string }{
		// delivering resolves through delivery outcomes or the timeout edge
		{SubDelivering, EventUnresolved},
		// uncertain: another timeout keeps it uncertain without a state write;
		// only a result or the 24h limit leaves the state
		{SubUncertain, EventTimeout},
		// terminal states have no outgoing edges
		{SubSettled, EventDeliver4XX},
		{SubSettled, EventDeliver2XX},
		{SubRejected, EventDeliver4XX},
		{SubRejected, EventDeliver2XX},
		{SubFailed, EventUnresolved},
		{SubFailed, EventDeliver2XX},
		// creation is not a transition
		{SubDelivering, EventSubmit},
		{SubDelivering, "bogus"},
	}
	for _, c := range cases {
		if _, err := SubmissionTransition(c.from, c.event); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("SubmissionTransition(%s, %s): want ErrIllegalTransition, got %v", c.from, c.event, err)
		}
	}
}

// The legal-edge tables above must be exhaustive: any pair NOT
// listed is illegal. This cross-check fails when someone adds an
// edge to the maps without a test row (or vice versa).
func TestTransitionMapsAreExactlyTheSpecEdges(t *testing.T) {
	type machine struct {
		name   string
		edges  map[[2]string]string
		states []string
		events []string
	}
	machines := []machine{
		{"task", taskTransitions, TaskStatuses,
			[]string{EventOpen, EventPause, EventClose, EventPlatformPause, EventPlatformClose, "create", "update", "fund", "refund"}},
		{"claim", claimTransitions, ClaimStatuses,
			[]string{EventClaimRenew, EventClaimRelease, EventClaimExpire, EventClaimUse, "claim"}},
		{"submission", submissionTransitions, SubmissionStates,
			[]string{EventSubmit, EventDeliver2XX, EventDeliver4XX,
				EventTimeout, EventDeliveryFailed, EventUnresolved}},
	}
	for _, m := range machines {
		for _, s := range m.states {
			for _, e := range m.events {
				_, err := transition(m.name, m.edges, s, e)
				_, legal := m.edges[[2]string{s, e}]
				if legal != (err == nil) {
					t.Errorf("%s: (%s, %s) legal=%v but err=%v — table test out of sync with the edge map",
						m.name, s, e, legal, err)
				}
			}
		}
	}
}

func TestSubmissionTerminal(t *testing.T) {
	for _, s := range []string{SubSettled, SubRejected, SubFailed} {
		if !SubmissionTerminal(s) {
			t.Errorf("SubmissionTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range []string{SubDelivering, SubUncertain} {
		if SubmissionTerminal(s) {
			t.Errorf("SubmissionTerminal(%s) = true, want false", s)
		}
	}
}
