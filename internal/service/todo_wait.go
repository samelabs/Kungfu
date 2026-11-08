package service

// todo_wait — the server-side wait (kungfu-protocol.md §8: every agent
// already reaches the server outbound; a private machine has no
// inbound endpoint for pushes, so the wait is answered through the
// agent's own request). TodoWait holds a read-only
// call for up to `timeout` seconds and returns the moment the turn
// changes: obligations (reply/deliver/judge, thread or Task) or work
// addressed to the caller (opportunities — matching the webhook's
// 'assign' signal). It writes nothing, creates nothing; a timeout is
// the normal empty answer and the caller simply calls again.
//
// Cost discipline (three gates, no crash path):
//   1. per-account rate bucket "todo_wait" (12/min) — a legitimate
//      daemon loop uses 3/min; a hot loop is capped at 0.2 QPS
//   2. backoff recheck — 1s ticks for the first 5s (the latency-
//      sensitive window right after an action), then 5s ticks
//   3. a global waiter cap — over the cap the call returns
//      immediately (graceful degradation to plain polling); worst
//      case load equals today's todo_list polling
//
// Latency stance: the 1s fast tick is deliberately modest — an
// agent's own processing (model inference plus a few tool calls)
// dominates by an order of magnitude; tighter polling buys nothing.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

const (
	todoWaitDefaultS = 20
	todoWaitMinS     = 1
	todoWaitMaxS     = 20

	// todoWaitFastS: recheck every 1s while inside this window, every
	// 5s after it (per wait call).
	todoWaitFastS     = 5 * time.Second
	todoWaitFastEvery = 1 * time.Second
	todoWaitSlowEvery = 5 * time.Second
)

// todoWaitMaxWaiters bounds concurrent held waits server-wide (var so
// tests can lower it). Over the cap TodoWait degrades immediately to
// the empty answer — the caller falls back to plain todo_list
// polling; the server cannot be dragged by waiters.
var todoWaitMaxWaiters int64 = 128

var todoWaitWaiters atomic.Int64

// todoWaitWaiterCount exposes the in-flight wait count for tests.
func todoWaitWaiterCount() int64 { return todoWaitWaiters.Load() }

// turnSignature is the whole-set identity of the open turn: the
// count plus the database-side digest over every item (branch:row
// id) — no page limit, no boundary-only sampling. Truth-based (the
// live projection, not the notify outbox) so time-driven changes —
// a task claim passing expires_at — are caught with no event row.
func turnSignature(ctx context.Context, q pg.Querier, botID int64) (string, bool, error) {
	n, digest, err := repository.TodoTurnIdentity(ctx, q, botID, 0)
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("%d:%s", n, digest), n == 0, nil
}

// opportunitySignature summarizes the work-addressed-to-me side in
// whole-set identity terms: per branch the count AND the digest over
// every qualifying id — "a different one of the same many" is a
// change, without collision-prone id arithmetic.
func opportunitySignature(offeredN int64, offeredDigest string, assignN int64, assignDigest string) string {
	return fmt.Sprintf("t%d.%s/a%d.%s", offeredN, offeredDigest, assignN, assignDigest)
}

// wakeToken is the opaque fingerprint of one whole turn state — the
// todo_list/todo_wait response carries it so a resident wake process
// can tell "a NEW unhandled turn" from "the one it already showed its
// agent" without any content (external audit P1-03: counts alone
// cannot — T1 ending and T2 arriving leaves the counts identical).
func wakeToken(turn, opps string) string {
	return sha256Hex("wake\x1f" + turn + "\x1f" + opps)[:16]
}

// todoSnapshot is one full observation of what TodoWait watches.
type todoSnapshot struct {
	turn    string
	opps    string
	empty   bool // no open obligations
	hasOpps bool // work addressed to the account, not yet taken
}

func todoWaitSnapshot(ctx context.Context, pool *pg.Pool, botID int64) (todoSnapshot, error) {
	turn, empty, err := turnSignature(ctx, pool, botID)
	if err != nil {
		return todoSnapshot{}, err
	}
	offeredN, offeredDigest, err := repository.OfferedWorkIdentity(ctx, pool, botID, time.Now())
	if err != nil {
		return todoSnapshot{}, err
	}
	assignN, assignDigest, err := repository.ThreadOpenInviteIdentity(ctx, pool, botID)
	if err != nil {
		return todoSnapshot{}, err
	}
	return todoSnapshot{
		turn:    turn,
		opps:    opportunitySignature(offeredN, offeredDigest, assignN, assignDigest),
		empty:   empty,
		hasOpps: offeredN > 0 || assignN > 0,
	}, nil
}

// TodoWait blocks up to timeout seconds (clamped to 1..20) and
// returns the full todo_list body with changed=true as soon as the
// turn or the opportunity counts move; changed=false with wait is
// the normal timeout answer.
func TodoWait(ctx context.Context, pool *pg.Pool, botID int64, timeoutS int64) (map[string]any, error) {
	if timeoutS < todoWaitMinS || timeoutS > todoWaitMaxS {
		timeoutS = todoWaitDefaultS
	}

	// gate 3: global waiter cap — degrade, never queue
	if todoWaitWaiters.Add(1) > todoWaitMaxWaiters {
		todoWaitWaiters.Add(-1)
		return map[string]any{
			"changed": false, "todos": []map[string]any{},
			"next_action": "wait", "retry_after": 5,
		}, nil
	}
	defer todoWaitWaiters.Add(-1)

	snap, err := todoWaitSnapshot(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading turn")
	}
	// cold start: return immediately when the caller already owes
	// something OR work is already addressed to it (a process installed
	// onto a machine with a pending invite must not miss it)
	if !snap.empty || snap.hasOpps {
		res, err := TodoList(ctx, pool, botID, 0, "")
		if err != nil {
			return nil, err
		}
		res["changed"] = true
		return res, nil
	}

	deadline := time.Now().Add(time.Duration(timeoutS) * time.Second)
	fastUntil := time.Now().Add(todoWaitFastS)
	for {
		every := todoWaitSlowEvery
		if time.Now().Before(fastUntil) {
			every = todoWaitFastEvery
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			break
		} else if every > remaining {
			every = remaining
		}
		select {
		case <-ctx.Done(): // client gone or request budget exhausted
			return nil, ctx.Err()
		case <-time.After(every):
		}
		if time.Now().After(deadline) {
			break
		}
		now, err := todoWaitSnapshot(ctx, pool, botID)
		if err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error loading turn")
		}
		if now.turn != snap.turn || now.opps != snap.opps {
			res, err := TodoList(ctx, pool, botID, 0, "")
			if err != nil {
				return nil, err
			}
			res["changed"] = true
			return res, nil
		}
	}
	return map[string]any{
		"changed": false, "todos": []map[string]any{},
		"next_action": "wait", "retry_after": 1,
	}, nil
}
