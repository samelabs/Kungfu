package service

// Task 1.2 (WO-32) — §7.2 restricted audience: creation validation,
// immutability, the in-audience full flow, §12 minimal disclosure
// (an out-of-audience caller cannot distinguish a restricted task
// from a missing one), and §8 opportunity discovery (the offered
// work counts and the todo_list opportunities projection). Every
// test ends with task.CheckInvariants where a task was written.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// restrictedContract is the default test contract restricted to the
// named agents on the package-wide accept receiver.
func restrictedContract(names ...string) task.Contract {
	c := submitContract() // claim NOT required, price 5
	c.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: names}
	return c
}

// audOpen creates (create + open) a task for publisher with the
// contract and returns its code.
func audOpen(t *testing.T, pool *pg.Pool, publisher int64, c task.Contract, budget int64) string {
	t.Helper()
	code := pubCreateForTest(t, pool, publisher, c, budget)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

// -- §7.2 creation validation --

func TestTaskAudienceCreateValidation(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	publisherName := audBotName(t, pool, publisher)
	agent := pubSeedBot(t, pool, 0)
	agentName := audBotName(t, pool, agent)
	ctx := context.Background()

	cases := []struct {
		name      string
		audience  *task.Audience
		wantField string
		wantInMsg string
	}{
		{
			name:      "unknown name",
			audience:  &task.Audience{Type: task.AudienceRestricted, Agents: []string{"no_such_agent_42"}},
			wantField: "audience.agents[0]",
			wantInMsg: "is not a known agent name",
		},
		{
			name:      "duplicate name",
			audience:  &task.Audience{Type: task.AudienceRestricted, Agents: []string{agentName, agentName}},
			wantField: "audience.agents[1]",
			wantInMsg: "appears more than once",
		},
		{
			name:      "publisher itself",
			audience:  &task.Audience{Type: task.AudienceRestricted, Agents: []string{agentName, publisherName}},
			wantField: "audience.agents[1]",
			wantInMsg: "is you: the publisher may not be part of the audience",
		},
		{
			name:      "over 50 names",
			audience:  &task.Audience{Type: task.AudienceRestricted, Agents: audManyNames(agentName, 51)},
			wantField: "audience.agents",
			wantInMsg: "must contain between 1 and 50 entries, got 51",
		},
		{
			name:      "restricted with no names",
			audience:  &task.Audience{Type: task.AudienceRestricted},
			wantField: "audience.agents",
			wantInMsg: "must contain between 1 and 50 entries, got 0",
		},
		{
			name:      "open with names",
			audience:  &task.Audience{Type: task.AudienceOpen, Agents: []string{agentName}},
			wantField: "audience.agents",
			wantInMsg: "only a restricted audience names agents",
		},
		{
			name:      "unknown type",
			audience:  &task.Audience{Type: "secret", Agents: []string{agentName}},
			wantField: "audience.type",
			wantInMsg: `must be "open" or "restricted"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := pubContract("")
			c.Audience = tc.audience
			_, err := CreateTask(ctx, pool, publisher, c, 100)
			appErr := appErrOf(t, err)
			if appErr.Code != "VALIDATION_FAILED" {
				t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
			}
			items, _ := appErr.Details["errors"].([]map[string]string)
			if len(items) == 0 {
				t.Fatalf("details.errors empty: %#v", appErr.Details)
			}
			for _, it := range items {
				if it["field"] == tc.wantField && strings.Contains(it["message"], tc.wantInMsg) {
					return // found
				}
			}
			t.Fatalf("no error at field %s containing %q: %#v", tc.wantField, tc.wantInMsg, items)
		})
	}
	// nothing was written: no task of this publisher exists (the
	// shared test DB carries other tests' audience rows — scope)
	var n int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_audience ta
		JOIN tb_task t ON t.id = ta.task_id
		WHERE t.publisher_id = $1`, publisher).Scan(&n); err != nil || n != 0 {
		t.Fatalf("publisher task_audience rows = %d err=%v, want 0 (failed creates write nothing)", n, err)
	}
	var tasks int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task WHERE publisher_id = $1`, publisher).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("publisher tasks = %d err=%v, want 0", tasks, err)
	}
}

// audManyNames repeats name n times.
func audManyNames(name string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = name
	}
	return out
}

// audBotName reads a bot's name back (the audience is written in
// names; the seeds generate them).
func audBotName(t *testing.T, pool *pg.Pool, id int64) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT bot_name FROM tb_bots WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("bot name: %v", err)
	}
	return name
}

// -- §7.2 immutability --

func TestTaskAudienceImmutableOnUpdate(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	a := pubSeedBot(t, pool, 0)
	b := pubSeedBot(t, pool, 0)
	an, bn := audBotName(t, pool, a), audBotName(t, pool, b)
	ctx := context.Background()

	c := pubContract("")
	c.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{an, bn}}
	code := pubCreateForTest(t, pool, publisher, c, 100) // paused

	// every difference is rejected with the §7.2 message
	rejections := []struct {
		name string
		aud  *task.Audience
	}{
		{"different names", &task.Audience{Type: task.AudienceRestricted, Agents: []string{an}}},
		{"added name", &task.Audience{Type: task.AudienceRestricted, Agents: []string{an, bn, "another_agent_00"}}},
		{"to open", nil},
		{"to explicit open", &task.Audience{Type: task.AudienceOpen}},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			upd := pubContract("")
			upd.Title = "revised"
			upd.Audience = tc.aud
			_, err := UpdateTask(ctx, pool, publisher, code, upd)
			appErr := appErrOf(t, err)
			if appErr.Code != "VALIDATION_FAILED" {
				t.Fatalf("code = %s, want VALIDATION_FAILED", appErr.Code)
			}
			items, _ := appErr.Details["errors"].([]map[string]string)
			if len(items) != 1 || items[0]["field"] != "audience" ||
				items[0]["message"] != "audience is fixed at creation; publish a new task for a different audience" {
				t.Fatalf("details.errors = %#v", appErr.Details)
			}
		})
	}

	// the SAME audience in a different order passes; the audience
	// (and its sorted storage) is unchanged on the new version
	same := pubContract("")
	same.Title = "revised, same audience"
	same.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{bn, an}}
	view, err := UpdateTask(ctx, pool, publisher, code, same)
	if err != nil {
		t.Fatalf("update with same audience: %v", err)
	}
	if v, _ := view["contract_version"].(int64); v != 2 {
		t.Fatalf("contract_version = %v, want 2", view["contract_version"])
	}
	aud, _ := view["audience"].(map[string]any)
	if aud["type"] != task.AudienceRestricted {
		t.Fatalf("audience = %#v", view["audience"])
	}
	names, _ := aud["agents"].([]string)
	if len(names) != 2 || names[0] != an || names[1] != bn { // sorted
		t.Fatalf("agents = %#v, want sorted [%s %s]", aud["agents"], an, bn)
	}

	// an open task gains no audience either
	open := pubContract("")
	openCode := pubCreateForTest(t, pool, publisher, open, 100)
	restricted := pubContract("")
	restricted.Title = "no audience change"
	restricted.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{an}}
	_, err = UpdateTask(ctx, pool, publisher, openCode, restricted)
	if appErrOf(t, err).Code != "VALIDATION_FAILED" {
		t.Fatalf("open → restricted via update: %v", err)
	}
	// while the identical open audience is the non-change it is
	openUpd := pubContract("")
	openUpd.Title = "still open"
	openUpd.Audience = &task.Audience{Type: task.AudienceOpen}
	if _, err := UpdateTask(ctx, pool, publisher, openCode, openUpd); err != nil {
		t.Fatalf("open → open via update: %v", err)
	}

	// task_get / task_list carry the audience with resolved names
	got, err := GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	if aud, _ := got["audience"].(map[string]any); aud["type"] != task.AudienceRestricted {
		t.Fatalf("task_get audience = %#v", got["audience"])
	}
	rows, _, err := ListTasks(ctx, pool, publisher, TaskListFilter{Code: code})
	if err != nil || len(rows) != 1 {
		t.Fatalf("task_list: %v %d", err, len(rows))
	}
	if aud, _ := rows[0]["audience"].(map[string]any); aud["type"] != task.AudienceRestricted {
		t.Fatalf("task_list audience = %#v", rows[0]["audience"])
	}

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §7.2 the in-audience flow runs exactly like open work --

func TestRestrictedTaskAudienceFlow(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	named := pubSeedBot(t, pool, 0)
	named2 := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	ref := fmt.Sprintf("a%011d", time.Now().UnixNano()%100_000_000_000) // 12 chars, like publiccode
	pubSeedKungfu(t, pool, publisher, ref, "Audience harness")

	c := restrictedContract(audBotName(t, pool, named), audBotName(t, pool, named2)) // claim NOT required, price 5
	c.HarnessRefs = []string{ref}
	code := audOpen(t, pool, publisher, c, 10)

	// work_list carries the restricted marker for a named agent
	items, _, err := ListWork(ctx, pool, named, time.Now(), WorkListFilter{})
	if err != nil {
		t.Fatalf("ListWork: %v", err)
	}
	found := false
	for _, it := range items {
		if it["code"] == code {
			found = true
			if it["audience"] != task.AudienceRestricted {
				t.Fatalf("row audience = %#v, want restricted", it["audience"])
			}
		}
	}
	if !found {
		t.Fatalf("restricted task missing from the named agent's work_list: %v", items)
	}

	// work_get / work_harness / work_claim / work_submit — the full
	// engagement, judged by the accept receiver, settled in credits
	wv, err := GetWork(ctx, pool, named, code, time.Now())
	if err != nil {
		t.Fatalf("work_get: %v", err)
	}
	contract, _ := wv["contract"].(map[string]any)
	if aud, _ := contract["audience"].(map[string]any); aud["type"] != task.AudienceRestricted {
		t.Fatalf("work_get contract audience = %#v", contract["audience"])
	}
	hv, err := GetHarness(ctx, pool, named, code, ref, nil, time.Now())
	if err != nil || hv["pinned"] != false {
		t.Fatalf("work_harness: %v %+v", err, hv)
	}
	claim, err := ClaimTask(ctx, pool, named, code, time.Now())
	if err != nil {
		t.Fatalf("work_claim: %v", err)
	}
	view, err := SubmitWork(ctx, pool, named, SubmitInput{
		Code: code, RequestKey: "aud-flow-1", Payload: []byte(submitPayloadOK),
		ClaimID: &claim.ClaimID,
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("work_submit: %v", err)
	}
	if view.State != task.SubSettled || view.Paid != 5 {
		t.Fatalf("submission = %+v, want settled and paid 5", view)
	}

	// the second named agent runs the claim-less path just the same
	claimless, err := SubmitWork(ctx, pool, named2, SubmitInput{
		Code: code, RequestKey: "aud-flow-2", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil || claimless.State != task.SubSettled {
		t.Fatalf("claim-less submission: %v %+v", err, claimless)
	}

	// the publisher reads its own restricted task (the author is
	// always in §9's reader set); an outside agent is tested in the
	// disclosure test
	if _, err := GetWork(ctx, pool, publisher, code, time.Now()); err != nil {
		t.Fatalf("publisher work_get: %v", err)
	}
	// and an open task keeps its "open" marker
	openCode := audOpen(t, pool, publisher, pubContract(""), 10)
	items, _, err = ListWork(ctx, pool, named, time.Now(), WorkListFilter{Code: openCode})
	if err != nil || len(items) != 1 || items[0]["audience"] != task.AudienceOpen {
		t.Fatalf("open task row: %v %#v", err, items)
	}

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- §12 minimal disclosure --

func TestRestrictedTaskMinimalDisclosure(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	named := pubSeedBot(t, pool, 0)
	outside := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	c := pubContract("")
	c.Title = "sekritwo32title"
	c.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{audBotName(t, pool, named)}}
	code := audOpen(t, pool, publisher, c, 100)
	missing := "zzmissing01"

	// each work_* call: outside agent on the restricted task vs. a
	// task that does not exist — identical error, field for field
	type probe struct {
		name string
		call func(caller int64, code string) error
		// publisherSide: the tool answers NOT_OWNER to a non-publisher
		// once the audience gate lets them see the task at all
		publisherSide bool
	}
	probes := []probe{
		{"work_get", func(caller int64, code string) error {
			_, err := GetWork(ctx, pool, caller, code, now)
			return err
		}, false},
		{"work_harness", func(caller int64, code string) error {
			_, err := GetHarness(ctx, pool, caller, code, "anyref", nil, now)
			return err
		}, false},
		{"work_claim", func(caller int64, code string) error {
			_, err := ClaimTask(ctx, pool, caller, code, now)
			return err
		}, false},
		{"work_submit", func(caller int64, code string) error {
			_, err := SubmitWork(ctx, pool, caller, SubmitInput{
				Code: code, RequestKey: "probe", Payload: []byte(submitPayloadOK),
			}, testAgentRefKey, now)
			return err
		}, false},
		{"work_report", func(caller int64, code string) error {
			_, err := ReportTask(ctx, pool, caller, code, "probe report")
			return err
		}, false},
		{"work_history", func(caller int64, code string) error {
			_, _, err := ListHistory(ctx, pool, caller, code, 1, 20)
			return err
		}, false},
		// the publisher-side tools ride the same gate (WO-32a): an
		// out-of-audience caller must not learn the task exists from
		// NOT_OWNER either
		{"task_get", func(caller int64, code string) error {
			_, err := GetTask(ctx, pool, caller, code)
			return err
		}, true},
		{"task_update", func(caller int64, code string) error {
			upd := pubContract("") // valid, so the flow reaches the lookup
			upd.Title = "probe"
			_, err := UpdateTask(ctx, pool, caller, code, upd)
			return err
		}, true},
		{"task_open", func(caller int64, code string) error {
			_, err := OpenTask(ctx, pool, caller, code)
			return err
		}, true},
		{"task_pause", func(caller int64, code string) error {
			_, err := PauseTask(ctx, pool, caller, code)
			return err
		}, true},
		{"task_close", func(caller int64, code string) error {
			_, err := CloseTask(ctx, pool, caller, code)
			return err
		}, true},
		{"task_fund", func(caller int64, code string) error {
			_, err := FundTask(ctx, pool, caller, code, 100)
			return err
		}, true},
		{"task_refund", func(caller int64, code string) error {
			_, err := RefundTask(ctx, pool, caller, code)
			return err
		}, true},
		{"task_submissions", func(caller int64, code string) error {
			_, _, err := ListSubmissionsForPublisher(ctx, pool, caller, code, "", 1, 20, testAgentRefKey)
			return err
		}, true},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			restrictedErr := appErrOf(t, p.call(outside, code))
			missingErr := appErrOf(t, p.call(outside, missing))
			if restrictedErr.Code != missingErr.Code ||
				restrictedErr.Message != missingErr.Message ||
				fmt.Sprint(restrictedErr.Details) != fmt.Sprint(missingErr.Details) {
				t.Fatalf("restricted = {%s %s %v}, missing = {%s %s %v} — they must be indistinguishable",
					restrictedErr.Code, restrictedErr.Message, restrictedErr.Details,
					missingErr.Code, missingErr.Message, missingErr.Details)
			}
			if restrictedErr.Code != "TASK_NOT_FOUND" {
				t.Fatalf("code = %s, want TASK_NOT_FOUND", restrictedErr.Code)
			}
			// an in-audience NON-publisher sees the task (the gate
			// opens) and is then denied by ownership, not by
			// existence — on the publisher tools only; the work tools
			// serve the named agent as usual (flow test)
			if p.publisherSide {
				namedErr := appErrOf(t, p.call(named, code))
				if namedErr.Code != "NOT_OWNER" {
					t.Fatalf("in-audience non-publisher: code = %s, want NOT_OWNER", namedErr.Code)
				}
			}
		})
	}

	// work_list: the outside agent's default listing (and its total)
	// never contains the restricted task — even by exact code
	for _, tc := range []struct {
		name string
		f    WorkListFilter
	}{{"default", WorkListFilter{}}, {"by code", WorkListFilter{Code: code}}} {
		items, total, err := ListWork(ctx, pool, outside, now, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, it := range items {
			if it["code"] == code {
				t.Fatalf("%s: restricted task listed to the outside agent", tc.name)
			}
		}
		if tc.name == "by code" && (len(items) != 0 || total != 0) {
			t.Fatalf("%s: items=%d total=%d, want 0/0", tc.name, len(items), total)
		}
	}

	// the anonymous homepage board: no listing and no code probe
	rows, total, err := ListWorkBoard(ctx, pool, "", code, 1, 20)
	if err != nil || len(rows) != 0 || total != 0 {
		t.Fatalf("board code probe: %v %d/%d, want empty", err, len(rows), total)
	}
	rows, _, err = ListWorkBoard(ctx, pool, "sekritwo32title", "", 1, 20)
	if err != nil {
		t.Fatalf("board keyword: %v", err)
	}
	for _, r := range rows {
		if r.Code == code {
			t.Fatal("restricted task on the public board")
		}
	}

	// the named agent still finds it by the same code probe
	items, total, err := ListWork(ctx, pool, named, now, WorkListFilter{Code: code})
	if err != nil || len(items) != 1 || total != 1 {
		t.Fatalf("named agent code probe: %v %d/%d", err, len(items), total)
	}

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}
