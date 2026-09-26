package mcpserver

// WO-7b tests: every publisher tool through both channels with the
// WO-7a canonical comparison; the §8.4 catalogue covered end-to-end
// through CallTool; the /api/v1 body cap; and one full lifecycle.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
	"kungfu.md/internal/task"
)

func pubContractArg(reviewWindow int64) map[string]any {
	return map[string]any{
		"title": "Pub task", "objective": "o", "inputs": "i",
		"output": map[string]any{
			"description": "d",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string"},
					"bullets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 3, "maxItems": 3},
				},
				"required": []string{"url", "bullets"},
			},
		},
		"acceptance": map[string]any{
			"mode":          "async",
			"review_window": reviewWindow,
			"criteria":      []map[string]any{{"id": "C1", "kind": "rule", "description": "r"}},
		},
		"examples": []map[string]any{
			{"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}}, "accepted": true},
		},
		"price": 5,
	}
}

// TestPublisherToolsBothChannels: each mutating tool runs on TWIN
// tasks in identical states (MCP on one, HTTP on the other) so both
// channels observe success; the comparison normalizes task codes.
func TestPublisherToolsBothChannels(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()

	pubKey, _, pubID := m2RegisterSeeded(t, srv, pool, 10_000)
	_, _, agentID := m2RegisterSeeded(t, srv, pool, 0)
	pubBot := wo7Bot(t, pool, pubID)

	mcpP := func(tool string, args map[string]any) (map[string]any, bool) {
		env, isErr, _ := mcpCall(t, srv, pubKey, tool, args)
		return env, isErr
	}
	httpP := func(tool string, args map[string]any) (map[string]any, int) {
		raw, _ := json.Marshal(args)
		env, status := CallTool(context.Background(), &deps, tool, pubBot, raw)
		return env, status
	}
	// assertNormCodes compares after replacing the twin codes with X.
	assertCodes := func(what string, m, h map[string]any, codes ...string) {
		t.Helper()
		norm := func(v map[string]any) string {
			raw, _ := json.Marshal(v)
			var mm map[string]any
			_ = json.Unmarshal(raw, &mm)
			dropAgentRef(mm)
			out, _ := json.Marshal(stripVolatile(mm))
			s := string(out)
			for _, c := range codes {
				s = strings.ReplaceAll(s, "\""+c+"\"", "\"X\"")
			}
			return s
		}
		if norm(m) != norm(h) {
			t.Fatalf("%s envelopes differ:\nMCP: %s\nHTTP: %s", what, norm(m), norm(h))
		}
	}

	// task_create on twins
	e1m, i1 := mcpP("task_create", map[string]any{"contract": pubContractArg(3600), "budget": 2000})
	e1h, s1 := httpP("task_create", map[string]any{"contract": pubContractArg(3600), "budget": 2000})
	if i1 || s1 != 200 || e1m["status"] != task.TaskDraft || e1m["next_action"] != nil {
		t.Fatalf("task_create: %+v isError=%v http=%d", e1m, i1, s1)
	}
	assertCodes("task_create", e1m, e1h, e1m["code"].(string), e1h["code"].(string))
	codeA, codeB := e1m["code"].(string), e1h["code"].(string)

	// task_get (draft twins)
	e2m, i2 := mcpP("task_get", map[string]any{"code": codeA})
	e2h, s2 := httpP("task_get", map[string]any{"code": codeB})
	if i2 || s2 != 200 || e2m["status"] != task.TaskDraft {
		t.Fatalf("task_get: %v %d", i2, s2)
	}
	assertCodes("task_get", e2m, e2h, codeA, codeB)

	// task_update (draft twins; the view carries status/counters — the
	// edited contract is asserted via the stored draft in the flow test)
	updated := pubContractArg(7200)
	updated["title"] = "Pub task v2"
	e3m, i3 := mcpP("task_update", map[string]any{"code": codeA, "contract": updated})
	e3h, s3 := httpP("task_update", map[string]any{"code": codeB, "contract": updated})
	if i3 || s3 != 200 || e3m["status"] != task.TaskDraft {
		t.Fatalf("task_update: %v %d %+v", i3, s3, e3m)
	}
	assertCodes("task_update", e3m, e3h, codeA, codeB)

	// task_open (twins: async, no receiver → no test delivery)
	e4m, i4 := mcpP("task_open", map[string]any{"code": codeA})
	e4h, s4 := httpP("task_open", map[string]any{"code": codeB})
	if i4 || s4 != 200 || e4m["status"] != task.TaskOpen || numOff(e4m["version"]) != 1 {
		t.Fatalf("task_open: %+v isError=%v http=%d", e4m, i4, s4)
	}
	assertCodes("task_open", e4m, e4h, codeA, codeB)

	// task_fund (twins)
	e5m, i5 := mcpP("task_fund", map[string]any{"code": codeA, "amount": 500})
	e5h, s5 := httpP("task_fund", map[string]any{"code": codeB, "amount": 500})
	if i5 || s5 != 200 || numOff(e5m["budget_locked"]) != 2500 {
		t.Fatalf("task_fund: %+v", e5m)
	}
	assertCodes("task_fund", e5m, e5h, codeA, codeB)

	// executor submissions on both twins (for submissions/verdict)
	for _, code := range []string{codeA, codeB} {
		if _, err := service.SubmitWork(ctx, pool, agentID, service.SubmitInput{
			Code: code, RequestKey: "pub-flow",
			Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
		}, deps.AgentRefKey, time.Now()); err != nil {
			t.Fatalf("executor submit %s: %v", code, err)
		}
	}

	// task_submissions (twins)
	e6m, i6 := mcpP("task_submissions", map[string]any{"code": codeA})
	e6h, s6 := httpP("task_submissions", map[string]any{"code": codeB})
	if i6 || s6 != 200 || numOff(e6m["total"]) != 1 {
		t.Fatalf("task_submissions: %+v", e6m)
	}
	assertCodes("task_submissions", e6m, e6h, codeA, codeB)
	rowA, _, _ := service.ListSubmissionsForPublisher(ctx, pool, pubID, codeA, "", 1, 10, deps.AgentRefKey)
	rowB, _, _ := service.ListSubmissionsForPublisher(ctx, pool, pubID, codeB, "", 1, 10, deps.AgentRefKey)

	// task_verdict (accept on each twin's submission)
	e7m, i7 := mcpP("task_verdict", map[string]any{"submission_id": rowA[0].SubmissionID, "verdict": map[string]any{"accepted": true}})
	e7h, s7 := httpP("task_verdict", map[string]any{"submission_id": rowB[0].SubmissionID, "verdict": map[string]any{"accepted": true}})
	if i7 || s7 != 200 || e7m["state"] != task.SubSettled || e7m["next_action"] != nil {
		t.Fatalf("task_verdict: %+v isError=%v http=%d", e7m, i7, s7)
	}
	assertCodes("task_verdict", e7m, e7h, codeA, codeB)

	// task_pause (twins, both open)
	e8m, i8 := mcpP("task_pause", map[string]any{"code": codeA})
	e8h, s8 := httpP("task_pause", map[string]any{"code": codeB})
	if i8 || s8 != 200 || e8m["status"] != task.TaskPaused {
		t.Fatalf("task_pause: %v %d", i8, s8)
	}
	assertCodes("task_pause", e8m, e8h, codeA, codeB)

	// task_close (twins, paused)
	e9m, i9 := mcpP("task_close", map[string]any{"code": codeA})
	e9h, s9 := httpP("task_close", map[string]any{"code": codeB})
	if i9 || s9 != 200 || e9m["status"] != task.TaskClosed {
		t.Fatalf("task_close: %v %d", i9, s9)
	}
	assertCodes("task_close", e9m, e9h, codeA, codeB)

	// task_refund (twins: 2500 − 5 settled = 2495 refunded)
	e10m, i10 := mcpP("task_refund", map[string]any{"code": codeA})
	e10h, s10 := httpP("task_refund", map[string]any{"code": codeB})
	if i10 || s10 != 200 || numOff(e10m["refunded"]) != 2495 {
		t.Fatalf("task_refund: %+v isError=%v http=%d", e10m, i10, s10)
	}
	assertCodes("task_refund", e10m, e10h, codeA, codeB)

	// task_list (read)
	e11m, i11 := mcpP("task_list", map[string]any{})
	e11h, s11 := httpP("task_list", map[string]any{})
	if i11 || s11 != 200 || numOff(e11m["total"]) < 2 {
		t.Fatalf("task_list: %+v", e11m)
	}
	// twin codes normalized; other tasks' codes also vary between
	// channels only by set — strip the whole tasks array for this read
	{
		m, h := map[string]any{"total": e11m["total"], "ok": e11m["ok"]}, map[string]any{"total": e11h["total"], "ok": e11h["ok"]}
		assertJSONEqual(t, "task_list-meta", m, h)
	}

	for _, code := range []string{codeA, codeB} {
		if err := task.CheckInvariants(ctx, pool, mustTaskIDOf(t, pool, code)); err != nil {
			t.Fatalf("CheckInvariants(%s): %v", code, err)
		}
	}
}

// dropAgentRef removes agent_ref values in place: agent_ref is derived
// per task code (§7.1), so twin tasks legitimately differ.
func dropAgentRef(m map[string]any) {
	delete(m, "agent_ref")
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			dropAgentRef(sub)
		}
		if arr, ok := v.([]any); ok {
			for _, it := range arr {
				if sub, ok := it.(map[string]any); ok {
					dropAgentRef(sub)
				}
			}
		}
	}
}

func mustTaskIDOf(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	tr, err := repository.FindTaskByCode(context.Background(), pool, code)
	if err != nil || tr == nil {
		t.Fatalf("task %s: %v", code, err)
	}
	return tr.ID
}

// TestErrorCatalogProtocolCoverage: every §8.4 executor- and
// publisher-side code (except UNAUTHORIZED/RATE_LIMIT — covered by
// TestAPIV1Auth and the work_submit limiter path) triggered through
// CallTool with code, HTTP status and next_action asserted.
func TestErrorCatalogProtocolCoverage(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()

	_, _, pubID := m2RegisterSeeded(t, srv, pool, 200_000)
	pubBot := wo7Bot(t, pool, pubID)
	agentKey, _, agentID := m2RegisterSeeded(t, srv, pool, 0)
	agentBot := wo7Bot(t, pool, agentID)
	_, _, otherID := m2RegisterSeeded(t, srv, pool, 10_000)
	otherBot := wo7Bot(t, pool, otherID)

	// an open async task by agentID's publisher? No: pubID owns tasks.
	code := wo7OpenTask(t, pool, pubID)
	ownCode := wo7OpenTask(t, pool, otherID)
	settledCode := wo7OpenTask(t, pool, pubID)
	reviewCode := wo7OpenTask(t, pool, pubID)
	_ = settledCode

	// one under_review submission (for VERDICT_INVALID + NOT_UNDER_REVIEW)
	subView, err := service.SubmitWork(ctx, pool, agentID, service.SubmitInput{
		Code: reviewCode, RequestKey: "cat-review",
		Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
	}, deps.AgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// one settled submission (a verdict already applied)
	if _, err := service.SubmitVerdict(ctx, pool, pubID, subView.SubmissionID,
		[]byte(`{"accepted":true}`), time.Now()); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	// a fresh under_review row for VERDICT_INVALID
	freshView, err := service.SubmitWork(ctx, pool, agentID, service.SubmitInput{
		Code: reviewCode, RequestKey: "cat-review-2",
		Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
	}, deps.AgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}

	call := func(bot *model.Bot, tool string, args map[string]any) (map[string]any, int) {
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		return env, status
	}

	cases := []struct {
		code, next string
		status     int
		trigger    func() (map[string]any, int)
	}{
		// executor side
		{"TASK_NOT_FOUND", "stop", 404, func() (map[string]any, int) {
			return call(agentBot, "work_get", map[string]any{"code": "nope00000000"})
		}},
		{"TASK_NOT_OPEN", "stop", 409, func() (map[string]any, int) {
			return call(agentBot, "work_claim", map[string]any{"code": draftOf(t, pool, pubID)})
		}},
		{"OWN_TASK", "stop", 403, func() (map[string]any, int) {
			return call(otherBot, "work_claim", map[string]any{"code": ownCode})
		}},
		{"SUBMISSION_LIMIT", "stop", 409, func() (map[string]any, int) {
			c := wo7OpenTask(t, pool, pubID)
			for i := 0; i < 5; i++ {
				seedRejectedSub(t, pool, c, agentID)
			}
			return call(agentBot, "work_claim", map[string]any{"code": c})
		}},
		{"CLAIM_REQUIRED", "retry", 409, func() (map[string]any, int) {
			c := wo7OpenTaskClaimRequired(t, pool, pubID)
			return call(agentBot, "work_submit", map[string]any{
				"code": c, "request_key": "cr-1",
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
			})
		}},
		{"CLAIM_INVALID", "retry", 409, func() (map[string]any, int) {
			claim, err := service.ClaimTask(ctx, pool, agentID, code, time.Now().Add(-2*time.Hour))
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			return call(agentBot, "work_claim_renew", map[string]any{"claim_id": claim.ClaimID})
		}},
		{"INVALID_REQUEST_KEY", "revise", 422, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": code, "request_key": "bad key!",
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
			})
		}},
		{"IDEMPOTENCY_CONFLICT", "revise", 409, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": reviewCode, "request_key": "cat-review", // same task+key as the settled row, different payload
				"payload": map[string]any{"url": "https://example.com/b", "bullets": []string{"s1", "s2", "s3"}},
			})
		}},
		{"INVALID_REVISES", "revise", 422, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": code, "request_key": "rev-1", "revises": 999999,
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
			})
		}},
		{"PAYLOAD_TOO_LARGE", "revise", 413, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": code, "request_key": "big-1",
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{strings.Repeat("x", 600000), "s2", "s3"}},
			})
		}},
		{"SCHEMA_MISMATCH", "revise", 422, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": code, "request_key": "sm-1",
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"one"}},
			})
		}},
		{"CREDENTIAL_IN_PAYLOAD", "revise", 422, func() (map[string]any, int) {
			return call(agentBot, "work_submit", map[string]any{
				"code": code, "request_key": "cred-1",
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", fakeCred()}},
			})
		}},
		{"SUBMISSION_NOT_FOUND", "", 404, func() (map[string]any, int) {
			id := int64(999999)
			return call(agentBot, "work_status", map[string]any{"submission_id": id})
		}},
		{"HARNESS_REF_NOT_FOUND", "", 404, func() (map[string]any, int) {
			return call(agentBot, "work_harness", map[string]any{"code": code, "ref_id": "nope00000000"})
		}},
		{"SLOTS_EXHAUSTED", "stop", 409, func() (map[string]any, int) {
			c := wo7OpenTask(t, pool, pubID) // price 5, budget 1000 → 200 slots
			if err := drainSlots(t, srv, pool, c, 200); err != nil {
				t.Fatalf("drain: %v", err)
			}
			return call(agentBot, "work_claim", map[string]any{"code": c})
		}},
		// publisher side
		{"NOT_OWNER", "", 403, func() (map[string]any, int) {
			return call(agentBot, "task_pause", map[string]any{"code": code}) // agent is not the publisher
		}},
		{"INSUFFICIENT_CREDITS", "", 402, func() (map[string]any, int) {
			return call(agentBot, "task_create", map[string]any{"contract": pubContractArg(3600), "budget": 1_000_000})
		}},
		{"INVALID_STATE", "", 409, func() (map[string]any, int) {
			c := wo7OpenTask(t, pool, pubID)
			if _, err := service.CloseTask(ctx, pool, pubID, c); err != nil {
				t.Fatalf("close: %v", err)
			}
			return call(pubBot, "task_pause", map[string]any{"code": c})
		}},
		{"VALIDATION_FAILED", "", 422, func() (map[string]any, int) {
			bad := pubContractArg(3600)
			bad["title"] = ""
			return call(pubBot, "task_create", map[string]any{"contract": bad, "budget": 2000})
		}},
		{"TEST_DELIVERY_FAILED", "", 422, func() (map[string]any, int) {
			return call(pubBot, "task_open", map[string]any{"code": syncBrokenTask(t, pool, pubID)})
		}},
		{"VERDICT_INVALID", "", 422, func() (map[string]any, int) {
			return call(pubBot, "task_verdict", map[string]any{
				"submission_id": freshView.SubmissionID,
				"verdict":       map[string]any{"accepted": false, "criteria": []string{"C9"}, "reason": "undeclared"},
			})
		}},
		{"NOT_UNDER_REVIEW", "", 409, func() (map[string]any, int) {
			return call(pubBot, "task_verdict", map[string]any{
				"submission_id": subView.SubmissionID, // already settled by the timeout-past verdict above
				"verdict":       map[string]any{"accepted": true},
			})
		}},
		{"HAS_RESERVATIONS", "", 409, func() (map[string]any, int) {
			c := wo7OpenTask(t, pool, pubID)
			if _, err := service.ClaimTask(ctx, pool, agentID, c, time.Now()); err != nil {
				t.Fatalf("claim: %v", err)
			}
			_, err := service.CloseTask(ctx, pool, pubID, c)
			if err != nil {
				t.Fatalf("close: %v", err)
			}
			return call(pubBot, "task_refund", map[string]any{"code": c})
		}},
		{"NOTHING_TO_REFUND", "", 409, func() (map[string]any, int) {
			c := wo7OpenTask(t, pool, pubID)
			if _, err := service.CloseTask(ctx, pool, pubID, c); err != nil {
				t.Fatalf("close: %v", err)
			}
			if _, err := service.RefundTask(ctx, pool, pubID, c); err != nil {
				t.Fatalf("first refund: %v", err)
			}
			return call(pubBot, "task_refund", map[string]any{"code": c})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			env, status := tc.trigger()
			if status != tc.status {
				t.Fatalf("status = %d, want %d (%v)", status, tc.status, env)
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj == nil || errObj["code"] != tc.code {
				t.Fatalf("error = %v, want %s (%v)", env["error"], tc.code, env)
			}
			na := env["next_action"]
			if tc.next == "" {
				if na != nil {
					t.Fatalf("next_action = %v, want null", na)
				}
			} else if na != tc.next {
				t.Fatalf("next_action = %v, want %s", na, tc.next)
			}
		})
	}

	// MCP channel spot-check: three codes with isError=true and the
	// same structuredContent shape
	for _, tc := range []struct{ tool, code string }{
		{"work_get", "TASK_NOT_FOUND"},
		{"task_pause", "NOT_OWNER"},
		{"work_submit", "SCHEMA_MISMATCH"},
	} {
		var env map[string]any
		var isErr bool
		switch tc.tool {
		case "work_get":
			env, isErr, _ = mcpCall(t, srv, agentKey, "work_get", map[string]any{"code": "nope00000000"})
		case "task_pause":
			env, isErr, _ = mcpCall(t, srv, agentKey, "task_pause", map[string]any{"code": code})
		case "work_submit":
			env, isErr, _ = mcpCall(t, srv, agentKey, "work_submit", map[string]any{
				"code": code, "request_key": fmt.Sprintf("mcpbad-%d", time.Now().UnixNano()),
				"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"one"}},
			})
		}
		if !isErr || env["error"].(map[string]any)["code"] != tc.code {
			t.Fatalf("MCP %s: isError=%v env=%v", tc.tool, isErr, env)
		}
		for _, k := range []string{"ok", "error", "next_action", "retry_after"} {
			if _, has := env[k]; !has {
				t.Fatalf("MCP %s: envelope missing %s", tc.tool, k)
			}
		}
	}
}

// helpers

func draftOf(t *testing.T, pool *pg.Pool, publisher int64) string {
	t.Helper()
	view, err := service.CreateTask(context.Background(), pool, publisher, wo7ContractArg(), 1000)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return view["code"].(string)
}

func wo7ContractArg() task.Contract {
	w := int64(3600)
	return task.Contract{
		Title: "C", Objective: "o", Inputs: "i",
		Output: task.Output{Description: "d", Schema: []byte(`{
			"type":"object","properties":{
				"url":{"type":"string"},
				"bullets":{"type":"array","items":{"type":"string"},"minItems":3,"maxItems":3}
			},"required":["url","bullets"]}`)},
		Acceptance: task.Acceptance{Mode: task.ModeAsync, ReviewWindow: &w,
			Criteria: []task.Criterion{{ID: "C1", Kind: task.KindRule, Description: "r"}}},
		Examples: []task.Example{
			{Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), Accepted: true},
		},
		Price: 5,
	}
}

func wo7OpenTaskClaimRequired(t *testing.T, pool *pg.Pool, publisher int64) string {
	c := wo7ContractArg()
	c.Claim = task.ClaimConfig{Required: true}
	view, err := service.CreateTask(context.Background(), pool, publisher, c, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if _, err := service.OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

func seedRejectedSub(t *testing.T, pool *pg.Pool, code string, agent int64) {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, Version: 1, AgentID: agent,
		RequestKey:  fmt.Sprintf("rej-%d", time.Now().UnixNano()),
		Payload:     []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
		PayloadHash: task.PayloadHash([]byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`)),
		Amount:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliver4XX, nil); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
}

// drainSlots reserves the task's entire budget via claims by other
// agents so slots hit 0.
func drainSlots(t *testing.T, srv *httptest.Server, pool *pg.Pool, code string, slots int) error {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < slots; i++ {
		_, _, id := m2RegisterSeeded(t, srv, pool, 0)
		if _, err := service.ClaimTask(ctx, pool, id, code, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

func fakeCred() string { return "kf_live_" + strings.Repeat("ab", 32) }

// syncBrokenTask creates a sync task whose receiver is unreachable:
// open runs the test delivery and fails with TEST_DELIVERY_FAILED.
func syncBrokenTask(t *testing.T, pool *pg.Pool, publisher int64) string {
	t.Helper()
	c := wo7ContractArg()
	c.Acceptance = task.Acceptance{Mode: task.ModeSync,
		Criteria: []task.Criterion{{ID: "C1", Kind: task.KindRule, Description: "r"}}}
	c.Receiver = task.Receiver{URL: "https://receiver.invalid/hook"}
	view, err := service.CreateTask(context.Background(), pool, publisher, c, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return view["code"].(string)
}

// TestPublisherLifecycle: task_create → task_open (sync test delivery
// 2xx via the TLS receiver) → work_submit → settled → task_close →
// task_refund, all through the registry, ending with CheckInvariants.
func TestPublisherLifecycle(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()

	pubKey, pubName, pubID := m2RegisterSeeded(t, srv, pool, 10_000)
	agentKey, _, agentID := m2RegisterSeeded(t, srv, pool, 0)
	pubBot := wo7Bot(t, pool, pubID)
	agentBot := wo7Bot(t, pool, agentID)

	// TLS receiver answering 2xx (the package test CA)
	rcv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	rcv.TLS = &tls.Config{Certificates: []tls.Certificate{wo7TLSCert}}
	rcv.StartTLS()
	t.Cleanup(rcv.Close)

	call := func(bot *model.Bot, tool string, args map[string]any) (map[string]any, int) {
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		return env, status
	}

	// create (sync contract against the receiver)
	contract := wo7ContractArg()
	contract.Acceptance = task.Acceptance{Mode: task.ModeSync,
		Criteria: []task.Criterion{{ID: "C1", Kind: task.KindRule, Description: "r"}}}
	contract.Receiver = task.Receiver{URL: rcv.URL}
	env, status := call(pubBot, "task_create", map[string]any{"contract": asContractMap(t, contract), "budget": 2000})
	if status != 200 || env["status"] != task.TaskDraft {
		t.Fatalf("create: %d %v", status, env)
	}
	code := env["code"].(string)

	// open — the test delivery hits the 2xx receiver
	env, status = call(pubBot, "task_open", map[string]any{"code": code})
	if status != 200 || env["status"] != task.TaskOpen || numOff(env["version"]) != float64(1) {
		t.Fatalf("open: %d %v", status, env)
	}

	// executor submits → synchronous 2xx → settled
	env, status = call(agentBot, "work_submit", map[string]any{
		"code": code, "request_key": "life-1",
		"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
	})
	if status != 200 || env["state"] != task.SubSettled || env["next_action"] != "done" || numOff(env["paid"]) != 5 {
		t.Fatalf("submit: %d %v", status, env)
	}

	// close (reserved drained by settlement) then refund
	env, status = call(pubBot, "task_close", map[string]any{"code": code})
	if status != 200 || env["status"] != task.TaskClosed {
		t.Fatalf("close: %d %v", status, env)
	}
	env, status = call(pubBot, "task_refund", map[string]any{"code": code})
	if status != 200 || numOff(env["refunded"]) != 1995 {
		t.Fatalf("refund: %d %v", status, env)
	}

	if err := task.CheckInvariants(ctx, pool, mustTaskIDOf(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
	_ = pubKey
	_ = agentKey
	_ = pubName
}

// asContractMap projects a Contract into a tool-argument map.
func asContractMap(t *testing.T, c task.Contract) map[string]any {
	t.Helper()
	m, err := asMap(c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRateLimitProtocol: a Deps with a 1-per-60s work_submit limiter —
// the second consecutive work_submit is 429 RATE_LIMIT with
// next_action=wait and a positive integer retry_after.
func TestRateLimitProtocol(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, ratelimit.NewLimiter(map[string]ratelimit.Config{
		"task_submit": {Window: 60, Limit: 1, Enabled: true},
	}))
	deps.AgentRefKey = []byte("wo7c-key")
	srv := httptest.NewServer(Handler(deps))
	t.Cleanup(srv.Close)

	_, _, pubID := m2RegisterSeeded(t, srv, pool, 10_000)
	agentKey, _, agentID := m2RegisterSeeded(t, srv, pool, 0)
	agentBot := wo7Bot(t, pool, agentID)
	code := wo7OpenTask(t, pool, pubID)

	submit := func(key string) (map[string]any, int) {
		raw, _ := json.Marshal(map[string]any{
			"code": code, "request_key": fmt.Sprintf("rl-%d", time.Now().UnixNano()),
			"payload": map[string]any{"url": "https://example.com/a", "bullets": []string{"s1", "s2", "s3"}},
		})
		env, status := CallTool(context.Background(), &deps, "work_submit", agentBot, raw)
		return env, status
	}
	_, _ = submit(agentKey)

	env, status := submit(agentKey)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%v)", status, env)
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "RATE_LIMIT" || env["next_action"] != "wait" {
		t.Fatalf("envelope: %v", env)
	}
	ra := env["retry_after"]
	if numOff(ra) <= 0 {
		t.Fatalf("retry_after = %v, want a positive integer", ra)
	}
}
