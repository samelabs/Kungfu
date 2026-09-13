package service

// Task business rules — the single source of truth for task fundability,
// status legality, and open-visibility. Every consumer (create/open,
// board, get, submit, owner test, settlement auto-close) must call these
// functions instead of re-implementing thresholds locally.
//
// Task budget is an escrow fact (locked via lock_task); Credits remains
// the ledger authority; delivery accepted (owner POST 2xx) is the only
// settlement trigger.

import (
	"math"

	"kungfu.md/internal/errors"
)

// MinOpenBudget is the minimum budget for a task to be open/acceptable.
const MinOpenBudget = 1000.0

// Task status values (no new statuses).
const (
	taskStatusPending = "pending"
	taskStatusOpen    = "open"
	taskStatusClosed  = "closed"
)

// fundable reports whether a budget/price pair can pay for one more
// delivery: positive price, budget at least MinOpenBudget, budget at
// least the price. Non-finite values are never fundable.
func fundable(budget, price float64) bool {
	if math.IsNaN(budget) || math.IsInf(budget, 0) ||
		math.IsNaN(price) || math.IsInf(price, 0) {
		return false
	}
	return price > 0 && budget >= MinOpenBudget && budget >= price
}

// agentAcceptable reports whether an agent may submit to a task right
// now: the task is open AND fundable.
func agentAcceptable(status string, budget, price float64) bool {
	return status == taskStatusOpen && fundable(budget, price)
}

// nextBudgetAfterDelivery returns the post-settlement budget and whether
// the task must auto-close: after paying price, if the remaining budget
// can no longer fund one more delivery, the task closes.
func nextBudgetAfterDelivery(budget, price float64) (float64, bool) {
	next := budget - price
	if next < MinOpenBudget || next < price {
		return next, true
	}
	return next, false
}

// assertFundable is the 400-contract validation used by owner-facing
// create/open paths (distinct from the agent-facing TaskCheck rules).
func assertFundable(postapi string, budget, price float64) error {
	if err := validatePostapiField(postapi); err != nil {
		return err
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		return errors.New(400, "INVALID_PRICE", "Price must be a finite number")
	}
	if price <= 0 {
		return errors.New(400, "INVALID_PRICE", "Price must be greater than zero")
	}
	if math.IsNaN(budget) || math.IsInf(budget, 0) {
		return errors.New(400, "INVALID_BUDGET", "Budget must be a finite number")
	}
	if budget < MinOpenBudget || budget < price {
		return errors.New(400, "TASK_BUDGET_TOO_LOW", "Open tasks require enough budget")
	}
	return nil
}
