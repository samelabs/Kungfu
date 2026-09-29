package service

// Agent read interface tests (WO-6a) — real PostgreSQL. Every test
// ends with task.CheckInvariants.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// workOpenTask opens a task on the accept-everything receiver with an
// optional mutation.
func workOpenTask(t *testing.T, pool *pg.Pool, publisher int64, budget int64, mutate func(*task.Contract)) *repository.TaskRow {
	t.Helper()
	code := submitOpenedTask(t, pool, publisher, budget, mutate)
	tr, err := repository.FindTaskByCode(context.Background(), pool, code)
	if err != nil || tr == nil {
		t.Fatalf("reload task: %v", err)
	}
	return tr
}

// -- ListWork inclusion / exclusion --

func TestListWorkFilters(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 10_000) // doubles as publisher of the own-task row
	other := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// eligible
	eligible := workOpenTask(t, pool, publisher, 1000, nil)
	// slots = 0 (fully reserved by another agent's claim)
	exhausted := workOpenTask(t, pool, publisher, 1000, func(c *task.Contract) { c.Price = 1000 })
	if _, err := ClaimTask(ctx, pool, other, exhausted.Code, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// paused
	paused := workOpenTask(t, pool, publisher, 1000, nil)
	if _, err := PauseTask(ctx, pool, publisher, paused.Code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// own task
	own := workOpenTask(t, pool, agent, 1000, nil)
	_ = own
	// agent hit the rejected cap (缺省 5)
	capped := workOpenTask(t, pool, publisher, 1000, nil)
	for i := 0; i < 5; i++ {
		seedSubmission(t, pool, capped.Code, agent, task.SubRejected)
	}

	items, _, err := ListWork(ctx, pool, agent, time.Now(), WorkListFilter{})
	if err != nil {
		t.Fatalf("ListWork: %v", err)
	}
	codes := map[string]bool{}
	for _, it := range items {
		codes[it["code"].(string)] = true
	}
	if !codes[eligible.Code] {
		t.Fatalf("eligible task missing: %v", codes)
	}
	for _, banned := range []string{exhausted.Code, paused.Code, own.Code, capped.Code} {
		if codes[banned] {
			t.Fatalf("task %s should be excluded", banned)
		}
	}
	if err := task.CheckInvariants(ctx, pool, exhausted.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestListWorkOrderAndCap(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 1_000_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// 102 eligible tasks; page 1 at the maximum page size carries 100
	// of them, newest open first, and total reports all 102 (WO-19 Q1)
	codes := make([]string, 0, 102)
	for i := 0; i < 102; i++ {
		tr := workOpenTask(t, pool, publisher, 1000, nil)
		codes = append(codes, tr.Code)
	}
	items, total, err := ListWork(ctx, pool, agent, time.Now(), WorkListFilter{PageSize: 100})
	if err != nil {
		t.Fatalf("ListWork: %v", err)
	}
	if len(items) != 100 {
		t.Fatalf("items = %d, want the 100 page size cap", len(items))
	}
	if total < 102 {
		t.Fatalf("total = %d, want at least 102 (all matching rows, not just the page)", total)
	}
	got := make([]string, 0, 100)
	for _, it := range items {
		got = append(got, it["code"].(string))
	}
	// newest first: the first item is the LAST created task
	if got[0] != codes[len(codes)-1] {
		t.Fatalf("first item = %s, want %s (newest open first)", got[0], codes[len(codes)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != codes[len(codes)-1-i] {
			t.Fatalf("order broken at %d: %s, want %s", i, got[i], codes[len(codes)-1-i])
		}
	}
}

// -- WO-19 Q1: keyword, code, paging, LIKE escaping --
//
// The test database is shared across every package's tests, so
// assertions never count unfiltered rows; each case scopes itself
// with a keyword unique to this test (pagingMarker).

func TestListWorkSearchAndPaging(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 1_000_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	const pagingMarker = "wo19pagequery7f3c"
	marked := func(title string) func(*task.Contract) {
		return func(c *task.Contract) {
			c.Title = title
			c.Requirements = "Paging fixture " + pagingMarker + "."
		}
	}

	codesOf := func(items []map[string]any) map[string]bool {
		out := map[string]bool{}
		for _, it := range items {
			out[it["code"].(string)] = true
		}
		return out
	}

	// five eligible tasks carrying the marker
	needle := workOpenTask(t, pool, publisher, 1000, marked("Translate Hungarian Poetry"))
	plain := workOpenTask(t, pool, publisher, 1000, marked("Summarize a page"))
	wild := workOpenTask(t, pool, publisher, 1000, marked("100% done checklist"))
	snake := workOpenTask(t, pool, publisher, 1000, marked("snake_case naming review"))
	inReq := workOpenTask(t, pool, publisher, 1000, func(c *task.Contract) {
		c.Title = "Fetch product facts"
		c.Requirements = "Fetch the given page and return three summary bullets mentioning Kryptonite. " + pagingMarker
	})

	// keyword: case-insensitive over title
	items, total, err := ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: "HUNGARIAN"})
	if err != nil {
		t.Fatalf("q title: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0]["code"] != needle.Code {
		t.Fatalf("q=HUNGARIAN: total=%d items=%v, want only %s", total, codesOf(items), needle.Code)
	}
	// keyword over requirements too
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: "kryptonite"})
	if err != nil {
		t.Fatalf("q requirements: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0]["code"] != inReq.Code {
		t.Fatalf("q=kryptonite: total=%d items=%v, want only %s", total, codesOf(items), inReq.Code)
	}

	// LIKE wildcards are matched literally: q="%" returns ONLY the
	// task whose title contains a literal percent sign — the other
	// known tasks (no % anywhere) must not leak in as they would if
	// the wildcard went unescaped
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: "%"})
	if err != nil {
		t.Fatalf("q percent: %v", err)
	}
	got := codesOf(items)
	if !got[wild.Code] || got[needle.Code] || got[plain.Code] || got[snake.Code] || got[inReq.Code] || total != int64(len(items)) {
		t.Fatalf("q=%% must match only %s: total=%d items=%v", wild.Code, total, got)
	}
	// ...and q="_" only the task with a literal underscore
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: "_"})
	if err != nil {
		t.Fatalf("q underscore: %v", err)
	}
	got = codesOf(items)
	if !got[snake.Code] || got[needle.Code] || got[plain.Code] || got[wild.Code] || got[inReq.Code] || total != int64(len(items)) {
		t.Fatalf("q=_ must match only %s: total=%d items=%v", snake.Code, total, got)
	}

	// code: exact match
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Code: plain.Code})
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0]["code"] != plain.Code {
		t.Fatalf("code=%s: total=%d items=%v", plain.Code, total, codesOf(items))
	}
	// code of a cap-exhausted task: empty list, zero total
	capped := workOpenTask(t, pool, publisher, 1000, marked("Cap exhausted fixture"))
	for i := 0; i < 5; i++ {
		seedSubmission(t, pool, capped.Code, agent, task.SubRejected)
	}
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Code: capped.Code})
	if err != nil {
		t.Fatalf("code capped: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("capped code must yield an empty list: total=%d items=%v", total, codesOf(items))
	}
	// the exhausted task never appears in the marker set either
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: "Cap exhausted"})
	if err != nil {
		t.Fatalf("q capped: %v", err)
	}
	if codesOf(items)[capped.Code] {
		t.Fatalf("cap-exhausted task must be absent: %v", codesOf(items))
	}

	// paging: the five marker tasks at page_size 2 → 2 + 2 + 1, newest
	// first; total is 5 on every page
	var paged []string
	for page := 1; page <= 3; page++ {
		items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: pagingMarker, Page: page, PageSize: 2})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if total != 5 {
			t.Fatalf("page %d: total=%d, want 5 on every page", page, total)
		}
		if want := []int{2, 2, 1}[page-1]; len(items) != want {
			t.Fatalf("page %d: %d items, want %d", page, len(items), want)
		}
		for _, it := range items {
			paged = append(paged, it["code"].(string))
		}
	}
	// the concatenated pages equal one full page of the same filter in order
	items, _, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: pagingMarker, PageSize: 5})
	if err != nil {
		t.Fatalf("full page: %v", err)
	}
	full := make([]string, 0, len(items))
	for _, it := range items {
		full = append(full, it["code"].(string))
	}
	if strings.Join(paged, ",") != strings.Join(full, ",") {
		t.Fatalf("paged order %v != full order %v", paged, full)
	}

	// WO-19b: an out-of-range page keeps the FULL total — a window
	// COUNT would collapse to 0 as soon as the page has no rows
	items, total, err = ListWork(ctx, pool, agent, time.Now(), WorkListFilter{Q: pagingMarker, Page: 4, PageSize: 2})
	if err != nil {
		t.Fatalf("out-of-range page: %v", err)
	}
	if len(items) != 0 || total != 5 {
		t.Fatalf("out-of-range page: %d items, total=%d, want 0/5", len(items), total)
	}
}

// -- statistics precision --

func TestListWorkStatsPrecision(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// Task A: 2 settled, 1 rejected, 1 failed → accept_rate 2/3,
	// failure_rate 1/4, median over the three reply durations.
	codeA := workOpenTask(t, pool, publisher, 1000, nil).Code
	seedSubmission(t, pool, codeA, agent, task.SubSettled)
	seedSubmission(t, pool, codeA, agent, task.SubRejected)
	seedSubmission(t, pool, codeA, agent, task.SubSettled)
	// one failed submission
	tx, _ := pool.TxBegin(ctx)
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: mustTaskID(t, pool, codeA), Version: 1, AgentID: agent,
		RequestKey: fmt.Sprintf("f-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)), Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert failed-seed: %v", err)
	}
	if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliveryFailed,
		&repository.SetSubmissionStateOpts{Failure: strPtrOf("RECEIVER_FAULT")}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	_ = tx.Commit(ctx)

	// Task B: no terminals → all rates null
	codeB := workOpenTask(t, pool, publisher, 1000, nil).Code

	items, _, err := ListWork(ctx, pool, agent, time.Now(), WorkListFilter{})
	if err != nil {
		t.Fatalf("ListWork: %v", err)
	}
	byCode := map[string]map[string]any{}
	for _, it := range items {
		byCode[it["code"].(string)] = it
	}
	a := byCode[codeA]["stats"].(workStats)
	if a.AcceptRate == nil || *a.AcceptRate != 2.0/3.0 {
		t.Fatalf("accept_rate = %v, want 2/3 (2 settled, 1 rejected)", a.AcceptRate)
	}
	if a.FailureRate == nil || *a.FailureRate != 0.25 {
		t.Fatalf("failure_rate = %v, want 1/4", a.FailureRate)
	}
	if a.MedianReplySeconds == nil {
		t.Fatalf("median missing: %+v", a)
	}
	b := byCode[codeB]["stats"].(workStats)
	if b.AcceptRate != nil || b.FailureRate != nil || b.MedianReplySeconds != nil {
		t.Fatalf("empty stats must be all null: %+v", b)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, codeA)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func strPtrOf(s string) *string { return &s }

// -- GetWork / GetHarness visibility and version pinning --

func TestGetWorkVisibility(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	draft := pubCreateForTest(t, pool, publisher, submitContract(), 1000)
	if _, err := GetWork(ctx, pool, agent, draft, time.Now()); appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("draft: %v, want TASK_NOT_FOUND", err)
	}

	closed := workOpenTask(t, pool, publisher, 1000, nil)
	if _, err := CloseTask(ctx, pool, publisher, closed.Code); err != nil {
		t.Fatalf("close: %v", err)
	}
	view, err := GetWork(ctx, pool, agent, closed.Code, time.Now())
	if err != nil {
		t.Fatalf("closed GetWork: %v", err)
	}
	if view["status"] != task.TaskClosed {
		t.Fatalf("status = %v, want closed", view["status"])
	}
	// WO-20b: the executor-side contract projection carries no sample
	if _, has := view["contract"].(map[string]any)["sample"]; has {
		t.Fatal("work_get contract projection still carries sample")
	}
	if err := task.CheckInvariants(ctx, pool, closed.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

func TestGetWorkVersionPinnedByClaim(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	// v1 with a harness ref owned by the publisher
	pubSeedKungfu(t, pool, publisher, "harnessref01", "Harness One")
	c := claimContract() // claim.required = true
	c.HarnessRefs = []string{"harnessref01"}
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := ClaimTask(ctx, pool, agent, code, now); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// publisher pauses + edits (v2: new title, no harness) + reopens
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	updated := claimContract()
	updated.Title = "Version two"
	updated.HarnessRefs = nil
	if _, err := UpdateTask(ctx, pool, publisher, code, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	view, err := GetWork(ctx, pool, agent, code, now)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if view["version"] != int32(1) {
		t.Fatalf("version = %v, want the claim's v1", view["version"])
	}
	contractJSON, _ := json.Marshal(view["contract"])
	if !strings.Contains(string(contractJSON), "Summarize a page") {
		t.Fatalf("v1 contract expected: %s", contractJSON)
	}
	harness, _ := view["harness"].([]map[string]any)
	if len(harness) != 1 || harness[0]["ref_id"] != "harnessref01" {
		t.Fatalf("harness = %v, want the v1 snapshot entry", harness)
	}

	hv, err := GetHarness(ctx, pool, agent, code, "harnessref01")
	if err != nil {
		t.Fatalf("GetHarness: %v", err)
	}
	if hv["content"] != "harness body" {
		t.Fatalf("harness content = %v", hv["content"])
	}
	if _, err := GetHarness(ctx, pool, agent, code, "nope00000000"); appErrOf(t, err).Code != "HARNESS_REF_NOT_FOUND" {
		t.Fatalf("missing ref: %v, want HARNESS_REF_NOT_FOUND", err)
	}
	claimTaskReserved(t, pool, code)
}

// -- §10.8: no receiver / publisher identity in executor output --

func TestExecutorOutputHidesReceiverAndPublisher(t *testing.T) {
	pool := pubTestPool(t)
	rcv := startProgReceiver(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// a sync task with a receiver, one settled submission by the agent
	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	view, _ := deliverSubmit(t, pool, agent, code)
	subID := view.SubmissionID.Int64()

	// a harness-backed task for GetHarness
	pubSeedKungfu(t, pool, publisher, "harnessref02", "H")
	hc := claimContract()
	hc.HarnessRefs = []string{"harnessref02"}
	hcode := pubCreateForTest(t, pool, publisher, hc, 1000)
	if _, err := OpenTask(ctx, pool, publisher, hcode); err != nil {
		t.Fatalf("open: %v", err)
	}

	var publisherName string
	_ = pool.QueryRow(ctx, `SELECT bot_name FROM tb_bots WHERE id=$1`, publisher).Scan(&publisherName)
	// §10.8: no receiver, no publisher identity. The bare numeric id is
	// not banned as a substring (any number could contain it); the
	// dedicated key, the bot name and the receiver are. The receiver is
	// banned as a KEY shape ("receiver":) — verdict.source = "receiver"
	// is a §6.1 protocol value, not a leak.
	banned := []string{`"receiver":`, rcv.url, `"publisher_id"`, publisherName}

	assertClean := func(t *testing.T, name string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		s := string(raw)
		for _, b := range banned {
			if strings.Contains(s, b) {
				t.Fatalf("%s leaks %q: %.300s", name, b, s)
			}
		}
	}

	lw, _, err := ListWork(ctx, pool, stranger, time.Now(), WorkListFilter{})
	if err != nil {
		t.Fatalf("ListWork: %v", err)
	}
	assertClean(t, "ListWork", lw)
	gw, err := GetWork(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	assertClean(t, "GetWork", gw)
	gh, err := GetHarness(ctx, pool, agent, hcode, "harnessref02")
	if err != nil {
		t.Fatalf("GetHarness: %v", err)
	}
	assertClean(t, "GetHarness", gh)
	st, err := GetSubmissionStatus(ctx, pool, agent, &subID, "", "")
	if err != nil {
		t.Fatalf("GetSubmissionStatus: %v", err)
	}
	assertClean(t, "GetSubmissionStatus", st)
	hist, _, err := ListHistory(ctx, pool, agent, "", 1, 20)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	assertClean(t, "ListHistory", hist)
}

// -- work_status --

func TestGetSubmissionStatus(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := workOpenTask(t, pool, publisher, 1000, nil).Code
	key := fmt.Sprintf("ws-%d", time.Now().UnixNano())
	view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: key, Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	byID, err := GetSubmissionStatus(ctx, pool, agent, view.SubmissionID.Int64Ptr(), "", "")
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if byID["submission_id"].(string) != fmt.Sprint(view.SubmissionID.Int64()) {
		t.Fatalf("id mismatch: %v", byID["submission_id"])
	}
	events := byID["events"].([]eventView)
	if len(events) != 2 { // submit + the receiver's 200
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[1].To != task.SubSettled || events[1].Cause != task.EventDeliver2XX {
		t.Fatalf("last event = %+v", events[1])
	}

	byKey, err := GetSubmissionStatus(ctx, pool, agent, nil, code, key)
	if err != nil {
		t.Fatalf("by key: %v", err)
	}
	if byKey["submission_id"].(string) != fmt.Sprint(view.SubmissionID.Int64()) {
		t.Fatalf("key lookup mismatch")
	}

	if _, err := GetSubmissionStatus(ctx, pool, stranger, view.SubmissionID.Int64Ptr(), "", ""); appErrOf(t, err).Code != "SUBMISSION_NOT_FOUND" {
		t.Fatalf("stranger: %v, want SUBMISSION_NOT_FOUND", err)
	}
	if _, err := GetSubmissionStatus(ctx, pool, agent, nil, code, "unknown-key"); appErrOf(t, err).Code != "SUBMISSION_NOT_FOUND" {
		t.Fatalf("unknown key: %v", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- work_history --

func TestListHistory(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	codeA := workOpenTask(t, pool, publisher, 1000, nil).Code
	codeB := workOpenTask(t, pool, publisher, 1000, nil).Code
	for _, code := range []string{codeA, codeA, codeB} {
		if _, err := SubmitWork(ctx, pool, agent, SubmitInput{
			Code:       code,
			RequestKey: fmt.Sprintf("hist-%s-%d", code, time.Now().UnixNano()),
			Payload:    []byte(submitPayloadOK),
		}, testAgentRefKey, time.Now()); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	all, total, err := ListHistory(ctx, pool, agent, "", 1, 10)
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("all: rows=%d total=%d err=%v", len(all), total, err)
	}
	onlyA, totalA, err := ListHistory(ctx, pool, agent, codeA, 1, 10)
	if err != nil || totalA != 2 || len(onlyA) != 2 {
		t.Fatalf("filtered: rows=%d total=%d err=%v", len(onlyA), totalA, err)
	}
	for _, v := range onlyA {
		if v.TaskCode != codeA {
			t.Fatalf("filter leaked %s", v.TaskCode)
		}
	}
	page1, _, _ := ListHistory(ctx, pool, agent, "", 1, 2)
	page2, total2, _ := ListHistory(ctx, pool, agent, "", 2, 2)
	if len(page1) != 2 || len(page2) != 1 || total2 != 3 {
		t.Fatalf("pagination: %d/%d total=%d", len(page1), len(page2), total2)
	}
	if page1[0].SubmissionID == page1[1].SubmissionID || page1[0].SubmissionID == page2[0].SubmissionID {
		t.Fatal("pages overlap")
	}
	for _, code := range []string{codeA, codeB} {
		if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
			t.Fatalf("CheckInvariants: %v", err)
		}
	}
}

// TestWorkGovernanceReasons (T3): with a platform-set reason, work_get
// exposes paused_reason / closed_reason and the TASK_NOT_OPEN errors of
// work_claim, work_claim_renew and work_submit carry it in details.
func TestWorkGovernanceReasons(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	// claim.required so the agent holds a claim that survives the pause
	paused := workOpenTask(t, pool, publisher, 1000, func(c *task.Contract) { c.Claim.Required = true })
	cv, err := ClaimTask(ctx, pool, agent, paused.Code, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := PauseTask(ctx, pool, publisher, paused.Code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// the production platform pause is §7.3 RECEIVER_FAULT
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET paused_reason = 'RECEIVER_FAULT' WHERE code = $1`, paused.Code); err != nil {
		t.Fatalf("set paused_reason: %v", err)
	}

	if view, err := GetWork(ctx, pool, agent, paused.Code, now); err != nil || view["paused_reason"] != "RECEIVER_FAULT" {
		t.Fatalf("work_get: %v paused_reason=%v", err, view["paused_reason"])
	}
	if _, err := ClaimTask(ctx, pool, agent, paused.Code, now); err == nil {
		t.Fatal("claim on paused task succeeded")
	} else if e := appErrOf(t, err); e.Code != "TASK_NOT_OPEN" || e.Details["reason"] != "RECEIVER_FAULT" {
		t.Fatalf("claim: %s details=%v", e.Code, e.Details)
	}
	if _, err := RenewClaim(ctx, pool, agent, cv.ClaimID.Int64(), now); err == nil {
		t.Fatal("renew on paused task succeeded")
	} else if e := appErrOf(t, err); e.Code != "TASK_NOT_OPEN" || e.Details["reason"] != "RECEIVER_FAULT" {
		t.Fatalf("renew: %s details=%v", e.Code, e.Details)
	}
	if _, err := submitOnce(t, pool, agent, paused.Code, nil); err == nil {
		t.Fatal("claim-less submit on paused task succeeded")
	} else if e := appErrOf(t, err); e.Code != "TASK_NOT_OPEN" || e.Details["reason"] != "RECEIVER_FAULT" {
		t.Fatalf("submit: %s details=%v", e.Code, e.Details)
	}

	// platform close with a reason: closed_reason rides work_get and the
	// TASK_NOT_OPEN details the same way
	closed := workOpenTask(t, pool, publisher, 1000, nil)
	if _, err := CloseTask(ctx, pool, publisher, closed.Code); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task SET closed_reason = 'GOVERNANCE' WHERE code = $1`, closed.Code); err != nil {
		t.Fatalf("set closed_reason: %v", err)
	}
	if view, err := GetWork(ctx, pool, agent, closed.Code, now); err != nil || view["closed_reason"] != "GOVERNANCE" {
		t.Fatalf("work_get closed: %v closed_reason=%v", err, view["closed_reason"])
	}
	if _, err := ClaimTask(ctx, pool, agent, closed.Code, now); err == nil {
		t.Fatal("claim on closed task succeeded")
	} else if e := appErrOf(t, err); e.Code != "TASK_NOT_OPEN" || e.Details["reason"] != "GOVERNANCE" {
		t.Fatalf("claim closed: %s details=%v", e.Code, e.Details)
	}

	for _, code := range []string{paused.Code, closed.Code} {
		if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
			t.Fatalf("CheckInvariants: %v", err)
		}
	}
}
