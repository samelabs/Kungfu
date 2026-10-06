package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

var (
	ErrThreadStaleInput          = errors.New("thread input is stale")
	ErrThreadIdempotencyConflict = errors.New("thread idempotency key conflicts with a different request")
)

const (
	threadOpCreate     = "thread_create"
	threadOpReply      = "thread_reply"
	threadOpBranch     = "thread_branch"
	threadOpHandle     = "thread_handle"
	threadOpRoleAdd    = "thread_role_add"
	threadOpJoin       = "thread_join"
	threadOpRoleRemove = "thread_role_remove"
	threadOpPermission = "thread_permission"
	threadOpClose      = "thread_close"
	threadOpReopen     = "thread_reopen"
	threadOpKeyReset   = "thread_key_reset"
	threadOpKeyRevoke  = "thread_key_revoke"
	threadOpSubject    = "thread_subject"
)

type ThreadParticipantSpec struct {
	RoleID     int64  `json:"role_id"`
	Permission string `json:"permission"`
}

type ThreadCreateInput struct {
	Subject        string                  `json:"subject"`
	Content        string                  `json:"content"`
	Participants   []ThreadParticipantSpec `json:"participants,omitempty"`
	IssueJoinKey   bool                    `json:"issue_join_key,omitempty"`
	IdempotencyKey string                  `json:"-"`
}

type ThreadCreateResult struct {
	Thread             *model.Thread
	RootEntry          *model.ThreadMemory
	JoinKey            string
	JoinKeyFingerprint string
	AlreadyApplied     bool
}

type ThreadReplyInput struct {
	ThreadID       int64  `json:"thread_id"`
	ReplyToEntryID int64  `json:"reply_to_entry_id"`
	InputEntryID   *int64 `json:"input_entry_id,omitempty"`
	Content        string `json:"content"`
	IdempotencyKey string `json:"-"`
}

type ThreadReplyResult struct {
	Thread         *model.Thread
	Entry          *model.ThreadMemory
	AlreadyApplied bool
}

type ThreadBranchInput struct {
	ParentThreadID int64                   `json:"parent_thread_id"`
	AnchorEntryID  int64                   `json:"anchor_entry_id"`
	InputEntryID   *int64                  `json:"input_entry_id,omitempty"`
	Subject        string                  `json:"subject"`
	Participants   []ThreadParticipantSpec `json:"participants,omitempty"`
	IssueJoinKey   bool                    `json:"issue_join_key,omitempty"`
	IdempotencyKey string                  `json:"-"`
}

type ThreadBranchResult struct {
	Thread             *model.Thread
	JoinKey            string
	JoinKeyFingerprint string
	AlreadyApplied     bool
}

type ThreadJoinResult struct {
	Thread         *model.Thread
	Role           *model.ThreadRole
	Joined         bool
	AlreadyApplied bool
}

type threadIdempotencyRef struct {
	ThreadID    int64  `json:"thread_id,omitempty"`
	EntryID     int64  `json:"entry_id,omitempty"`
	RoleID      int64  `json:"role_id,omitempty"`
	InputID     int64  `json:"input_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Changed     bool   `json:"changed,omitempty"`
}

func threadRequestHash(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return sum[:], nil
}

func beginThreadIdempotency(ctx context.Context, tx pgx.Tx, roleID int64, operation, key string, request any) (*threadIdempotencyRef, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, errors.New("idempotency key is required")
	}
	hash, err := threadRequestHash(request)
	if err != nil {
		return nil, false, err
	}
	inserted, err := repository.TryInsertThreadIdempotency(ctx, tx, roleID, operation, key, hash)
	if err != nil {
		return nil, false, err
	}
	if inserted {
		return nil, true, nil
	}
	existing, err := repository.FindThreadIdempotency(ctx, tx, roleID, operation, key)
	if err != nil || existing == nil {
		if err == nil {
			err = errors.New("idempotency record not found")
		}
		return nil, false, err
	}
	if !bytes.Equal(existing.RequestHash, hash) {
		return nil, false, ErrThreadIdempotencyConflict
	}
	if existing.ResultRef == "" {
		return nil, false, errors.New("idempotency result is incomplete")
	}
	var ref threadIdempotencyRef
	if err := json.Unmarshal([]byte(existing.ResultRef), &ref); err != nil {
		return nil, false, err
	}
	return &ref, false, nil
}

func completeThreadIdempotency(ctx context.Context, tx pgx.Tx, roleID int64, operation, key string, ref threadIdempotencyRef) error {
	body, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return repository.CompleteThreadIdempotency(ctx, tx, roleID, operation, strings.TrimSpace(key), string(body))
}

func threadKeyHashFingerprint(hash []byte) string {
	if len(hash) < 6 {
		return ""
	}
	return hex.EncodeToString(hash[:6])
}

func threadKeyFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return threadKeyHashFingerprint(sum[:])
}

func requireThreadManageOrGovern(ctx context.Context, q pg.Querier, actorID, threadID int64) error {
	role, err := repository.FindThreadRole(ctx, q, threadID, actorID)
	if err != nil {
		return err
	}
	govern, err := repository.RootCreatorCanGovernThread(ctx, q, threadID, actorID)
	if err != nil {
		return err
	}
	if (role == nil || role.Permission != model.ThreadManage) && !govern {
		return errors.New("thread manage permission required")
	}
	return nil
}

func requireOpenThreadWriter(ctx context.Context, q pg.Querier, actorID, threadID int64) (*model.Thread, *model.ThreadRole, error) {
	thread, err := repository.FindThreadByID(ctx, q, threadID)
	if err != nil || thread == nil {
		if err == nil {
			err = errors.New("thread not found")
		}
		return nil, nil, err
	}
	if thread.Status != model.ThreadOpen {
		return thread, nil, errors.New("thread is closed")
	}
	role, err := repository.FindThreadRole(ctx, q, threadID, actorID)
	if err != nil {
		return thread, nil, err
	}
	if !threadRoleCanWrite(role) {
		return thread, role, errors.New("current ThreadRole cannot write")
	}
	return thread, role, nil
}

func CreateThreadState(ctx context.Context, pool *pg.Pool, actorID int64, in ThreadCreateInput) (*ThreadCreateResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	hashInput := struct {
		Subject      string                  `json:"subject"`
		Content      string                  `json:"content"`
		Participants []ThreadParticipantSpec `json:"participants,omitempty"`
		IssueJoinKey bool                    `json:"issue_join_key,omitempty"`
	}{strings.TrimSpace(in.Subject), in.Content, in.Participants, in.IssueJoinKey}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpCreate, in.IdempotencyKey, hashInput)
	if err != nil {
		return nil, err
	}
	if !acquired {
		thread, err := repository.FindThreadByID(ctx, tx, replay.ThreadID)
		if err != nil || thread == nil {
			return nil, errors.New("idempotent Thread result not found")
		}
		entry, err := repository.FindThreadMemoryByID(ctx, tx, replay.EntryID)
		if err != nil || entry == nil {
			return nil, errors.New("idempotent root entry not found")
		}
		return &ThreadCreateResult{
			Thread: thread, RootEntry: entry,
			JoinKeyFingerprint: threadKeyHashFingerprint(thread.JoinKeyHash), AlreadyApplied: true,
		}, nil
	}

	thread, entry, err := createRootThreadKernel(ctx, tx, actorID, in.Subject, in.Content)
	if err != nil {
		return nil, err
	}
	for _, p := range in.Participants {
		if p.RoleID == actorID {
			return nil, errors.New("creator must not be repeated as an initial participant")
		}
		permission := p.Permission
		if permission == "" {
			permission = model.ThreadWrite
		}
		added, err := addThreadRoleKernel(ctx, tx, actorID, thread.ID, p.RoleID, permission, entry.ID)
		if err != nil {
			return nil, err
		}
		if !added {
			return nil, errors.New("initial participant already exists")
		}
		if permission == model.ThreadWrite || permission == model.ThreadManage {
			ok, err := repository.InsertPendingThreadReceipt(ctx, tx, thread.ID, entry.ID, p.RoleID, model.ThreadReceiptEntry)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("initial participant receipt already exists")
			}
		}
	}

	result := &ThreadCreateResult{Thread: thread, RootEntry: entry}
	if in.IssueJoinKey {
		raw, err := resetThreadJoinKeyKernel(ctx, tx, actorID, thread.ID, entry.ID)
		if err != nil {
			return nil, err
		}
		result.JoinKey = raw
		result.JoinKeyFingerprint = threadKeyFingerprint(raw)
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpCreate, in.IdempotencyKey, threadIdempotencyRef{
		ThreadID: thread.ID, EntryID: entry.ID, Fingerprint: result.JoinKeyFingerprint,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	result.Thread, _ = repository.FindThreadByID(ctx, pool, thread.ID)
	return result, nil
}

func ReplyThreadState(ctx context.Context, pool *pg.Pool, actorID int64, in ThreadReplyInput) (*ThreadReplyResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	hashInput := struct {
		ThreadID       int64  `json:"thread_id"`
		ReplyToEntryID int64  `json:"reply_to_entry_id"`
		InputEntryID   *int64 `json:"input_entry_id,omitempty"`
		Content        string `json:"content"`
	}{in.ThreadID, in.ReplyToEntryID, in.InputEntryID, in.Content}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpReply, in.IdempotencyKey, hashInput)
	if err != nil {
		return nil, err
	}
	if !acquired {
		entry, err := repository.FindThreadMemoryByID(ctx, tx, replay.EntryID)
		if err != nil || entry == nil {
			return nil, errors.New("idempotent reply result not found")
		}
		thread, _ := repository.FindThreadByID(ctx, tx, replay.ThreadID)
		return &ThreadReplyResult{Thread: thread, Entry: entry, AlreadyApplied: true}, nil
	}

	if _, err := repository.FindThreadByIDForUpdate(ctx, tx, in.ThreadID); err != nil {
		return nil, err
	}
	if _, _, err := requireOpenThreadWriter(ctx, tx, actorID, in.ThreadID); err != nil {
		return nil, err
	}
	allowed, err := repository.EntryAllowedInThreadScope(ctx, tx, in.ThreadID, in.ReplyToEntryID)
	if err != nil || !allowed {
		if err == nil {
			err = errors.New("reply target is outside thread scope")
		}
		return nil, err
	}
	if in.InputEntryID != nil {
		if *in.InputEntryID != in.ReplyToEntryID {
			return nil, errors.New("Todo reply target must equal its input entry")
		}
		handled, err := repository.HandlePendingThreadReceipt(ctx, tx, in.ThreadID, *in.InputEntryID, actorID)
		if err != nil {
			return nil, err
		}
		if !handled {
			return nil, ErrThreadStaleInput
		}
	}

	target, err := repository.FindThreadMemoryByID(ctx, tx, in.ReplyToEntryID)
	if err != nil || target == nil {
		if err == nil {
			err = errors.New("reply target not found")
		}
		return nil, err
	}
	entry, err := appendThreadMemoryKernel(ctx, tx, in.ThreadID, actorID, in.Content, &in.ReplyToEntryID)
	if err != nil {
		return nil, err
	}
	if target.AuthorRoleID != actorID {
		targetRole, err := repository.FindThreadRole(ctx, tx, in.ThreadID, target.AuthorRoleID)
		if err != nil {
			return nil, err
		}
		if threadRoleCanWrite(targetRole) {
			if _, err := repository.InsertPendingThreadReceipt(ctx, tx, in.ThreadID, entry.ID, target.AuthorRoleID, model.ThreadReceiptReply); err != nil {
				return nil, err
			}
		}
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpReply, in.IdempotencyKey,
		threadIdempotencyRef{ThreadID: in.ThreadID, EntryID: entry.ID}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	thread, _ := repository.FindThreadByID(ctx, pool, in.ThreadID)
	return &ThreadReplyResult{Thread: thread, Entry: entry}, nil
}

func BranchThreadState(ctx context.Context, pool *pg.Pool, actorID int64, in ThreadBranchInput) (*ThreadBranchResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	hashInput := struct {
		ParentThreadID int64                   `json:"parent_thread_id"`
		AnchorEntryID  int64                   `json:"anchor_entry_id"`
		InputEntryID   *int64                  `json:"input_entry_id,omitempty"`
		Subject        string                  `json:"subject"`
		Participants   []ThreadParticipantSpec `json:"participants,omitempty"`
		IssueJoinKey   bool                    `json:"issue_join_key,omitempty"`
	}{in.ParentThreadID, in.AnchorEntryID, in.InputEntryID, strings.TrimSpace(in.Subject), in.Participants, in.IssueJoinKey}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpBranch, in.IdempotencyKey, hashInput)
	if err != nil {
		return nil, err
	}
	if !acquired {
		thread, err := repository.FindThreadByID(ctx, tx, replay.ThreadID)
		if err != nil || thread == nil {
			return nil, errors.New("idempotent branch result not found")
		}
		return &ThreadBranchResult{
			Thread: thread, JoinKeyFingerprint: threadKeyHashFingerprint(thread.JoinKeyHash), AlreadyApplied: true,
		}, nil
	}

	if _, err := repository.FindThreadByIDForUpdate(ctx, tx, in.ParentThreadID); err != nil {
		return nil, err
	}
	if _, _, err := requireOpenThreadWriter(ctx, tx, actorID, in.ParentThreadID); err != nil {
		return nil, err
	}
	if in.InputEntryID != nil {
		if *in.InputEntryID != in.AnchorEntryID {
			return nil, errors.New("Todo branch anchor must equal its input entry")
		}
		handled, err := repository.HandlePendingThreadReceipt(ctx, tx, in.ParentThreadID, *in.InputEntryID, actorID)
		if err != nil {
			return nil, err
		}
		if !handled {
			return nil, ErrThreadStaleInput
		}
	}

	child, err := createChildThreadKernel(ctx, tx, actorID, in.ParentThreadID, in.AnchorEntryID, in.Subject)
	if err != nil {
		return nil, err
	}
	for _, p := range in.Participants {
		if p.RoleID == actorID {
			return nil, errors.New("branch creator must not be repeated as an initial participant")
		}
		permission := p.Permission
		if permission == "" {
			permission = model.ThreadWrite
		}
		added, err := addThreadRoleKernel(ctx, tx, actorID, child.ID, p.RoleID, permission, in.AnchorEntryID)
		if err != nil {
			return nil, err
		}
		if !added {
			return nil, errors.New("initial Child participant already exists")
		}
		if permission == model.ThreadWrite || permission == model.ThreadManage {
			ok, err := repository.InsertPendingThreadReceipt(ctx, tx, child.ID, in.AnchorEntryID, p.RoleID, model.ThreadReceiptEntry)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("initial Child participant receipt already exists")
			}
		}
	}

	result := &ThreadBranchResult{Thread: child}
	if in.IssueJoinKey {
		raw, err := resetThreadJoinKeyKernel(ctx, tx, actorID, child.ID, in.AnchorEntryID)
		if err != nil {
			return nil, err
		}
		result.JoinKey = raw
		result.JoinKeyFingerprint = threadKeyFingerprint(raw)
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpBranch, in.IdempotencyKey,
		threadIdempotencyRef{ThreadID: child.ID, Fingerprint: result.JoinKeyFingerprint}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	result.Thread, _ = repository.FindThreadByID(ctx, pool, child.ID)
	return result, nil
}

func HandleThreadInput(ctx context.Context, pool *pg.Pool, actorID, threadID, inputEntryID int64, idempotencyKey string) (bool, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		ThreadID int64 `json:"thread_id"`
		InputID  int64 `json:"input_entry_id"`
	}{threadID, inputEntryID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpHandle, idempotencyKey, request)
	if err != nil {
		return false, err
	}
	if !acquired {
		return replay.Changed, nil
	}
	if _, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID); err != nil {
		return false, err
	}
	if _, _, err := requireOpenThreadWriter(ctx, tx, actorID, threadID); err != nil {
		return false, err
	}
	handled, err := repository.HandlePendingThreadReceipt(ctx, tx, threadID, inputEntryID, actorID)
	if err != nil {
		return false, err
	}
	if !handled {
		return false, ErrThreadStaleInput
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpHandle, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, InputID: inputEntryID, Changed: true}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func AddThreadParticipant(ctx context.Context, pool *pg.Pool, actorID, threadID int64, p ThreadParticipantSpec, entryID int64, idempotencyKey string) (*model.ThreadRole, bool, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	permission := p.Permission
	if permission == "" {
		permission = model.ThreadWrite
	}
	request := struct {
		ThreadID   int64  `json:"thread_id"`
		RoleID     int64  `json:"role_id"`
		Permission string `json:"permission"`
		EntryID    int64  `json:"entry_id"`
	}{threadID, p.RoleID, permission, entryID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpRoleAdd, idempotencyKey, request)
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		role, err := repository.FindThreadRole(ctx, tx, replay.ThreadID, replay.RoleID)
		return role, false, err
	}
	thread, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID)
	if err != nil || thread == nil {
		return nil, false, errors.New("thread not found")
	}
	if thread.Status != model.ThreadOpen {
		return nil, false, errors.New("thread is closed")
	}
	added, err := addThreadRoleKernel(ctx, tx, actorID, threadID, p.RoleID, permission, entryID)
	if err != nil {
		return nil, false, err
	}
	if !added {
		return nil, false, errors.New("participant already exists")
	}
	if permission == model.ThreadWrite || permission == model.ThreadManage {
		ok, err := repository.InsertPendingThreadReceipt(ctx, tx, threadID, entryID, p.RoleID, model.ThreadReceiptEntry)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, errors.New("participant entry receipt already exists")
		}
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpRoleAdd, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, RoleID: p.RoleID, Changed: true}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	role, err := repository.FindThreadRole(ctx, pool, threadID, p.RoleID)
	return role, true, err
}

func JoinThreadState(ctx context.Context, pool *pg.Pool, roleID int64, rawKey, idempotencyKey string) (*ThreadJoinResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		JoinKey string `json:"join_key"`
	}{rawKey}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, roleID, threadOpJoin, idempotencyKey, request)
	if err != nil {
		return nil, err
	}
	if !acquired {
		thread, _ := repository.FindThreadByID(ctx, tx, replay.ThreadID)
		role, err := repository.FindThreadRole(ctx, tx, replay.ThreadID, roleID)
		return &ThreadJoinResult{Thread: thread, Role: role, Joined: replay.Changed, AlreadyApplied: true}, err
	}
	thread, role, joined, err := joinThreadByKeyKernel(ctx, tx, roleID, rawKey)
	if err != nil {
		return nil, err
	}
	if joined {
		ok, err := repository.InsertPendingThreadReceipt(ctx, tx, thread.ID, role.EntryID, roleID, model.ThreadReceiptEntry)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("join entry receipt already exists")
		}
	}
	if err := completeThreadIdempotency(ctx, tx, roleID, threadOpJoin, idempotencyKey,
		threadIdempotencyRef{ThreadID: thread.ID, RoleID: roleID, Changed: joined}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	thread, _ = repository.FindThreadByID(ctx, pool, thread.ID)
	role, err = repository.FindThreadRole(ctx, pool, thread.ID, roleID)
	return &ThreadJoinResult{Thread: thread, Role: role, Joined: joined}, err
}

func ChangeThreadPermission(ctx context.Context, pool *pg.Pool, actorID, threadID, targetRoleID int64, permission string, entryID *int64, idempotencyKey string) (*model.ThreadRole, error) {
	if !validThreadPermission(permission) {
		return nil, errors.New("invalid thread permission")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		ThreadID   int64  `json:"thread_id"`
		RoleID     int64  `json:"role_id"`
		Permission string `json:"permission"`
		EntryID    *int64 `json:"entry_id,omitempty"`
	}{threadID, targetRoleID, permission, entryID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpPermission, idempotencyKey, request)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return repository.FindThreadRole(ctx, tx, replay.ThreadID, replay.RoleID)
	}
	thread, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID)
	if err != nil || thread == nil {
		return nil, errors.New("thread not found")
	}
	if thread.Status != model.ThreadOpen {
		return nil, errors.New("thread is closed")
	}
	if err := requireThreadManageOrGovern(ctx, tx, actorID, threadID); err != nil {
		return nil, err
	}
	current, err := repository.FindThreadRole(ctx, tx, threadID, targetRoleID)
	if err != nil || current == nil {
		if err == nil {
			err = errors.New("participant not found")
		}
		return nil, err
	}
	wasActionable := current.Permission == model.ThreadWrite || current.Permission == model.ThreadManage
	willActionable := permission == model.ThreadWrite || permission == model.ThreadManage
	if entryID != nil && !(!wasActionable && willActionable) && current.EntryID != *entryID {
		return nil, errors.New("entry can only change when permission becomes actionable")
	}
	if current.Permission == permission && (entryID == nil || current.EntryID == *entryID) {
		if err := completeThreadIdempotency(ctx, tx, actorID, threadOpPermission, idempotencyKey,
			threadIdempotencyRef{ThreadID: threadID, RoleID: targetRoleID}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return current, nil
	}

	switch {
	case wasActionable && !willActionable:
		if _, err := repository.WithdrawPendingThreadReceiptsByRole(ctx, tx, threadID, targetRoleID); err != nil {
			return nil, err
		}
		if err := repository.UpdateThreadRolePermission(ctx, tx, threadID, targetRoleID, permission, nil); err != nil {
			return nil, err
		}
	case !wasActionable && willActionable:
		if entryID == nil {
			return nil, errors.New("new entry is required when permission becomes actionable")
		}
		ok, err := repository.EntryAllowedInThreadScope(ctx, tx, threadID, *entryID)
		if err != nil || !ok {
			if err == nil {
				err = errors.New("entry is outside thread scope")
			}
			return nil, err
		}
		if err := repository.UpdateThreadRolePermission(ctx, tx, threadID, targetRoleID, permission, entryID); err != nil {
			return nil, err
		}
		inserted, err := repository.InsertPendingThreadReceipt(ctx, tx, threadID, *entryID, targetRoleID, model.ThreadReceiptEntry)
		if err != nil {
			return nil, err
		}
		if !inserted {
			return nil, errors.New("new actionable entry already has a receipt for this Role")
		}
	default:
		if err := repository.UpdateThreadRolePermission(ctx, tx, threadID, targetRoleID, permission, nil); err != nil {
			return nil, err
		}
	}
	if err := repository.BumpThreadRevision(ctx, tx, threadID); err != nil {
		return nil, err
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpPermission, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, RoleID: targetRoleID, Changed: true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return repository.FindThreadRole(ctx, pool, threadID, targetRoleID)
}

func RemoveThreadParticipant(ctx context.Context, pool *pg.Pool, actorID, threadID, targetRoleID int64, idempotencyKey string) (bool, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		ThreadID int64 `json:"thread_id"`
		RoleID   int64 `json:"role_id"`
	}{threadID, targetRoleID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpRoleRemove, idempotencyKey, request)
	if err != nil {
		return false, err
	}
	if !acquired {
		return replay.Changed, nil
	}
	thread, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID)
	if err != nil || thread == nil {
		return false, errors.New("thread not found")
	}
	if err := requireThreadManageOrGovern(ctx, tx, actorID, threadID); err != nil {
		return false, err
	}
	if role, err := repository.FindThreadRole(ctx, tx, threadID, targetRoleID); err != nil || role == nil {
		if err == nil {
			err = errors.New("participant not found")
		}
		return false, err
	}
	if _, err := repository.WithdrawPendingThreadReceiptsByRole(ctx, tx, threadID, targetRoleID); err != nil {
		return false, err
	}
	if err := repository.DeleteThreadRole(ctx, tx, threadID, targetRoleID); err != nil {
		return false, err
	}
	if _, err := repository.ClearThreadJoinKey(ctx, tx, threadID); err != nil {
		return false, err
	}
	if err := repository.BumpThreadRevision(ctx, tx, threadID); err != nil {
		return false, err
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpRoleRemove, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, RoleID: targetRoleID, Changed: true}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func setThreadOpenState(ctx context.Context, pool *pg.Pool, actorID, threadID int64, open bool, idempotencyKey string) (*model.Thread, error) {
	operation := threadOpClose
	from, to := model.ThreadOpen, model.ThreadClosed
	if open {
		operation = threadOpReopen
		from, to = model.ThreadClosed, model.ThreadOpen
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		ThreadID int64 `json:"thread_id"`
	}{threadID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, operation, idempotencyKey, request)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return repository.FindThreadByID(ctx, tx, replay.ThreadID)
	}
	thread, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID)
	if err != nil || thread == nil {
		return nil, errors.New("thread not found")
	}
	if err := requireThreadManageOrGovern(ctx, tx, actorID, threadID); err != nil {
		return nil, err
	}
	if thread.Status == to {
		if err := completeThreadIdempotency(ctx, tx, actorID, operation, idempotencyKey,
			threadIdempotencyRef{ThreadID: threadID}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return thread, nil
	}
	if thread.Status != from {
		return nil, errors.New("invalid thread status")
	}
	if !open {
		if _, err := repository.WithdrawAllPendingThreadReceipts(ctx, tx, threadID); err != nil {
			return nil, err
		}
	}
	if err := repository.SetThreadStatus(ctx, tx, threadID, from, to); err != nil {
		return nil, err
	}
	if thread.ParentThreadID != nil {
		if err := repository.BumpThreadRevision(ctx, tx, *thread.ParentThreadID); err != nil {
			return nil, err
		}
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, operation, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, Changed: true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return repository.FindThreadByID(ctx, pool, threadID)
}

func CloseThreadState(ctx context.Context, pool *pg.Pool, actorID, threadID int64, idempotencyKey string) (*model.Thread, error) {
	return setThreadOpenState(ctx, pool, actorID, threadID, false, idempotencyKey)
}

func ReopenThreadState(ctx context.Context, pool *pg.Pool, actorID, threadID int64, idempotencyKey string) (*model.Thread, error) {
	return setThreadOpenState(ctx, pool, actorID, threadID, true, idempotencyKey)
}

type ThreadKeyResetResult struct {
	Thread             *model.Thread
	JoinKey            string
	JoinKeyFingerprint string
	AlreadyApplied     bool
}

func ResetThreadJoinKeyState(ctx context.Context, pool *pg.Pool, actorID, threadID, entryID int64, idempotencyKey string) (*ThreadKeyResetResult, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	request := struct {
		ThreadID int64 `json:"thread_id"`
		EntryID  int64 `json:"entry_id"`
	}{threadID, entryID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpKeyReset, idempotencyKey, request)
	if err != nil {
		return nil, err
	}
	if !acquired {
		thread, _ := repository.FindThreadByID(ctx, tx, replay.ThreadID)
		return &ThreadKeyResetResult{
			Thread: thread, JoinKeyFingerprint: threadKeyHashFingerprint(thread.JoinKeyHash), AlreadyApplied: true,
		}, nil
	}
	if _, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID); err != nil {
		return nil, err
	}
	raw, err := resetThreadJoinKeyKernel(ctx, tx, actorID, threadID, entryID)
	if err != nil {
		return nil, err
	}
	fingerprint := threadKeyFingerprint(raw)
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpKeyReset, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, Fingerprint: fingerprint, Changed: true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	thread, _ := repository.FindThreadByID(ctx, pool, threadID)
	return &ThreadKeyResetResult{Thread: thread, JoinKey: raw, JoinKeyFingerprint: fingerprint}, nil
}

func RevokeThreadJoinKeyState(ctx context.Context, pool *pg.Pool, actorID, threadID int64, idempotencyKey string) (bool, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = pg.Rollback(tx) }()
	request := struct {
		ThreadID int64 `json:"thread_id"`
	}{threadID}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpKeyRevoke, idempotencyKey, request)
	if err != nil {
		return false, err
	}
	if !acquired {
		return replay.Changed, nil
	}
	if _, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID); err != nil {
		return false, err
	}
	changed, err := revokeThreadJoinKeyKernel(ctx, tx, actorID, threadID)
	if err != nil {
		return false, err
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpKeyRevoke, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, Changed: changed}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return changed, nil
}

func UpdateThreadSubjectState(ctx context.Context, pool *pg.Pool, actorID, threadID int64, subject, idempotencyKey string) (*model.Thread, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, errors.New("thread subject is required")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()
	request := struct {
		ThreadID int64  `json:"thread_id"`
		Subject  string `json:"subject"`
	}{threadID, subject}
	replay, acquired, err := beginThreadIdempotency(ctx, tx, actorID, threadOpSubject, idempotencyKey, request)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return repository.FindThreadByID(ctx, tx, replay.ThreadID)
	}
	thread, err := repository.FindThreadByIDForUpdate(ctx, tx, threadID)
	if err != nil || thread == nil {
		return nil, errors.New("thread not found")
	}
	if err := requireThreadManageOrGovern(ctx, tx, actorID, threadID); err != nil {
		return nil, err
	}
	changed := thread.Subject != subject
	if changed {
		if err := repository.SetThreadSubject(ctx, tx, threadID, subject); err != nil {
			return nil, err
		}
	}
	if err := completeThreadIdempotency(ctx, tx, actorID, threadOpSubject, idempotencyKey,
		threadIdempotencyRef{ThreadID: threadID, Changed: changed}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return repository.FindThreadByID(ctx, pool, threadID)
}
