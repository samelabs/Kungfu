package service

// Thread speaking state (D3) — kungfu.md §6.3: entries, the response
// object rules, handle/retract, and the receipts they create and end.
// All writes go through runThreadAction (L3 replay snapshots) inside
// one transaction that first locks the thread row (L5 serialization
// against joins, key changes and closes).

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"

	"github.com/jackc/pgx/v5"
)

const (
	threadEntryMaxContentBytes = 100 * 1024
	threadEntryMaxSummaryRunes = 500
	threadEntryMaxAsk          = 50
	threadHandleMaxNoteRunes   = 1000
)

func speechRight(role string) bool {
	return role == repository.ThreadRoleGovernor || role == repository.ThreadRoleSpeaker
}

// ThreadPost is the single speaking action (§6.3): one entry, its
// response objects computed once by the ordered rules and frozen,
// and — when it replies — the CAS that ends the speaker's own pending
// receipt toward the replied entry in the same transaction.
func ThreadPost(ctx context.Context, pool *pg.Pool, botID int64, threadCode string,
	content, memoryCode, summary string, replyTo *int64, ask []int64, assign *AssignSpec, idemKey string) (map[string]any, error) {

	// The replay identity covers every semantic input; ask presence
	// (nil vs explicit empty) is part of it — ask:[] is "notify only",
	// absent ask falls through to the default rules.
	askForHash := []int64{}
	if ask != nil {
		askForHash = append([]int64(nil), ask...)
		sort.Slice(askForHash, func(i, j int) bool { return askForHash[i] < askForHash[j] })
	}
	askMarker := "absent"
	if ask != nil {
		askMarker = "present"
	}
	var replyMarker string
	if replyTo != nil {
		replyMarker = numStr(*replyTo)
	}
	assignForHash := "none"
	if assign != nil {
		assignForHash = strings.Join([]string{
			strconv.FormatInt(assign.To, 10), assign.Requirements, assign.OutputSchema,
			strconv.FormatInt(assign.DeliverDueS, 10), strconv.FormatInt(assign.JudgeDueS, 10),
		}, ",")
	}
	requestHash := sha256Hex(strings.Join([]string{
		"v1", threadCode, content, memoryCode, summary, replyMarker, askMarker,
		numsJoin(askForHash), assignForHash,
	}, "\x1f"))

	return runThreadAction(ctx, pool, botID, "thread_post", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, threadCode)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
			}
			if th == nil {
				return threadActionOutcome{}, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
			}
			if th.Status != repository.ThreadStatusOpen {
				return threadActionOutcome{}, errors.New(409, "THREAD_CLOSED", "Thread is closed")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
			}
			if me == nil {
				return threadActionOutcome{}, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
			}
			if !speechRight(me.Role) {
				return threadActionOutcome{}, errors.New(403, "READ_ONLY", "Observers cannot post")
			}

			if (content == "") == (memoryCode == "") {
				return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED",
					"Provide exactly one of content or memory")
			}

			// -- resolve the pinned memory version (§5/§9) --
			var memoryID, memoryRevision int64
			var memoryRef string
			if content != "" {
				trimmed := strings.TrimSpace(content)
				if trimmed == "" {
					return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED", "content is empty")
				}
				if len(content) > threadEntryMaxContentBytes {
					return threadActionOutcome{}, errors.New(413, "CONTENT_TOO_LARGE", "Content exceeds 100KB limit")
				}
				if utf8.RuneCountInString(content) > threadEntryMaxSummaryRunes && strings.TrimSpace(summary) == "" {
					return threadActionOutcome{}, errors.New(422, "SUMMARY_REQUIRED",
						"Content over 500 characters requires a summary")
				}
				if utf8.RuneCountInString(summary) > threadEntryMaxSummaryRunes {
					return threadActionOutcome{}, errors.New(413, "CONTENT_TOO_LARGE", "Summary exceeds 500 characters")
				}
				if security.ContainsCredentialValue(map[string]interface{}{
					"content": content, "summary": summary,
				}) {
					return threadActionOutcome{}, errors.New(400, "SENSITIVE_CONTENT",
						"Entry content must not contain credential-shaped strings")
				}
				id, code, err := repository.CreateThreadKungfu(ctx, tx, botID, "", summary, content, sha256Hex(content))
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error storing entry memory")
				}
				memoryID, memoryRevision, memoryRef = id, 1, code
			} else {
				k, err := repository.FindKungfuByCodeAnyStatus(ctx, tx, memoryCode)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading memory")
				}
				if k == nil {
					return threadActionOutcome{}, errors.New(404, "NOT_FOUND", "Memory not found")
				}
				if k.BotID == botID {
					if k.Status != "active" {
						return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
							"Own memory must be active to be referenced")
					}
				} else if k.Status != "active" || k.Visibility != "public" {
					// others' pins ride on publicness (§5): no
					// existence leak for what is not readable
					return threadActionOutcome{}, errors.New(404, "NOT_FOUND", "Memory not found")
				}
				memoryID, memoryRevision, memoryRef = k.ID, k.Revision, k.Code
			}

			// -- reply target lives in this thread or nowhere --
			var repliedAuthor *int64
			if replyTo != nil {
				target, err := repository.FindThreadEntry(ctx, tx, th.ID, *replyTo)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading entry")
				}
				if target == nil {
					return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
						"reply_to must reference an entry of this thread")
				}
				repliedAuthor = &target.AuthorID
			}

			// -- response objects, the ordered rules of §6.3 --
			var asked []int64
			switch {
			case ask != nil:
				if len(ask) > threadEntryMaxAsk {
					return threadActionOutcome{}, errors.New(422, "INVALID_TARGET", "ask exceeds 50 members")
				}
				seen := map[int64]bool{}
				for _, id := range ask {
					if id == botID {
						return threadActionOutcome{}, errors.New(422, "INVALID_TARGET", "ask must not include the author")
					}
					if seen[id] {
						continue
					}
					m, err := repository.FindThreadMember(ctx, tx, th.ID, id)
					if err != nil {
						return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading member")
					}
					if m == nil || !speechRight(m.Role) {
						return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
							"ask entries must be speech-capable members of this thread")
					}
					seen[id] = true
					asked = append(asked, id)
				}
			case repliedAuthor != nil && *repliedAuthor != botID:
				m, err := repository.FindThreadMember(ctx, tx, th.ID, *repliedAuthor)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading member")
				}
				// lost speech right ⇒ nobody, no fallback to later rules
				if m != nil && speechRight(m.Role) {
					asked = []int64{*repliedAuthor}
				}
			default:
				n, err := repository.CountThreadSpeechCapable(ctx, tx, th.ID)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error counting members")
				}
				if n == 2 {
					rows, err := repository.ListThreadMembers(ctx, tx, th.ID)
					if err != nil {
						return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading members")
					}
					for _, r := range rows {
						if r.AccountID != botID && speechRight(r.Role) {
							asked = []int64{r.AccountID}
							break
						}
					}
				}
			}
			if asked == nil {
				asked = []int64{}
			}

			// -- one entry, seq under the room lock --
			seq, err := repository.NextThreadSeq(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error allocating seq")
			}
			askedJSON, _ := json.Marshal(asked)
			entryID, err := repository.InsertThreadEntry(ctx, tx, th.ID, seq, botID,
				memoryID, memoryRevision, replyTo, string(askedJSON), summary)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error storing entry")
			}
			for _, id := range asked {
				if err := repository.InsertThreadReceipt(ctx, tx, th.ID, entryID, id); err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error creating receipts")
				}
			}
			for _, id := range asked {
				enqueueNotify(ctx, tx, id, "reply", 1)
			}

			// atomic assignment creation rides the same entry (§6.4,
			// L2): content fixed here, deadlines normalized, the
			// assignee must hold speech right at this moment.
			var assignID int64
			if assign != nil {
				if strings.TrimSpace(assign.Requirements) == "" ||
					len(assign.Requirements) > assignMaxRequirements {
					return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED",
						"assign.requirements is required (max 16KB)")
				}
				if assign.To == botID {
					// self-assignment is legal (§6.4); the author
					// already holds speech right here
				} else {
					m, err := repository.FindThreadMember(ctx, tx, th.ID, assign.To)
					if err != nil {
						return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading member")
					}
					if m == nil || !speechRight(m.Role) {
						return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
							"assign.to must be a speech-capable member of this thread")
					}
				}
				assign.DeliverDueS = normalizeDue(assign.DeliverDueS)
				assign.JudgeDueS = normalizeDue(assign.JudgeDueS)
				if !assignDueValid(assign.DeliverDueS) || !assignDueValid(assign.JudgeDueS) {
					return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED",
						"assign deadlines must be 60..604800 seconds")
				}
				if assign.OutputSchema != "" && !json.Valid([]byte(assign.OutputSchema)) {
					return threadActionOutcome{}, errors.New(422, "VALIDATION_FAILED",
						"assign.output_schema must be valid JSON")
				}
				assignID, err = repository.InsertAssignment(ctx, tx, th.ID, entryID, botID,
					assign.To, assign.Requirements, assign.OutputSchema,
					assign.DeliverDueS, assign.JudgeDueS)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error storing assignment")
				}
				if err := repository.LinkEntryAssignment(ctx, tx, entryID, assignID); err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error linking assignment")
				}
			}

			// replying ends my own pending toward the target — even
			// without one, the reply is legal (§6.3)
			fulfilledReply := false
			if replyTo != nil {
				fulfilledReply, err = repository.FulfillThreadReceipt(ctx, tx, *replyTo, botID, "reply", nil)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error ending receipt")
				}
			}

			// L3: the replayed result must equal the first result
			// verbatim, so the caller-facing view IS the fact set —
			// asked as ids, names resolve via the member table.
			facts := map[string]any{
				"thread":    th.Code,
				"entry":     entryID,
				"seq":       seq,
				"memory":    memoryRef,
				"revision":  memoryRevision,
				"asked":     asked,
				"fulfilled": fulfilledReply,
			}
			if assignID != 0 {
				facts["assign"] = assignID
			}
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// ThreadHandle ends one of my pending receipts without speaking
// (§6.3). The note, when present, persists on the receipt only.
func ThreadHandle(ctx context.Context, pool *pg.Pool, botID int64, threadCode string,
	entryID int64, note string, idemKey string) (map[string]any, error) {

	requestHash := sha256Hex(strings.Join([]string{"v1", threadCode, numStr(entryID), note}, "\x1f"))

	return runThreadAction(ctx, pool, botID, "thread_handle", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, threadCode)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
			}
			if th == nil {
				return threadActionOutcome{}, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
			}
			if me == nil {
				return threadActionOutcome{}, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
			}
			if th.Status != repository.ThreadStatusOpen {
				return threadActionOutcome{}, errors.New(409, "THREAD_CLOSED", "Thread is closed")
			}
			if utf8.RuneCountInString(note) > threadHandleMaxNoteRunes {
				return threadActionOutcome{}, errors.New(413, "CONTENT_TOO_LARGE", "Note exceeds 1000 characters")
			}
			entry, err := repository.FindThreadEntry(ctx, tx, th.ID, entryID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading entry")
			}
			if entry == nil {
				return threadActionOutcome{}, errors.New(422, "INVALID_TARGET", "Entry not in this thread")
			}
			var notePtr *string
			if note != "" {
				notePtr = &note
			}
			ok, err := repository.FulfillThreadReceipt(ctx, tx, entryID, botID, "handle", notePtr)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error ending receipt")
			}
			if !ok {
				return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
					"No pending receipt of yours on this entry")
			}
			facts := map[string]any{"thread": th.Code, "entry": entryID, "resolution": "handle"}
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

// ThreadRetract withdraws every still-pending receipt the author's
// entry created (§6.3); fulfilled history is untouchable (L1).
func ThreadRetract(ctx context.Context, pool *pg.Pool, botID int64, threadCode string,
	entryID int64, idemKey string) (map[string]any, error) {

	requestHash := sha256Hex(strings.Join([]string{"v1", threadCode, numStr(entryID)}, "\x1f"))

	return runThreadAction(ctx, pool, botID, "thread_retract", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, threadCode)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
			}
			if th == nil {
				return threadActionOutcome{}, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
			}
			if me == nil {
				return threadActionOutcome{}, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
			}
			if th.Status != repository.ThreadStatusOpen {
				return threadActionOutcome{}, errors.New(409, "THREAD_CLOSED", "Thread is closed")
			}
			entry, err := repository.FindThreadEntry(ctx, tx, th.ID, entryID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error loading entry")
			}
			if entry == nil {
				return threadActionOutcome{}, errors.New(422, "INVALID_TARGET", "Entry not in this thread")
			}
			if entry.AuthorID != botID {
				return threadActionOutcome{}, errors.New(403, "NOT_YOURS", "Only the entry author can retract")
			}
			n, err := repository.WithdrawThreadReceiptsByEntry(ctx, tx, entryID, "retract")
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error withdrawing receipts")
			}
			facts := map[string]any{"thread": th.Code, "entry": entryID, "withdrawn": n}
			return threadActionOutcome{Facts: facts, View: facts}, nil
		})
}

func numStr(n int64) string { return strconv.FormatInt(n, 10) }

func numsJoin(ns []int64) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, strconv.FormatInt(n, 10))
	}
	return strings.Join(parts, ",")
}
