package service

// WO-11 low-friction publishing tests: the minimal contract {title,
// objective, price} plus budget = price publishes a real, auditable
// task; the budget floor is one unit of price; open-on-create leaves a
// recoverable draft when opening fails.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// minimalContract is the 3-field contract.
func minimalContract() task.Contract {
	return task.Contract{Title: "Summarize a page", Objective: "Three bullets of the page.", Price: 5}
}

func TestMinimalContractDefaults(t *testing.T) {
	c := minimalContract().WithDefaults()
	if c.Acceptance.Mode != task.ModeAsync ||
		c.Acceptance.ReviewWindow == nil || *c.Acceptance.ReviewWindow != 259200 {
		t.Fatalf("acceptance defaults: %v %v", c.Acceptance.Mode, c.Acceptance.ReviewWindow)
	}
	if c.Output.Description != task.DefaultOutputDescription || string(c.Output.Schema) != task.DefaultOutputSchema {
		t.Fatalf("output defaults: %q %s", c.Output.Description, c.Output.Schema)
	}
	if len(c.Acceptance.Criteria) != 1 || c.Acceptance.Criteria[0].Kind != task.KindJudgment ||
		c.Acceptance.Criteria[0].ID != "C1" {
		t.Fatalf("criteria default: %+v", c.Acceptance.Criteria)
	}
	if errs := task.ValidateContract(c); len(errs) != 0 {
		t.Fatalf("minimal contract invalid after defaults: %v", errs)
	}
	// an explicit sync contract gets NO judgment default and must
	// declare its own criteria
	sync := minimalContract()
	sync.Acceptance.Mode = task.ModeSync
	filled := sync.WithDefaults()
	if len(filled.Acceptance.Criteria) != 0 {
		t.Fatalf("sync contract got the judgment default: %+v", filled.Acceptance.Criteria)
	}
	if errs := task.ValidateContract(filled); len(errs) == 0 {
		t.Fatal("sync without criteria validates")
	}
	// examples are optional now (0–5); when present the rules hold
	if errs := task.ValidateContract(minimalContract().WithDefaults()); errs != nil {
		t.Fatalf("0 examples rejected: %v", errs)
	}
}

func TestMinimalCreateOpenAndAuditView(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100)
	ctx := context.Background()

	view, err := CreateTask(ctx, pool, publisher, minimalContract(), 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	// task_get returns the materialized contract (auditable snapshot)
	after, err := GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after["status"] != task.TaskOpen || after["version"].(int32) != 1 {
		t.Fatalf("after open: %v", after)
	}
	vrow, err := repository.FindTaskVersion(ctx, pool, mustTaskID(t, pool, code), 1)
	if err != nil || vrow == nil {
		t.Fatalf("version: %v", err)
	}
	var stored task.Contract
	if err := json.Unmarshal(vrow.Contract, &stored); err != nil {
		t.Fatalf("stored contract: %v", err)
	}
	if stored.Acceptance.Mode != task.ModeAsync || stored.Output.Description != task.DefaultOutputDescription ||
		len(stored.Acceptance.Criteria) != 1 || stored.Acceptance.Criteria[0].Kind != task.KindJudgment {
		t.Fatalf("stored contract not materialized: %+v", stored)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
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

	// sync + unreachable receiver: creation succeeds, opening fails
	// (an explicit sync task must declare its own non-judgment criteria)
	c := minimalContract()
	c.Acceptance.Mode = task.ModeSync
	c.Acceptance.Criteria = []task.Criterion{{ID: "C1", Kind: task.KindRule, Description: "three bullets"}}
	c.Examples = []task.Example{{Payload: []byte(`{"result":"three bullets"}`), Accepted: true}}
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

func TestDefaultAsyncJourneyToSettled(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// minimal task, no receiver: default async parks in under_review
	code := submitOpenedTask(t, pool, publisher, 5, func(c *task.Contract) {
		*c = minimalContract()
	})
	view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "min-1",
		Payload: []byte(`{"result":"three bullets"}`),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if view.State != task.SubUnderReview {
		t.Fatalf("state = %s, want under_review", view.State)
	}
	// the default judgment criterion accepts via task_verdict
	accepted, err := SubmitVerdict(ctx, pool, publisher, view.SubmissionID.Int64(),
		[]byte(`{"accepted":true}`), time.Now())
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if accepted.State != task.SubSettled || accepted.Paid != 5 {
		t.Fatalf("accepted: %+v", accepted)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}
