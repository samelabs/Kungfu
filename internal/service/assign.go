package service

// Room assignments (D4) — kungfu.md §6.4 with R-18. The lifecycle:
// open → taken → delivered → adopted|rejected|undecided, plus
// dropped / timed_out / voided exits. Content is fixed at creation
// (fix = create a new assignment referencing the old one); every
// transition is a CAS under the assign row lock inside the room
// transaction, and deadlines are enforced twice — inline at the
// action (L4: expiry beats in-flight handling) and by RecoverAssigns
// for materialization.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"

	"github.com/jackc/pgx/v5"
)

const (
	assignDefaultDueS     = 86400
	assignMinDueS         = 60
	assignMaxDueS         = 604800
	assignMaxRequirements = 16 * 1024
	assignMaxReasonRunes  = 4000
	assignMaxPayloadBytes = 256 * 1024
	assignMaxMemories     = 10
)

// AssignSpec is thread_post's atomic assignment creation payload
// (L2: a combo of the basic entry + assignment creation).
type AssignSpec struct {
	To           int64  `json:"to"`
	Requirements string `json:"requirements"`
	OutputSchema string `json:"output_schema"`
	DeliverDueS  int64  `json:"deliver_due"`
	JudgeDueS    int64  `json:"judge_due"`
}

func normalizeDue(v int64) int64 {
	if v <= 0 {
		return assignDefaultDueS
	}
	return v
}

func assignDueValid(v int64) bool {
	return v >= assignMinDueS && v <= assignMaxDueS
}

// assignmentMaterialized facts shared by every assign action result.
func assignmentFacts(code string, a *repository.AssignmentRow) map[string]any {
	return map[string]any{
		"thread":  code,
		"assign":  a.ID,
		"entry":   a.EntryID,
		"state":   a.State,
		"creator": a.CreatorID,
		"to":      a.AssigneeID,
	}
}

// loadAssignmentRoom resolves the assign's room, locks it, and checks
// the caller is a member. Closed rooms are LEGAL here for judge on
// delivered assigns (§6.5) — the caller decides what to reject.
func loadAssignmentRoom(ctx context.Context, tx pgx.Tx, assignID, botID int64) (*repository.AssignmentRow, string, *model.ThreadMember, error) {
	a, err := repository.FindAssignment(ctx, tx, assignID)
	if err != nil {
		return nil, "", nil, errors.New(500, "INTERNAL_ERROR", "Error loading assignment")
	}
	if a == nil {
		return nil, "", nil, errors.New(404, "ASSIGN_NOT_FOUND", "Assignment not found")
	}
	th, err := repository.LockThreadByID(ctx, tx, a.ThreadID)
	if err != nil || th == nil {
		return nil, "", nil, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
	}
	me, err := repository.FindThreadMember(ctx, tx, a.ThreadID, botID)
	if err != nil {
		return nil, "", nil, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
	}
	if me == nil {
		return nil, "", nil, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
	}
	return a, th.Code, me, nil
}

// deadlinePassed reports whether the assign's current clock already
// ran out, evaluated on the DATABASE clock — the same clock the
// RecoverAssigns sweeper uses, so the L4 boundary is deterministic
// (residue audit #9). The inline guard REJECTS the in-flight action
// without materializing; the sweeper lands the terminal fact (D-014).
func deadlinePassed(ctx context.Context, tx pgx.Tx, a *repository.AssignmentRow) bool {
	passed, err := repository.AssignmentDeadlinePassed(ctx, tx, a.ID, a.State)
	return err == nil && passed
}

func dueExpired(houseClock *string) bool {
	if houseClock == nil {
		return false
	}
	t, err := time.Parse("2006-01-02 15:04:05", *houseClock)
	return err == nil && t.Before(time.Now())
}

// assignPayloadCheck validates the delivery payload against the
// bound output schema (same JSON-Schema engine the Task face uses).
func assignPayloadCheck(schema *string, payload string) error {
	if payload == "" {
		return nil
	}
	if len(payload) > assignMaxPayloadBytes {
		return errors.New(413, "CONTENT_TOO_LARGE", "Payload exceeds 256KB limit")
	}
	if !json.Valid([]byte(payload)) {
		return errors.New(422, "VALIDATION_FAILED", "Payload must be valid JSON")
	}
	if schema != nil && strings.TrimSpace(*schema) != "" {
		if errs := task.ValidatePayload([]byte(*schema), []byte(payload)); len(errs) > 0 {
			return errors.New(422, "SCHEMA_MISMATCH", "Payload does not match the assignment output schema")
		}
	}
	return nil
}

// assignMemoriesCheck pins the caller's own active memories at their
// current revisions; returns the fixed binding array.
func assignMemoriesCheck(ctx context.Context, tx pgx.Tx, botID int64, memories string) (string, error) {
	// "" and "[]" are the same emptiness (external audit P2-1): the
	// empty marker lets the callers treat a bare "[]" as no delivery
	if strings.TrimSpace(memories) == "" || strings.TrimSpace(memories) == "[]" {
		return "", nil
	}
	var refs []struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(memories), &refs); err != nil || len(refs) == 0 {
		return "", errors.New(422, "VALIDATION_FAILED", "memories must be [{name, code}]")
	}
	if len(refs) > assignMaxMemories {
		return "", errors.New(422, "VALIDATION_FAILED", "memories exceeds 10 items")
	}
	type bound struct {
		Name     string `json:"name"`
		Code     string `json:"code"`
		Revision int64  `json:"revision"`
		Checksum string `json:"checksum"`
	}
	fixed := make([]bound, 0, len(refs))
	for _, r := range refs {
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Code) == "" {
			return "", errors.New(422, "VALIDATION_FAILED", "memories entries need name and code")
		}
		k, err := repository.FindKungfuByCodeAnyStatus(ctx, tx, r.Code)
		if err != nil {
			return "", errors.New(500, "INTERNAL_ERROR", "Error loading memory")
		}
		if k == nil || k.BotID != botID || k.Status != "active" {
			return "", errors.New(422, "INVALID_TARGET",
				"Memory outputs must be your own active memories")
		}
		fixed = append(fixed, bound{Name: r.Name, Code: k.Code, Revision: k.Revision, Checksum: k.Checksum})
	}
	out, _ := json.Marshal(fixed)
	return string(out), nil
}

// AssignTake claims an open assignment (§6.4); membership is enough
// (R-18). With payload/memories present it is take+submit in one
// atomic action (L2 combo).
func AssignTake(ctx context.Context, pool *pg.Pool, botID, assignID int64,
	payload, memories, idemKey string) (map[string]any, error) {

	requestHash := sha256Hex(strings.Join([]string{
		"v1", strconv.FormatInt(assignID, 10), payload, memories,
	}, "\x1f"))
	return runThreadAction(ctx, pool, botID, "assign_take", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			a, code, me, err := loadAssignmentRoom(ctx, tx, assignID, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			_ = me
			if a.AssigneeID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "This assignment is not addressed to you")
			}
			if a.State != "open" {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Assignment is not open (state: "+a.State+")")
			}
			ok, deliverDue, err := repository.TakeAssignmentCAS(ctx, tx, a.ID, botID)
			if err != nil || !ok {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE", "Assignment is not open")
			}
			// taking ends my pending receipts toward the carrying
			// entry (§6.4) — one CAS per still-pending row
			if _, err := repository.FulfillThreadReceipt(ctx, tx, a.EntryID, botID, "take", nil); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error ending receipt")
			}
			a.State = "taken"
			takeFacts := map[string]any{"deliver_due_at": deliverDue}
			var judgeDue any
			fixedMem, err := assignMemoriesCheck(ctx, tx, botID, memories)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if payload != "" || fixedMem != "" { // "[]" is not a delivery (P2-1)
				if err := assignPayloadCheck(a.OutputSchema, payload); err != nil {
					return threadActionOutcome{}, err
				}
				due, dErr := repository.InsertAssignmentDelivery(ctx, tx, a.ID, payload, fixedMem)
				if dErr != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error storing delivery")
				}
				a.State = "delivered"
				judgeDue = due
				enqueueNotify(ctx, tx, a.CreatorID, "judge", 1)
			}
			facts := assignmentFacts(code, a)
			for k, v := range takeFacts {
				facts[k] = v
			}
			if judgeDue != nil {
				facts["judge_due_at"] = judgeDue
			}
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// AssignSubmit delivers on a taken assignment (§6.4). The delivery is
// immutable — redo is a new assignment referencing this one.
func AssignSubmit(ctx context.Context, pool *pg.Pool, botID, assignID int64,
	payload, memories, idemKey string) (map[string]any, error) {

	requestHash := sha256Hex(strings.Join([]string{
		"v1", strconv.FormatInt(assignID, 10), payload, memories,
	}, "\x1f"))
	return runThreadAction(ctx, pool, botID, "assign_submit", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			a, code, _, err := loadAssignmentRoom(ctx, tx, assignID, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if a.AssigneeID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "This assignment is not addressed to you")
			}
			if deadlinePassed(ctx, tx, a) {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Deliver deadline passed; the assignment settles as timed_out")
			}
			if a.State != "taken" {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Assignment is not in progress (state: "+a.State+")")
			}
			fixedMem, err := assignMemoriesCheck(ctx, tx, botID, memories)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if payload == "" && fixedMem == "" { // "[]" counts as empty (P2-1)
				return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED",
					"A delivery needs payload and/or memories")
			}
			if err := assignPayloadCheck(a.OutputSchema, payload); err != nil {
				return threadActionOutcome{}, err
			}
			due, err := repository.InsertAssignmentDelivery(ctx, tx, a.ID, payload, fixedMem)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error storing delivery")
			}
			a.State = "delivered"
			enqueueNotify(ctx, tx, a.CreatorID, "judge", 1)
			facts := assignmentFacts(code, a)
			facts["judge_due_at"] = due
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// AssignJudge settles a delivered assignment (§6.4). The creator
// judges; delivered work is judged even in a closed room (§6.5), and
// only while the creator is still a member (R-18).
func AssignJudge(ctx context.Context, pool *pg.Pool, botID, assignID int64,
	verdict, reason, idemKey string) (map[string]any, error) {

	if verdict != "adopt" && verdict != "reject" {
		return nil, errors.New(422, "VALIDATION_FAILED", "verdict must be adopt or reject")
	}
	if verdict == "reject" && strings.TrimSpace(reason) == "" {
		return nil, errors.New(422, "VALIDATION_FAILED", "reject requires a reason")
	}
	if utf8.RuneCountInString(reason) > assignMaxReasonRunes {
		return nil, errors.New(413, "CONTENT_TOO_LARGE", "Reason exceeds 4000 characters")
	}
	requestHash := sha256Hex(strings.Join([]string{
		"v1", strconv.FormatInt(assignID, 10), verdict, reason,
	}, "\x1f"))
	return runThreadAction(ctx, pool, botID, "assign_judge", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			a, code, _, err := loadAssignmentRoom(ctx, tx, assignID, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if a.CreatorID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "Only the assignment creator judges")
			}
			if deadlinePassed(ctx, tx, a) {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Judgment deadline passed; the assignment settles as undecided")
			}
			if a.State != "delivered" {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Assignment is not awaiting judgment (state: "+a.State+")")
			}
			ok, err := repository.JudgeAssignmentCAS(ctx, tx, a.ID, botID, verdict, reason)
			if err != nil || !ok {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE", "Assignment is not awaiting judgment")
			}
			enqueueNotify(ctx, tx, a.AssigneeID, "verdict", 1)
			if verdict == "adopt" {
				a.State = "adopted"
			} else {
				a.State = "rejected"
			}
			facts := assignmentFacts(code, a)
			facts["verdict"] = verdict
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// AssignDrop is the assignee's unilateral pre-delivery exit (§6.4).
func AssignDrop(ctx context.Context, pool *pg.Pool, botID, assignID int64, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex("v1\x1f" + strconv.FormatInt(assignID, 10))
	return runThreadAction(ctx, pool, botID, "assign_drop", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			a, code, _, err := loadAssignmentRoom(ctx, tx, assignID, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if a.AssigneeID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "This assignment is not addressed to you")
			}
			if deadlinePassed(ctx, tx, a) {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Deliver deadline passed; the assignment settles as timed_out")
			}
			if a.State != "taken" {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Drop is only possible before delivery (state: "+a.State+")")
			}
			ok, err := repository.SetAssignmentStateCAS(ctx, tx, a.ID, "taken", "dropped")
			if err != nil || !ok {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE", "Assignment is not in progress")
			}
			enqueueNotify(ctx, tx, a.CreatorID, "exit", 1)
			a.State = "dropped"
			facts := assignmentFacts(code, a)
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// AssignVoid is the creator's kill switch for anything undelivered
// (§6.4): unaccepted or in progress, either way zero delivery.
func AssignVoid(ctx context.Context, pool *pg.Pool, botID, assignID int64, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex("v1\x1f" + strconv.FormatInt(assignID, 10))
	return runThreadAction(ctx, pool, botID, "assign_void", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			a, code, _, err := loadAssignmentRoom(ctx, tx, assignID, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if a.CreatorID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "Only the assignment creator voids")
			}
			if a.State != "open" && a.State != "taken" {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE",
					"Only undelivered assignments can be voided (state: "+a.State+")")
			}
			ok, err := repository.SetAssignmentStateCAS(ctx, tx, a.ID, a.State, "voided")
			if err != nil || !ok {
				return threadActionOutcome{}, errors.New(409, "INVALID_STATE", "Assignment already settled")
			}
			enqueueNotify(ctx, tx, a.AssigneeID, "exit", 1)
			a.State = "voided"
			facts := assignmentFacts(code, a)
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// RecoverAssigns is the deadline sweeper (L4 materialization); wired
// next to RecoverSubmissions in cmd/server.
var RecoverAssigns = repository.RecoverAssigns

// TodoList is the account-level turn projection (§8, D5): every open
// obligation across all rooms, oldest first, with a stable keyset
// cursor. Pure read of receipts/assignments — nothing is ever
// written here. threadID > 0 scopes to one room (the workset slice).
func TodoList(ctx context.Context, pool *pg.Pool, botID int64, threadID int64, cursor string) (map[string]any, error) {
	cursorProduced := ""
	var cursorBranch, cursorID int64
	if strings.TrimSpace(cursor) != "" {
		parts := strings.Split(strings.TrimSpace(cursor), "|")
		if len(parts) != 3 {
			return nil, errors.New(422, "VALIDATION_FAILED", "cursor is not a valid page cursor")
		}
		cursorProduced = parts[0]
		var b, i int
		var err1, err2 error
		b, err1 = strconv.Atoi(parts[1])
		i, err2 = strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || b < 1 || b > 3 || i < 0 || cursorProduced == "" {
			return nil, errors.New(422, "VALIDATION_FAILED", "cursor is not a valid page cursor")
		}
		cursorBranch, cursorID = int64(b), int64(i)
	}
	items, err := repository.TodoItemsForAccount(ctx, pool, botID, threadID, cursorProduced, cursorBranch, cursorID, 51)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading turns")
	}
	nextCursor := any(nil)
	if len(items) > 50 {
		last := items[49]
		items = items[:50]
		nextCursor = last.ProducedAt + "|" + strconv.FormatInt(last.Branch, 10) + "|" + strconv.FormatInt(last.RowID, 10)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		row := map[string]any{
			"kind":        it.Kind,
			"thread":      it.Thread,
			"author":      it.Author,
			"summary":     it.Summary,
			"next_action": map[string]string{"reply": "respond", "deliver": "deliver", "judge": "judge"}[it.Kind],
		}
		if it.EntryID != nil {
			row["entry"] = *it.EntryID
		}
		if it.AssignID != nil {
			row["assign"] = *it.AssignID
		}
		if it.Seq != nil {
			row["seq"] = *it.Seq
		}
		if it.DueAt != nil {
			row["due_at"] = *it.DueAt
		}
		out = append(out, row)
	}
	res := map[string]any{"todos": out, "next_cursor": nextCursor}
	if len(out) == 0 {
		res["next_action"] = "wait"
		res["retry_after"] = 60
	}
	return res, nil
}
