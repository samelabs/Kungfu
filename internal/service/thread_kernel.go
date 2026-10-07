package service

// Thread kernel (D2) — kungfu.md §6.1, §6.2, §6.5, §4, L2–L5.
// Room skeleton: start (with optional first key in the same
// transaction), key issue/revoke, join by key, leave, member
// governance, close; every write runs through the L3 idempotency
// seam (thread_idempotency). Entries/receipts/assignments and turn
// projections are the D3–D5 stages — thread_get carries their empty
// placeholders.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// PRD §2 application values (L4 caps; cursor page ≤ 50).
const (
	threadMaxSubjectRunes        = 200
	threadMaxMembers             = 50
	threadMaxOpenRoomsPerAccount = 100
	threadPageSize               = 50
)

// -- keys (§6.1, §2) --

// generateThreadKey mints one key: raw "kf_"+32hex (disclosed in the
// first successful response only), sha256 hex digest (the only stored
// form), and a non-secret fingerprint that survives idempotent replay.
func generateThreadKey() (raw, hash, fingerprint string, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}
	body := hex.EncodeToString(b)
	raw = "kf_" + body
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	fingerprint = "kf_" + body[:4] + "…" + body[28:]
	return raw, hash, fingerprint, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func threadValidRole(role string) bool {
	return role == repository.ThreadRoleGovernor ||
		role == repository.ThreadRoleSpeaker ||
		role == repository.ThreadRoleObserver
}

// -- L3 idempotency seam --

// threadActionOutcome is what one write tool's transaction produced:
//
//	Facts     protocol-fact fields of the first result — the ONLY
//	          part stored in thread_idempotency and replayed verbatim
//	          (no post-hoc projection; raw key material absent)
//	FirstOnly one-time disclosure (the raw key) — first response only
//	View      fresh working-set projection ("next") — first response
//	          only; a replay returns the stored facts, unchanged
type threadActionOutcome struct {
	Facts     map[string]any
	FirstOnly map[string]any
	View      map[string]any
}

// runThreadAction executes one write tool inside a single
// transaction. With an idempotency key (L3): an existing record with
// the same request hash replays its stored snapshot; the same key
// with a different request is IDEMPOTENCY_CONFLICT with no side
// effects; the first success stores its facts before commit. Without
// a key the action still runs, just leaves no replayable receipt.
func runThreadAction(ctx context.Context, pool *pg.Pool, botID int64, tool, idemKey, requestHash string,
	work func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error)) (map[string]any, error) {

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
	}
	defer func() { _ = pg.Rollback(tx) }()

	if idemKey != "" {
		rec, err := repository.FindThreadIdempotency(ctx, tx, botID, tool, idemKey)
		if err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
		}
		if rec != nil {
			if rec.RequestHash != requestHash {
				return nil, errors.New(409, "IDEMPOTENCY_CONFLICT",
					"This idempotency key was already used with a different request")
			}
			var facts map[string]any
			if err := json.Unmarshal([]byte(rec.ResultSnapshot), &facts); err != nil {
				return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
			}
			return facts, nil // the stored receipt, verbatim
		}
	}

	out, err := work(ctx, tx)
	if err != nil {
		return nil, err
	}

	if idemKey != "" {
		snapshot, mErr := json.Marshal(out.Facts)
		if mErr != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
		}
		if err := repository.InsertThreadIdempotency(ctx, tx, botID, tool, idemKey,
			requestHash, string(snapshot)); err != nil {
			if !repository.IsUniqueViolation(err) {
				return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
			}
			// A concurrent request with the same (account, tool, key)
			// committed first (L5): this transaction's work rolls back
			// — the primary key made the double execution impossible —
			// and the losing call resolves against the winner's
			// receipt: replay or conflict, never a second effect.
			_ = pg.Rollback(tx)
			rec, findErr := repository.FindThreadIdempotency(ctx, pool, botID, tool, idemKey)
			if findErr != nil || rec == nil {
				return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
			}
			if rec.RequestHash != requestHash {
				return nil, errors.New(409, "IDEMPOTENCY_CONFLICT",
					"This idempotency key was already used with a different request")
			}
			var facts map[string]any
			if err := json.Unmarshal([]byte(rec.ResultSnapshot), &facts); err != nil {
				return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
			}
			return facts, nil
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during "+tool)
	}

	env := map[string]any{}
	for k, v := range out.Facts {
		env[k] = v
	}
	for k, v := range out.FirstOnly {
		env[k] = v
	}
	for k, v := range out.View {
		env[k] = v
	}
	return env, nil
}

// requireActiveAccount: only active accounts execute actions (§4).
// Checked inside the caller's transaction so a deactivation racing a
// write produces one of the two legal serial orders.
func requireActiveAccount(ctx context.Context, q pg.Querier, botID int64) error {
	active, err := repository.FindActiveBotAccountByID(ctx, q, botID)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Error checking account status")
	}
	if active == nil {
		return errors.New(401, "UNAUTHORIZED", "Account is not active")
	}
	return nil
}

// threadNextActions projects the §6.2 capability table onto the
// D2-deployed tools for the caller's role and the room's current
// facts (order: the §6.2 governor capability list, then leave). The
// D3–D5 tools join this list when they exist.
func threadNextActions(th *model.Thread, role string, members, governors int64) []string {
	next := []string{}
	if th.Status == repository.ThreadStatusClosed {
		return []string{"thread_leave"} // members may still leave (§6.5)
	}
	if role == repository.ThreadRoleGovernor {
		next = append(next, "thread_key")
		if th.KeyHash != nil {
			next = append(next, "thread_key_revoke")
		}
		if members > 1 {
			next = append(next, "thread_remove", "thread_set_role")
		}
		next = append(next, "thread_close")
		if governors > 1 {
			next = append(next, "thread_leave") // sole governor cannot leave (§6.2)
		}
		return next
	}
	return []string{"thread_leave"}
}

func threadView(next []string) map[string]any {
	return map[string]any{"next": next}
}

// loadRoomForGovernor runs the shared governance preconditions under
// the room lock, in order: room exists → caller is a member → caller
// is a governor → room is open. Returns the locked room and the
// caller's membership row.
func loadRoomForGovernor(ctx context.Context, tx pgx.Tx, code string, botID int64) (*model.Thread, *model.ThreadMember, error) {
	th, err := repository.LockThreadByCode(ctx, tx, code)
	if err != nil {
		return nil, nil, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
	}
	if th == nil {
		return nil, nil, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
	}
	me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
	if err != nil {
		return nil, nil, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
	}
	if me == nil {
		return nil, nil, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
	}
	if me.Role != repository.ThreadRoleGovernor {
		return nil, nil, errors.New(403, "NOT_GOVERNOR", "Only a governor may do this")
	}
	if th.Status != repository.ThreadStatusOpen {
		return nil, nil, errors.New(409, "THREAD_CLOSED", "Thread is closed and read-only")
	}
	return th, me, nil
}

// -- thread_start (§6.1) --

// ThreadStart creates an open room; the creator joins as governor.
// key=true issues the first key in the same transaction (L2), bound
// to the default speaker role (§6.1).
func ThreadStart(ctx context.Context, pool *pg.Pool, botID int64, subject string, issueKey bool, idemKey string) (map[string]any, error) {
	subject = strings.TrimSpace(subject)
	if utf8.RuneCountInString(subject) > threadMaxSubjectRunes {
		return nil, errors.NewWithDetails(400, "VALIDATION_FAILED", "subject exceeds 200 characters",
			map[string]interface{}{"errors": []map[string]string{
				{"field": "subject", "message": "subject is limited to 200 characters"},
			}})
	}
	var subjectPtr *string
	if subject != "" {
		subjectPtr = &subject
	}
	requestHash := sha256Hex(fmt.Sprintf(`{"key":%t,"subject":%q}`, issueKey, subject))

	return runThreadAction(ctx, pool, botID, "thread_start", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			if err := repository.LockAccountRow(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}
			open, err := repository.CountOpenThreadMembershipsByAccount(ctx, tx, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}
			if open >= threadMaxOpenRoomsPerAccount {
				return threadActionOutcome{}, errors.New(409, "ROOM_LIMIT",
					"Account is already a member of 100 open threads")
			}

			code, err := repository.GenerateUniqueThreadCode(ctx, tx)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}
			id, err := repository.InsertThread(ctx, tx, code, subjectPtr)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}
			if err := repository.InsertThreadMember(ctx, tx, id, botID,
				repository.ThreadRoleGovernor, nil); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}

			facts := map[string]any{
				"thread": code,
				"status": repository.ThreadStatusOpen,
				"role":   repository.ThreadRoleGovernor,
				"key":    "", // raw key appears in the first response only (L3)
			}
			firstOnly := map[string]any{}
			if issueKey {
				raw, hash, fingerprint, kErr := generateThreadKey()
				if kErr != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
				}
				if err := repository.IssueThreadKey(ctx, tx, id, hash,
					repository.ThreadRoleSpeaker); err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
				}
				facts["key_role"] = repository.ThreadRoleSpeaker
				facts["key_fingerprint"] = fingerprint
				firstOnly["key"] = raw
			}
			if subjectPtr != nil {
				facts["subject"] = subject
			}
			// fresh room: one member (the creator-governor), one governor
			room, err := repository.FindThreadByCode(ctx, tx, code)
			if err != nil || room == nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_start")
			}
			return threadActionOutcome{
				Facts:     facts,
				FirstOnly: firstOnly,
				View:      threadView(threadNextActions(room, repository.ThreadRoleGovernor, 1, 1)),
			}, nil
		})
}

// -- thread_key / thread_key_revoke (§6.1) --

// ThreadIssueKey signs a new key bound to role (default speaker) and
// thereby invalidates the previous key in the same transaction. The
// raw key is disclosed in the first successful response only; the
// stored receipt keeps the fingerprint.
func ThreadIssueKey(ctx context.Context, pool *pg.Pool, botID int64, code, role, idemKey string) (map[string]any, error) {
	if role == "" {
		role = repository.ThreadRoleSpeaker // §6.1 default
	}
	if !threadValidRole(role) {
		return nil, errors.NewWithDetails(400, "VALIDATION_FAILED", "role must be governor, speaker or observer",
			map[string]interface{}{"errors": []map[string]string{
				{"field": "role", "message": "role must be one of governor, speaker, observer"},
			}})
	}
	requestHash := sha256Hex(fmt.Sprintf(`{"role":%q,"thread":%q}`, role, code))

	return runThreadAction(ctx, pool, botID, "thread_key", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, _, err := loadRoomForGovernor(ctx, tx, code, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			raw, hash, fingerprint, kErr := generateThreadKey()
			if kErr != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key")
			}
			if err := repository.IssueThreadKey(ctx, tx, th.ID, hash, role); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key")
			}
			members, err := repository.CountThreadMembers(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key")
			}
			governors, err := repository.CountThreadGovernors(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key")
			}
			newRoom, err := repository.FindThreadByCode(ctx, tx, code)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key")
			}
			return threadActionOutcome{
				Facts: map[string]any{
					"thread":                   th.Code,
					"key":                      "", // one-time disclosure below (L3)
					"key_role":                 role,
					"key_fingerprint":          fingerprint,
					"previous_key_invalidated": th.KeyHash != nil,
				},
				FirstOnly: map[string]any{"key": raw},
				View:      threadView(threadNextActions(newRoom, repository.ThreadRoleGovernor, members, governors)),
			}, nil
		})
}

// ThreadRevokeKey invalidates the active key on its own (§6.1).
func ThreadRevokeKey(ctx context.Context, pool *pg.Pool, botID int64, code, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"thread":%q}`, code))

	return runThreadAction(ctx, pool, botID, "thread_key_revoke", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, _, err := loadRoomForGovernor(ctx, tx, code, botID)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if th.KeyHash == nil {
				return threadActionOutcome{}, errors.New(422, "INVALID_TARGET",
					"Thread has no active key to revoke")
			}
			if err := repository.RevokeThreadKey(ctx, tx, th.ID); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_key_revoke")
			}
			return threadActionOutcome{
				Facts: map[string]any{"thread": th.Code, "key_revoked": true},
				View:  threadView(threadGovernorNextAfter(ctx, tx, th)),
			}, nil
		})
}

// -- thread_join (§6.1, L2) --

// ThreadJoin admits an active account holding the current key. A
// member joining again returns the existing membership unchanged —
// the protocol's second sanctioned no-new-effect success (L2); any
// key rotation or close that committed first is seen under the room
// lock (L5).
func ThreadJoin(ctx context.Context, pool *pg.Pool, botID int64, rawKey, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"key":%q}`, rawKey))

	return runThreadAction(ctx, pool, botID, "thread_join", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			hash := sha256Hex(rawKey)
			found, err := repository.FindOpenThreadByKeyHash(ctx, tx, hash)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			if found == nil {
				return threadActionOutcome{}, errors.New(401, "KEY_INVALID", "Key is invalid")
			}
			th, err := repository.LockThreadByID(ctx, tx, found.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			// Re-verify under the lock: a close or key rotation that
			// committed between the lookup and the lock kills this key.
			if th == nil || th.Status != repository.ThreadStatusOpen ||
				th.KeyHash == nil || *th.KeyHash != hash {
				return threadActionOutcome{}, errors.New(401, "KEY_INVALID", "Key is invalid")
			}

			if existing, err := repository.FindThreadMember(ctx, tx, th.ID, botID); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			} else if existing != nil {
				// L2: duplicate join with a valid key — return the
				// existing membership, no new effects, role unchanged.
				members, err := repository.CountThreadMembers(ctx, tx, th.ID)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
				}
				governors, err := repository.CountThreadGovernors(ctx, tx, th.ID)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
				}
				return threadActionOutcome{
					Facts: map[string]any{
						"thread":    th.Code,
						"role":      existing.Role,
						"joined_at": existing.JoinedAt,
					},
					View: threadView(threadNextActions(th, existing.Role, members, governors)),
				}, nil
			}

			members, err := repository.CountThreadMembers(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			if members >= threadMaxMembers {
				return threadActionOutcome{}, errors.New(409, "MEMBER_LIMIT",
					"Thread already has 50 members")
			}
			if err := repository.LockAccountRow(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			open, err := repository.CountOpenThreadMembershipsByAccount(ctx, tx, botID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			if open >= threadMaxOpenRoomsPerAccount {
				return threadActionOutcome{}, errors.New(409, "ROOM_LIMIT",
					"Account is already a member of 100 open threads")
			}

			if err := repository.InsertThreadMember(ctx, tx, th.ID, botID,
				*th.KeyRole, &hash); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			me, err := repository.FindThreadMember(ctx, tx, th.ID, botID)
			if err != nil || me == nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			governors, err := repository.CountThreadGovernors(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_join")
			}
			return threadActionOutcome{
				Facts: map[string]any{
					"thread":    th.Code,
					"role":      me.Role,
					"joined_at": me.JoinedAt,
				},
				View: threadView(threadNextActions(th, me.Role, members+1, governors)),
			}, nil
		})
}

// -- thread_leave (§6.2) --

// ThreadLeave terminates the caller's membership; allowed in open and
// closed rooms alike. Leaving as the last governor of an open room is
// LAST_MANAGER — hand over or close first.
func ThreadLeave(ctx context.Context, pool *pg.Pool, botID int64, code, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"thread":%q}`, code))

	return runThreadAction(ctx, pool, botID, "thread_leave", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, code)
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
			if th.Status == repository.ThreadStatusOpen && me.Role == repository.ThreadRoleGovernor {
				governors, err := repository.CountThreadGovernors(ctx, tx, th.ID)
				if err != nil {
					return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_leave")
				}
				if governors <= 1 {
					return threadActionOutcome{}, errors.New(409, "LAST_MANAGER",
						"An open thread must keep at least one governor; hand over or close first")
				}
			}
			if _, err := repository.DeleteThreadMember(ctx, tx, th.ID, botID); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_leave")
			}
			return threadActionOutcome{
				Facts: map[string]any{"thread": th.Code, "left": true},
				View:  threadView([]string{}),
			}, nil
		})
}

// -- thread_remove / thread_set_role (§6.2) --

// ThreadRemoveMember removes another member (governor action). The
// last governor cannot be removed from an open room (LAST_MANAGER).
func ThreadRemoveMember(ctx context.Context, pool *pg.Pool, botID int64, code string, member int64, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"member":%d,"thread":%q}`, member, code))

	return runThreadAction(ctx, pool, botID, "thread_remove", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			th, err := threadGovernedMemberChange(ctx, tx, code, botID, member, "")
			if err != nil {
				return threadActionOutcome{}, err
			}
			if _, err := repository.DeleteThreadMember(ctx, tx, th.ID, member); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_remove")
			}
			return threadActionOutcome{
				Facts: map[string]any{"thread": th.Code, "member": member, "removed": true},
				View:  threadView(threadGovernorNextAfter(ctx, tx, th)),
			}, nil
		})
}

// ThreadSetRole changes a member's role (governor action). Downgrading
// the last governor of an open room is LAST_MANAGER; setting a role
// the member already holds is rejected (L2 allows no extra
// no-effect successes).
func ThreadSetRole(ctx context.Context, pool *pg.Pool, botID int64, code string, member int64, role, idemKey string) (map[string]any, error) {
	if !threadValidRole(role) {
		return nil, errors.NewWithDetails(400, "VALIDATION_FAILED", "role must be governor, speaker or observer",
			map[string]interface{}{"errors": []map[string]string{
				{"field": "role", "message": "role must be one of governor, speaker, observer"},
			}})
	}
	requestHash := sha256Hex(fmt.Sprintf(`{"member":%d,"role":%q,"thread":%q}`, member, role, code))

	return runThreadAction(ctx, pool, botID, "thread_set_role", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			th, err := threadGovernedMemberChange(ctx, tx, code, botID, member, role)
			if err != nil {
				return threadActionOutcome{}, err
			}
			if err := repository.SetThreadMemberRole(ctx, tx, th.ID, member, role); err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_set_role")
			}
			return threadActionOutcome{
				Facts: map[string]any{"thread": th.Code, "member": member, "role": role},
				View:  threadView(threadGovernorNextAfter(ctx, tx, th)),
			}, nil
		})
}

// threadGovernedMemberChange runs the shared preconditions of
// remove/set_role under the room lock: caller is an active governor
// of an open room, the target is a member, and the open-room
// ≥1-governor invariant survives the change. newRole "" means removal.
func threadGovernedMemberChange(ctx context.Context, tx pgx.Tx, code string, botID, member int64, newRole string) (*model.Thread, error) {
	if err := requireActiveAccount(ctx, tx, botID); err != nil {
		return nil, err
	}
	th, _, err := loadRoomForGovernor(ctx, tx, code, botID)
	if err != nil {
		return nil, err
	}
	target, err := repository.FindThreadMember(ctx, tx, th.ID, member)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading member")
	}
	if target == nil {
		return nil, errors.New(422, "INVALID_TARGET", "Not a member of this thread")
	}
	if newRole != "" && target.Role == newRole {
		return nil, errors.New(422, "INVALID_TARGET", "Member already has this role")
	}
	// §6.2: an open thread always keeps at least one governor.
	if target.Role == repository.ThreadRoleGovernor && newRole != repository.ThreadRoleGovernor {
		governors, err := repository.CountThreadGovernors(ctx, tx, th.ID)
		if err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error counting governors")
		}
		if governors <= 1 {
			return nil, errors.New(409, "LAST_MANAGER",
				"An open thread must keep at least one governor; hand over or close first")
		}
	}
	return th, nil
}

func threadGovernorNextAfter(ctx context.Context, tx pgx.Tx, th *model.Thread) []string {
	members, _ := repository.CountThreadMembers(ctx, tx, th.ID)
	governors, _ := repository.CountThreadGovernors(ctx, tx, th.ID)
	refreshed, _ := repository.FindThreadByCode(ctx, tx, th.Code)
	if refreshed == nil {
		refreshed = th
	}
	return threadNextActions(refreshed, repository.ThreadRoleGovernor, members, governors)
}

// -- thread_close (§6.5) --

// ThreadClose closes the room: terminal state, key invalidated,
// memberships kept read-only (members may still leave). Entry,
// receipt and assignment close-outs are the D3/D4 stages — this
// stage's room has none, so those close-outs are vacuous.
func ThreadClose(ctx context.Context, pool *pg.Pool, botID int64, code, idemKey string) (map[string]any, error) {
	requestHash := sha256Hex(fmt.Sprintf(`{"thread":%q}`, code))

	return runThreadAction(ctx, pool, botID, "thread_close", idemKey, requestHash,
		func(ctx context.Context, tx pgx.Tx) (threadActionOutcome, error) {
			if err := requireActiveAccount(ctx, tx, botID); err != nil {
				return threadActionOutcome{}, err
			}
			th, err := repository.LockThreadByCode(ctx, tx, code)
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
			if me.Role != repository.ThreadRoleGovernor {
				return threadActionOutcome{}, errors.New(403, "NOT_GOVERNOR", "Only a governor may close this thread")
			}
			if th.Status != repository.ThreadStatusOpen {
				return threadActionOutcome{}, errors.New(409, "THREAD_CLOSED", "Thread is already closed")
			}
			ok, err := repository.CloseThreadByID(ctx, tx, th.ID)
			if err != nil {
				return threadActionOutcome{}, errors.New(500, "INTERNAL_ERROR", "Error occurred during thread_close")
			}
			if !ok {
				return threadActionOutcome{}, errors.New(409, "THREAD_CLOSED", "Thread is already closed")
			}
			return threadActionOutcome{
				Facts: map[string]any{"thread": th.Code, "status": repository.ThreadStatusClosed},
				View:  threadView([]string{"thread_leave"}),
			}, nil
		})
}

// -- thread_get / thread_list (§8 working set, D2 part) --

// ThreadGet returns the caller's working set for one room: room
// structure, own role, the member table, key facts (§9), the D2
// action list — and empty placeholders for the timeline and open
// items (D3/D5 fill those).
func ThreadGet(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]any, error) {
	th, err := repository.FindThreadByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading thread")
	}
	if th == nil {
		return nil, errors.New(404, "THREAD_NOT_FOUND", "Thread not found")
	}
	me, err := repository.FindThreadMember(ctx, pool, th.ID, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading membership")
	}
	if me == nil {
		return nil, errors.New(403, "NOT_MEMBER", "You are not a member of this thread")
	}

	rows, err := repository.ListThreadMembers(ctx, pool, th.ID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading members")
	}
	members := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{
			"account_id": r.AccountID,
			"account":    r.BotName,
			"role":       r.Role,
			"joined_at":  r.JoinedAt,
		}
		if r.JoinedViaKeyHash != nil {
			m["joined_via_key_hash"] = *r.JoinedViaKeyHash
		}
		members = append(members, m)
	}

	keyFacts := map[string]any{"active": false}
	if th.KeyHash != nil {
		keyFacts = map[string]any{
			"active":    true,
			"role":      *th.KeyRole,
			"issued_at": *th.KeyIssuedAt,
		}
	}

	var governors int64
	for _, r := range rows {
		if r.Role == repository.ThreadRoleGovernor {
			governors++
		}
	}

	room := map[string]any{"code": th.Code, "status": th.Status}
	if th.Subject != nil {
		room["subject"] = *th.Subject
	}
	return map[string]any{
		"thread":   room,
		"role":     me.Role,
		"members":  members,
		"key":      keyFacts,
		"timeline": []any{}, // D3 stage
		"todos":    []any{}, // D5 stage
		"next":     threadNextActions(th, me.Role, int64(len(rows)), governors),
	}, nil
}

// ThreadList returns the caller's rooms, newest first, cursor-paginated
// (≤50 per page). Open-item counts join with the D5 turn projection.
func ThreadList(ctx context.Context, pool *pg.Pool, botID int64, status, cursor string) (map[string]any, error) {
	if status != "" && status != repository.ThreadStatusOpen && status != repository.ThreadStatusClosed {
		return nil, errors.NewWithDetails(400, "VALIDATION_FAILED", "status must be open or closed",
			map[string]interface{}{"errors": []map[string]string{
				{"field": "status", "message": "status must be one of open, closed"},
			}})
	}
	var beforeID int64
	if strings.TrimSpace(cursor) != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(cursor), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New(422, "VALIDATION_FAILED", "cursor is not a valid page cursor")
		}
		beforeID = id
	}

	rows, err := repository.ListThreadsForMember(ctx, pool, botID, status, beforeID, threadPageSize+1)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error loading threads")
	}
	var nextCursor any
	if len(rows) > threadPageSize {
		nextCursor = strconv.FormatInt(rows[threadPageSize-1].ID, 10)
		rows = rows[:threadPageSize]
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		room := map[string]any{"code": r.Code, "status": r.Status}
		if r.Subject != nil {
			room["subject"] = *r.Subject
		}
		items = append(items, map[string]any{
			"thread":     room,
			"role":       r.Role,
			"joined_at":  r.JoinedAt,
			"open_items": 0, // D5 turn projection
		})
	}
	return map[string]any{
		"threads":     items,
		"next_cursor": nextCursor,
	}, nil
}
