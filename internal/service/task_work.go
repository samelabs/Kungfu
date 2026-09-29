package service

// Agent read interfaces (WO-6a) — spec §5.1 discovery, §6.3 statistics,
// §10.7 (version-consistent contract/harness) and §10.8 (receiver and
// publisher identity never exposed to executors). Pure reads; no
// protocol wiring.

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// §5.1/§6.3 presentation bounds.
const (
	workListDefaultPageSize = 20
	workListMaxPageSize     = 100
	workRequirementsExcerpt = 280 // runes
	statsWindow             = 30 * 24 * time.Hour
)

// WorkListFilter is the work_list query surface (WO-19 Q1): a
// keyword over title/requirements, an exact code, and paging. All
// fields optional; an absent filter returns page 1 at the default
// size. Code wins over keyword when both are given.
type WorkListFilter struct {
	Q        string
	Code     string
	Page     int
	PageSize int
}

// Normalize trims, resolves precedence and applies the paging
// defaults (page 1; page_size 20, range 1–100).
func (f *WorkListFilter) Normalize() {
	f.Q = strings.TrimSpace(f.Q)
	f.Code = strings.TrimSpace(f.Code)
	if f.Code != "" {
		f.Q = ""
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > workListMaxPageSize {
		f.PageSize = workListDefaultPageSize
	}
}

// workStats is the §6.3 statistic block (rates are nil when the
// denominator is 0).
type workStats struct {
	AcceptRate         *float64 `json:"accept_rate"`
	MedianReplySeconds *float64 `json:"median_reply_seconds"`
	FailureRate        *float64 `json:"failure_rate"`
}

// myStats is the executor's own tally on one task (§5.1);
// rejections_left is what remains of max_rejected_per_agent.
type myStats struct {
	Accepted       int64 `json:"accepted"`
	Rejected       int64 `json:"rejected"`
	RejectionsLeft int64 `json:"rejections_left"`
}

func statsView(s repository.TaskStats) workStats {
	rate := func(num, den int64) *float64 {
		if den == 0 {
			return nil
		}
		v := float64(num) / float64(den)
		return &v
	}
	return workStats{
		AcceptRate:         rate(s.Settled, s.Settled+s.Rejected),
		MedianReplySeconds: s.MedianReplySeconds,
		FailureRate:        rate(s.Failed, s.TerminalTotal),
	}
}

// myTally computes the executor's counts and the rejections left
// under the contract's cap (§5.3 step 5, §3 缺省 5).
func myTally(counts repository.AgentSubmissionCounts, contract task.Contract) myStats {
	left := rejectedCapFor(contract) - counts.Rejected
	if left < 0 {
		left = 0
	}
	return myStats{Accepted: counts.Settled, Rejected: counts.Rejected, RejectionsLeft: left}
}

// myTallyFor reads the agent's tallies for the task's CURRENT version
// contract (caps do not change per version for listing purposes).
func myTallyFor(ctx context.Context, pool *pg.Pool, agentID int64, t *repository.TaskRow, contract task.Contract) (myStats, error) {
	counts, err := repository.CountAgentSubmissions(ctx, pool, t.ID, agentID)
	if err != nil {
		return myStats{}, err
	}
	return myTally(counts, contract), nil
}

// ListWork is §5.1 work_list: open, claimable, not-own,
// not-cap-exhausted tasks, newest open first, ONE page of the filter
// with the total number of matching rows. All §5.1 filtering runs in
// SQL (WO-19 Q1 — no candidate window); the caller's three data
// classes beyond the page (agent counts, 30-day stats) stay batch
// queries — no per-task round trips.
func ListWork(ctx context.Context, pool *pg.Pool, agentID int64, now time.Time, filter WorkListFilter) ([]map[string]any, int64, error) {
	filter.Normalize()
	rows, total, err := repository.FindOpenWorkPage(ctx, pool, agentID,
		repository.WorkFilter{Keyword: filter.Q, Code: filter.Code},
		filter.PageSize, (filter.Page-1)*filter.PageSize)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if len(rows) == 0 {
		return []map[string]any{}, total, nil
	}

	type entry struct {
		row      repository.WorkCandidate
		contract task.Contract
		my       myStats
	}
	entries := make([]entry, 0, len(rows))
	taskIDs := make([]int64, len(rows))
	for i := range rows {
		taskIDs[i] = rows[i].Task.ID
		var contract task.Contract
		if err := json.Unmarshal(rows[i].Contract, &contract); err != nil {
			return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		entries = append(entries, entry{row: rows[i], contract: contract})
	}

	// Batch 1: this agent's submission tallies across the page (for
	// the my block — the cap itself was already enforced in SQL).
	countsByTask, err := repository.CountAgentSubmissionsBatch(ctx, pool, agentID, taskIDs)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	for i := range entries {
		counts := countsByTask[entries[i].row.Task.ID] // zero value when no submissions
		entries[i].my = myTally(counts, entries[i].contract)
	}

	// Batch 2: §6.3 statistics for the page.
	statsByTask, err := repository.GetTaskStatsBatch(ctx, pool, taskIDs, now.Add(-statsWindow))
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	out := make([]map[string]any, 0, len(entries))
	for _, k := range entries {
		stats := statsByTask[k.row.Task.ID] // zero value when no terminals
		requirements := []rune(k.contract.Requirements)
		if len(requirements) > workRequirementsExcerpt {
			requirements = requirements[:workRequirementsExcerpt]
		}
		out = append(out, map[string]any{
			"code":         k.row.Task.Code,
			"title":        k.contract.Title,
			"requirements": string(requirements),
			"price":        k.contract.Price,
			"slots":        slotsFor(&k.row.Task, k.contract),
			"claim":        map[string]any{"required": k.contract.Claim.Required},
			"stats":        statsView(stats),
			"my":           k.my,
		})
	}
	return out, total, nil
}

// workBoardRequirementsExcerpt is the homepage board excerpt (WO-19
// H1) — shorter than the §5.1 listing excerpt.
const workBoardRequirementsExcerpt = 140 // runes

// WorkBoardRow is one row of the anonymous homepage task board
// (WO-19 H1): no caller identity, no personal data.
type WorkBoardRow struct {
	Code         string `json:"code"`
	Title        string `json:"title"`
	Requirements string `json:"requirements"`
	Price        int64  `json:"price"`
	Slots        int64  `json:"slots"`
}

// ListWorkBoard is the anonymous homepage board: the SAME query as
// work_list (§5.1, WO-19 Q1) with agentID 0 — open status, slots >= 1,
// version join, keyword/code filters, paging — but no own-task or
// rejection-cap exclusion (an anonymous view excludes no account).
func ListWorkBoard(ctx context.Context, pool *pg.Pool, keyword, code string, page, pageSize int) ([]WorkBoardRow, int64, error) {
	f := WorkListFilter{Q: keyword, Code: code, Page: page, PageSize: pageSize}
	f.Normalize()
	rows, total, err := repository.FindOpenWorkPage(ctx, pool, 0,
		repository.WorkFilter{Keyword: f.Q, Code: f.Code},
		f.PageSize, (f.Page-1)*f.PageSize)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	out := make([]WorkBoardRow, 0, len(rows))
	for i := range rows {
		var contract task.Contract
		if err := json.Unmarshal(rows[i].Contract, &contract); err != nil {
			return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		requirements := []rune(contract.Requirements)
		if len(requirements) > workBoardRequirementsExcerpt {
			requirements = requirements[:workBoardRequirementsExcerpt]
		}
		out = append(out, WorkBoardRow{
			Code:         rows[i].Task.Code,
			Title:        contract.Title,
			Requirements: string(requirements),
			Price:        contract.Price,
			Slots:        slotsFor(&rows[i].Task, contract),
		})
	}
	return out, total, nil
}

// visibleTask loads a task with executor visibility.
func visibleTask(ctx context.Context, pool *pg.Pool, code string) (*repository.TaskRow, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return t, nil
}

// GetWork is §5.1 work_get: the task's full contract (minus
// receiver), the live harness directory, the 30-day stats and the
// caller's tallies.
func GetWork(ctx context.Context, pool *pg.Pool, agentID int64, code string, now time.Time) (map[string]any, error) {
	t, err := visibleTask(ctx, pool, code)
	if err != nil {
		return nil, err
	}
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}

	// §10.8: never expose the receiver — strip the key from the
	// contract projection entirely
	var contractProjection map[string]any
	if err := json.Unmarshal(t.Contract, &contractProjection); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	delete(contractProjection, "receiver")

	stats, err := repository.GetTaskStats(ctx, pool, t.ID, now.Add(-statsWindow))
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	my, err := myTallyFor(ctx, pool, agentID, t, contract)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	// M3: the harness directory is queried LIVE from the publisher's
	// current memories, in the contract's harness_refs order
	directory, dirErr := liveHarnessDirectory(ctx, pool, t.PublisherID, contract.HarnessRefs)
	if dirErr != nil {
		return nil, dirErr
	}

	view := map[string]any{
		"code":     t.Code,
		"status":   t.Status,
		"contract": contractProjection,
		"harness":  directory,
		"stats":    statsView(stats),
		"my":       my,
	}
	if t.PausedReason != nil {
		view["paused_reason"] = *t.PausedReason
	}
	if t.ClosedReason != nil {
		view["closed_reason"] = *t.ClosedReason
	}
	return view, nil
}

// liveHarnessDirectory queries the publisher's memories NOW (M3) and
// projects them in the contract's harness_refs order.
func liveHarnessDirectory(ctx context.Context, pool *pg.Pool, publisherID int64, refs []string) ([]map[string]any, error) {
	directory := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		k, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, publisherID, ref)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if k == nil {
			continue // deleted memories are absent from the directory
		}
		entry := map[string]any{
			"ref_id": k.Code,
			"title":  k.Title,
			"bytes":  len(k.Content),
		}
		if k.Description != nil {
			entry["description"] = *k.Description
		}
		directory = append(directory, entry)
	}
	return directory, nil
}

// contractJSONOf marshals a contract for projection.
func contractJSONOf(c task.Contract) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// GetHarness is §5.1 work_harness: one memory's CURRENT content (M3).
// The ref must be in the contract's harness_refs and the memory must
// still exist; anything else is HARNESS_REF_NOT_FOUND.
func GetHarness(ctx context.Context, pool *pg.Pool, agentID int64, code, refID string) (map[string]any, error) {
	t, err := visibleTask(ctx, pool, code)
	if err != nil {
		return nil, err
	}
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	// the ref must be part of this task's harness_refs
	found := false
	for _, ref := range contract.HarnessRefs {
		if ref == refID {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New(0, "HARNESS_REF_NOT_FOUND",
			fmt.Sprintf("ref %q is not part of this task", refID))
	}
	// read the publisher's CURRENT memory content
	k, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, t.PublisherID, refID)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if k == nil {
		return nil, errors.New(0, "HARNESS_REF_NOT_FOUND",
			fmt.Sprintf("ref %q is not part of this task", refID))
	}
	return map[string]any{
		"ref_id":  k.Code,
		"title":   k.Title,
		"content": k.Content,
	}, nil
}

// eventView is one SubmissionEvent in the executor's status output.
type eventView struct {
	Seq   int32     `json:"seq"`
	From  *string   `json:"from"`
	To    string    `json:"to"`
	Cause string    `json:"cause"`
	At    time.Time `json:"at"`
}

// GetSubmissionStatus is §8.1 work_status: by submission id, or by
// (task code, request_key). Only the executor's own submissions are
// visible (§8.4 SUBMISSION_NOT_FOUND otherwise).
func GetSubmissionStatus(ctx context.Context, pool *pg.Pool, agentID int64, submissionID *int64, code, requestKey string) (map[string]any, error) {
	var sub *repository.SubmissionRow
	var err error
	switch {
	case submissionID != nil:
		sub, err = repository.FindSubmissionByID(ctx, pool, *submissionID)
		if goerrors.Is(err, pgx.ErrNoRows) || sub == nil {
			return nil, errors.New(0, "SUBMISSION_NOT_FOUND", "Submission not found")
		}
	case code != "" && requestKey != "":
		t, terr := repository.FindTaskByCode(ctx, pool, code)
		if goerrors.Is(terr, pgx.ErrNoRows) || t == nil {
			return nil, errors.New(0, "SUBMISSION_NOT_FOUND", "Submission not found")
		}
		if terr != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		sub, err = repository.FindSubmissionByIdentity(ctx, pool, t.ID, agentID, requestKey)
		if goerrors.Is(err, pgx.ErrNoRows) || sub == nil {
			return nil, errors.New(0, "SUBMISSION_NOT_FOUND", "Submission not found")
		}
	default:
		return nil, errors.NewWithDetails(0, "VALIDATION_FAILED", "Give submission_id or code + request_key",
			map[string]any{"errors": []map[string]string{{"field": "submission_id", "message": "submission_id or code + request_key is required"}}})
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if sub.AgentID != agentID {
		return nil, errors.New(0, "SUBMISSION_NOT_FOUND", "Submission not found")
	}
	t, err := repository.FindTaskByID(ctx, pool, sub.TaskID)
	if err != nil || t == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	events, err := repository.ListSubmissionEvents(ctx, pool, sub.SubmissionID)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	views := make([]eventView, 0, len(events))
	for _, e := range events {
		views = append(views, eventView{Seq: e.Seq, From: e.FromState, To: e.ToState, Cause: e.Cause, At: e.At})
	}
	view := newSubmissionView(sub, t.Code)
	out := map[string]any{}
	raw, _ := json.Marshal(view)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	out["events"] = views
	return out, nil
}

// ListHistory is §8.1 work_history: the agent's submissions, newest
// first, optionally filtered by task.
func ListHistory(ctx context.Context, pool *pg.Pool, agentID int64, code string, page, pageSize int) ([]SubmissionView, int64, error) {
	var taskID *int64
	if code != "" {
		t, err := repository.FindTaskByCode(ctx, pool, code)
		if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
			return nil, 0, errors.New(0, "TASK_NOT_FOUND", "Task not found")
		}
		if err != nil {
			return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		taskID = &t.ID
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	if page < 1 {
		page = 1
	}
	total, err := repository.CountAgentSubmissionsBy(ctx, pool, agentID, taskID)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	rows, err := repository.ListAgentSubmissions(ctx, pool, agentID, taskID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	codes := map[int64]string{}
	for i := range rows {
		if _, ok := codes[rows[i].TaskID]; !ok {
			if t, err := repository.FindTaskByID(ctx, pool, rows[i].TaskID); err == nil && t != nil {
				codes[rows[i].TaskID] = t.Code
			}
		}
	}
	out := make([]SubmissionView, 0, len(rows))
	for i := range rows {
		out = append(out, newSubmissionView(&rows[i], codes[rows[i].TaskID]))
	}
	return out, total, nil
}

// RejectionsLeft is how many more rejections the agent may collect on
// the task (§5.3 step 5) under its current version's cap — 0 means a
// rejected submission ends the agent's work on this task (§8.3 stop).
func RejectionsLeft(ctx context.Context, pool *pg.Pool, agentID int64, code string) (int64, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || t == nil {
		return 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	contract, err := effectiveContract(ctx, pool, t)
	if err != nil {
		return 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	my, err := myTallyFor(ctx, pool, agentID, t, contract)
	if err != nil {
		return 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return my.RejectionsLeft, nil
}
