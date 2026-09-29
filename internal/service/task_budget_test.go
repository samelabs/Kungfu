package service

// Budget tests: the budget floor is one unit of price; a failed open
// leaves a recoverable draft (close + refund return the budget).

import (
	"context"
	"testing"

	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
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

func TestOpenFailureLeavesRecoverableDraft(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100)
	ctx := context.Background()

	// unreachable receiver: creation succeeds, opening fails
	c := minimalContract()
	c.Receiver = task.Receiver{URL: "https://receiver.invalid/hook"}
	view, err := CreateTask(ctx, pool, publisher, c, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if _, err := OpenTask(ctx, pool, publisher, code); appErrOf(t, err).Code != "TEST_DELIVERY_FAILED" {
		t.Fatalf("open: %v, want TEST_DELIVERY_FAILED", err)
	}
	// draft with the budget locked; close + refund recovers it
	draft, _ := repository.FindTaskByCode(ctx, pool, code)
	if draft.Status != task.TaskDraft || draft.BudgetLocked != 5 {
		t.Fatalf("after failed open: %s locked=%d", draft.Status, draft.BudgetLocked)
	}
	if _, err := CloseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := RefundTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if err := task.CheckInvariants(ctx, pool, draft.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}
