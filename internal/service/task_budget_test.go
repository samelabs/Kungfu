package service

// Budget tests: the budget floor is one unit of price; a failed open
// leaves a recoverable draft (close + refund return the budget).

import (
	"context"
	"kungfu.md/internal/task"
	"testing"
)

// minimalContract carries only the required §3 fields.
func minimalContract() task.Contract {
	return task.Contract{
		Title:        "Summarize a page",
		Requirements: "Three bullets of the page.",
		Receiver:     task.Receiver{URL: okReceiverURL},
		Price:        5,
	}
}

func TestBudgetFloorIsOnePrice(t *testing.T) {
	pool := pubTestPool(t)
	fresh := pubSeedBot(t, pool, 66) // the signup grant
	ctx := context.Background()

	// a fresh 66-credit account publishes price=10 budget=60
	c := minimalContract()
	c.Price = 10
	if _, err := CreateTask(ctx, pool, fresh, c, 60); err != nil {
		t.Fatalf("create with budget=60: %v", err)
	}
	// budget below one price unit → VALIDATION_FAILED(field=budget)
	if _, err := CreateTask(ctx, pool, fresh, c, 9); appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("budget<price: %v, want VALIDATION_FAILED", err)
	}
	// balance below the budget → INSUFFICIENT_CREDITS
	if _, err := CreateTask(ctx, pool, fresh, c, 1000); appErrOf(t, err).Code != "INSUFFICIENT_CREDITS" {
		t.Fatalf("insufficient: %v, want INSUFFICIENT_CREDITS", err)
	}
}
