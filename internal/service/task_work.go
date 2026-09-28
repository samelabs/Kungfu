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
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// §5.1/§6.3 presentation bounds.
const (
	workListMax             = 100
	workRequirementsExcerpt = 280 // runes
	statsWindow             = 30 * 24 * time.Hour
	listOpenWorkingSet      = 500 // candidate rows before slot/limit filtering
)

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

// ListWork is §5.1 work_list: open, claimable, not-own, not-exhausted
// tasks, newest open first, at most 100.
func ListWork(ctx context.Context, pool *pg.Pool, agentID int64, now time.Time) ([]map[string]any, error) {
	rows, err := repository.ListOpenTasksForAgent(ctx, pool, agentID, listOpenWorkingSet)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	since := now.Add(-statsWindow)
	out := make([]map[string]any, 0, workListMax)
	for i := range rows {
		if len(out) == workListMax {
			break
		}
		t := &rows[i]
		contract, err := effectiveContract(ctx, pool, t)
		if goerrors.Is(err, pgx.ErrNoRows) {
			// an open row without a version snapshot is not listable
			// (only reachable from directly-seeded test rows; opens
			// always write the snapshot first)
			continue
		}
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if slotsFor(t, contract) < 1 {
			continue
		}
		counts, err := repository.CountAgentSubmissions(ctx, pool, t.ID, agentID)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		my := myTally(counts, contract)
		if my.RejectionsLeft <= 0 {
			continue // executor exhausted this task's rejection cap
		}
		stats, err := repository.GetTaskStats(ctx, pool, t.ID, since)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		requirements := []rune(contract.Requirements)
		if len(requirements) > workRequirementsExcerpt {
			requirements = requirements[:workRequirementsExcerpt]
		}
		out = append(out, map[string]any{
			"code":         t.Code,
			"title":        contract.Title,
			"requirements": string(requirements),
			"price":        contract.Price,
			"slots":        slotsFor(t, contract),
			"claim":        map[string]any{"required": contract.Claim.Required},
			"stats":        statsView(stats),
			"my":           my,
		})
	}
	return out, nil
}

// agentVersion resolves the version the agent sees: their active
// claim's version if any, else the task's current version (§10.7).
func agentVersion(ctx context.Context, pool *pg.Pool, agentID int64, t *repository.TaskRow) (int32, error) {
	if claim, err := repository.FindActiveClaimByTaskAgent(ctx, pool, t.ID, agentID); err == nil && claim != nil {
		return claim.Version, nil
	} else if err != nil && !goerrors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	return t.Version, nil
}

// harnessEntry is one item of the version snapshot.
type harnessEntry struct {
	RefID   string `json:"ref_id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

func loadVersion(ctx context.Context, pool *pg.Pool, taskID int64, version int32) (*repository.TaskVersionRow, task.Contract, []harnessEntry, error) {
	v, err := repository.FindTaskVersion(ctx, pool, taskID, version)
	if goerrors.Is(err, pgx.ErrNoRows) || v == nil {
		return nil, task.Contract{}, nil, errors.New(0, "INTERNAL_ERROR", "Version snapshot missing")
	}
	if err != nil {
		return nil, task.Contract{}, nil, err
	}
	var contract task.Contract
	if err := json.Unmarshal(v.Contract, &contract); err != nil {
		return nil, task.Contract{}, nil, err
	}
	var harness []harnessEntry
	if err := json.Unmarshal(v.Harness, &harness); err != nil {
		return nil, task.Contract{}, nil, err
	}
	return v, contract, harness, nil
}

// visibleTask loads a task with executor visibility: draft is
// TASK_NOT_FOUND (§5.1 amendment).
func visibleTask(ctx context.Context, pool *pg.Pool, code string) (*repository.TaskRow, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) || t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t.Status == task.TaskDraft {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	return t, nil
}

// GetWork is §5.1 work_get: the full contract (receiver removed —
// §10.8) of the version the agent is bound to, its harness directory,
// stats and personal tally.
func GetWork(ctx context.Context, pool *pg.Pool, agentID int64, code string, now time.Time) (map[string]any, error) {
	t, err := visibleTask(ctx, pool, code)
	if err != nil {
		return nil, err
	}
	version, err := agentVersion(ctx, pool, agentID, t)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	_, contract, harness, err := loadVersion(ctx, pool, t.ID, version)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	// §10.8: never expose the receiver — strip the key from the
	// contract projection entirely
	var contractProjection map[string]any
	if err := json.Unmarshal(contractJSONOf(contract), &contractProjection); err != nil {
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

	directory := make([]map[string]any, 0, len(harness))
	for _, h := range harness {
		directory = append(directory, map[string]any{
			"ref_id": h.RefID,
			"title":  h.Title,
			"bytes":  len(h.Content),
		})
	}
	return map[string]any{
		"code":     t.Code,
		"status":   t.Status,
		"version":  version,
		"contract": contractProjection,
		"harness":  directory,
		"stats":    statsView(stats),
		"my":       my,
	}, nil
}

// contractJSONOf marshals a contract for projection.
func contractJSONOf(c task.Contract) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// GetHarness is §5.1 work_harness: one snapshot entry's content.
func GetHarness(ctx context.Context, pool *pg.Pool, agentID int64, code, refID string) (map[string]any, error) {
	t, err := visibleTask(ctx, pool, code)
	if err != nil {
		return nil, err
	}
	version, err := agentVersion(ctx, pool, agentID, t)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	_, _, harness, err := loadVersion(ctx, pool, t.ID, version)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	for _, h := range harness {
		if h.RefID == refID {
			return map[string]any{
				"ref_id":  h.RefID,
				"title":   h.Title,
				"content": h.Content,
			}, nil
		}
	}
	return nil, errors.New(0, "HARNESS_REF_NOT_FOUND",
		fmt.Sprintf("ref %q is not part of this task version", refID))
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
