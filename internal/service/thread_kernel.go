package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func canonicalRolePair(a, b int64) (int64, int64) {
	if a < b {
		return a, b
	}
	return b, a
}

// resolveRoleExact reuses the existing Agent account identity. bot_name is the
// unique external Role address; no second identity table is introduced.
func resolveRoleExact(ctx context.Context, q pg.Querier, name string) (*model.Bot, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("role name is required")
	}
	role, err := repository.FindActiveBotSummaryByName(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("role not found")
	}
	return role, nil
}

// ---- Link kernel ----------------------------------------------------------

func requestRoleLink(ctx context.Context, pool *pg.Pool, actorID int64, targetName string) (*model.RoleLink, *model.Bot, error) {
	target, err := resolveRoleExact(ctx, pool, targetName)
	if err != nil {
		return nil, nil, err
	}
	if target.ID == actorID {
		return nil, nil, errors.New("cannot link a role to itself")
	}
	low, high := canonicalRolePair(actorID, target.ID)

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	// ON CONFLICT DO NOTHING makes opposite-direction concurrent requests
	// converge on the same canonical row without auto-accepting it.
	if _, err := repository.InsertPendingRoleLink(ctx, tx, low, high, actorID); err != nil {
		return nil, nil, err
	}
	link, err := repository.FindRoleLink(ctx, tx, low, high)
	if err != nil || link == nil {
		if err == nil {
			err = errors.New("role link disappeared")
		}
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return link, target, nil
}

func acceptRoleLink(ctx context.Context, pool *pg.Pool, actorID int64, peerName string) (*model.RoleLink, error) {
	peer, err := resolveRoleExact(ctx, pool, peerName)
	if err != nil {
		return nil, err
	}
	low, high := canonicalRolePair(actorID, peer.ID)

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Rollback(tx) }()

	link, err := repository.FindRoleLinkForUpdate(ctx, tx, low, high)
	if err != nil || link == nil {
		if err == nil {
			err = errors.New("role link not found")
		}
		return nil, err
	}
	if link.Status != model.RoleLinkPending || link.RequestedByRoleID == actorID {
		return nil, errors.New("role link is not an incoming pending request")
	}
	if err := repository.ActivateRoleLink(ctx, tx, low, high); err != nil {
		return nil, err
	}
	link, err = repository.FindRoleLink(ctx, tx, low, high)
	if err != nil || link == nil {
		if err == nil {
			err = errors.New("role link disappeared")
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return link, nil
}

func declineRoleLink(ctx context.Context, pool *pg.Pool, actorID int64, peerName string) error {
	return deleteRoleLinkWithRule(ctx, pool, actorID, peerName, func(link *model.RoleLink) bool {
		return link.Status == model.RoleLinkPending && link.RequestedByRoleID != actorID
	})
}

func cancelRoleLink(ctx context.Context, pool *pg.Pool, actorID int64, peerName string) error {
	return deleteRoleLinkWithRule(ctx, pool, actorID, peerName, func(link *model.RoleLink) bool {
		return link.Status == model.RoleLinkPending && link.RequestedByRoleID == actorID
	})
}

func removeRoleLink(ctx context.Context, pool *pg.Pool, actorID int64, peerName string) error {
	return deleteRoleLinkWithRule(ctx, pool, actorID, peerName, func(link *model.RoleLink) bool {
		return link.Status == model.RoleLinkActive
	})
}

func deleteRoleLinkWithRule(ctx context.Context, pool *pg.Pool, actorID int64, peerName string, allowed func(*model.RoleLink) bool) error {
	peer, err := resolveRoleExact(ctx, pool, peerName)
	if err != nil {
		return err
	}
	low, high := canonicalRolePair(actorID, peer.ID)

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Rollback(tx) }()

	link, err := repository.FindRoleLinkForUpdate(ctx, tx, low, high)
	if err != nil || link == nil {
		if err == nil {
			err = errors.New("role link not found")
		}
		return err
	}
	if !allowed(link) {
		return errors.New("role link action is not allowed in its current state")
	}
	if err := repository.DeleteRoleLink(ctx, tx, low, high); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func rolesHaveActiveLink(ctx context.Context, q pg.Querier, a, b int64) (bool, error) {
	if a == b {
		return false, nil
	}
	low, high := canonicalRolePair(a, b)
	link, err := repository.FindRoleLink(ctx, q, low, high)
	if err != nil || link == nil {
		return false, err
	}
	return link.Status == model.RoleLinkActive, nil
}

// ---- Thread structural kernel --------------------------------------------

func validThreadPermission(permission string) bool {
	switch permission {
	case model.ThreadRead, model.ThreadWrite, model.ThreadManage:
		return true
	default:
		return false
	}
}

func threadRoleCanWrite(role *model.ThreadRole) bool {
	return role != nil && (role.Permission == model.ThreadWrite || role.Permission == model.ThreadManage)
}

func createRootThreadKernel(ctx context.Context, tx pgx.Tx, creatorID int64, subject, rootContent string) (*model.Thread, *model.ThreadMemory, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, nil, errors.New("thread subject is required")
	}
	creator, err := repository.FindActiveBotAccountByID(ctx, tx, creatorID)
	if err != nil || creator == nil {
		if err == nil {
			err = errors.New("creator role not found")
		}
		return nil, nil, err
	}
	code, err := repository.GenerateUniqueThreadCode(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	threadID, err := repository.InsertThread(ctx, tx, code, subject, creatorID, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	memory, err := persistThreadMemory(ctx, tx, creatorID, "", nil, "", rootContent)
	if err != nil {
		return nil, nil, err
	}
	seq, _, err := repository.AllocateThreadSeq(ctx, tx, threadID)
	if err != nil {
		return nil, nil, err
	}
	entry, err := repository.InsertThreadMemory(ctx, tx, threadID, memory.ID, memory.Revision, seq, creatorID, nil)
	if err != nil {
		return nil, nil, err
	}
	if _, err := repository.InsertThreadRole(ctx, tx, threadID, creatorID, model.ThreadManage, nil, entry.ID); err != nil {
		return nil, nil, err
	}
	thread, err := repository.FindThreadByID(ctx, tx, threadID)
	if err != nil || thread == nil {
		if err == nil {
			err = errors.New("created thread not found")
		}
		return nil, nil, err
	}
	return thread, entry, nil
}

// addThreadRoleKernel establishes participation only. Receipt/Todo effects are
// deliberately left to T3. Root creator inherited governance may manage a
// descendant without becoming its participant.
func addThreadRoleKernel(ctx context.Context, q pg.Querier, actorID, threadID, targetRoleID int64, permission string, entryID int64) (bool, error) {
	if !validThreadPermission(permission) {
		return false, errors.New("invalid thread permission")
	}
	target, err := repository.FindActiveBotAccountByID(ctx, q, targetRoleID)
	if err != nil || target == nil {
		if err == nil {
			err = errors.New("target role not found")
		}
		return false, err
	}
	linked, err := rolesHaveActiveLink(ctx, q, actorID, targetRoleID)
	if err != nil {
		return false, err
	}
	if !linked {
		return false, errors.New("active Role Link required for direct participant add")
	}

	actorRole, err := repository.FindThreadRole(ctx, q, threadID, actorID)
	if err != nil {
		return false, err
	}
	canGovern, err := repository.RootCreatorCanGovernThread(ctx, q, threadID, actorID)
	if err != nil {
		return false, err
	}
	if (actorRole == nil || actorRole.Permission != model.ThreadManage) && !canGovern {
		return false, errors.New("thread manage permission required")
	}

	allowed, err := repository.EntryAllowedInThreadScope(ctx, q, threadID, entryID)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, errors.New("entry is outside thread scope")
	}
	joinedBy := actorID
	inserted, err := repository.InsertThreadRole(ctx, q, threadID, targetRoleID, permission, &joinedBy, entryID)
	if err != nil || !inserted {
		return inserted, err
	}
	if err := repository.BumpThreadRevision(ctx, q, threadID); err != nil {
		return false, err
	}
	return true, nil
}

func newThreadJoinKey() (raw string, hash []byte, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// resetThreadJoinKeyKernel replaces the bearer join capability and binds it
// to one exact collaboration entry. Only the safe hash is persisted.
func resetThreadJoinKeyKernel(ctx context.Context, q pg.Querier, actorID, threadID, entryID int64) (string, error) {
	thread, err := repository.FindThreadByID(ctx, q, threadID)
	if err != nil || thread == nil {
		if err == nil {
			err = errors.New("thread not found")
		}
		return "", err
	}
	if thread.Status != model.ThreadOpen {
		return "", errors.New("thread is closed")
	}
	actorRole, err := repository.FindThreadRole(ctx, q, threadID, actorID)
	if err != nil {
		return "", err
	}
	canGovern, err := repository.RootCreatorCanGovernThread(ctx, q, threadID, actorID)
	if err != nil {
		return "", err
	}
	if (actorRole == nil || actorRole.Permission != model.ThreadManage) && !canGovern {
		return "", errors.New("thread manage permission required")
	}
	allowed, err := repository.EntryAllowedInThreadScope(ctx, q, threadID, entryID)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", errors.New("join entry is outside thread scope")
	}
	raw, hash, err := newThreadJoinKey()
	if err != nil {
		return "", err
	}
	if err := repository.SetThreadJoinKey(ctx, q, threadID, hash, entryID); err != nil {
		return "", err
	}
	return raw, nil
}

func revokeThreadJoinKeyKernel(ctx context.Context, q pg.Querier, actorID, threadID int64) (bool, error) {
	actorRole, err := repository.FindThreadRole(ctx, q, threadID, actorID)
	if err != nil {
		return false, err
	}
	canGovern, err := repository.RootCreatorCanGovernThread(ctx, q, threadID, actorID)
	if err != nil {
		return false, err
	}
	if (actorRole == nil || actorRole.Permission != model.ThreadManage) && !canGovern {
		return false, errors.New("thread manage permission required")
	}
	return repository.ClearThreadJoinKey(ctx, q, threadID)
}

// joinThreadByKeyKernel establishes only the structural write membership.
// T3 composes the entry receipt in the same outer transaction. Reusing a key
// as an existing participant preserves the current permission and entry.
func joinThreadByKeyKernel(ctx context.Context, q pg.Querier, roleID int64, rawKey string) (*model.Thread, *model.ThreadRole, bool, error) {
	sum := sha256.Sum256([]byte(rawKey))
	thread, err := repository.FindOpenThreadByJoinKeyHashForUpdate(ctx, q, sum[:])
	if err != nil || thread == nil || thread.JoinEntryID == nil {
		if err == nil {
			err = errors.New("join key is invalid")
		}
		return nil, nil, false, err
	}
	role, err := repository.FindActiveBotAccountByID(ctx, q, roleID)
	if err != nil || role == nil {
		if err == nil {
			err = errors.New("role not found")
		}
		return nil, nil, false, err
	}
	existing, err := repository.FindThreadRole(ctx, q, thread.ID, roleID)
	if err != nil {
		return nil, nil, false, err
	}
	if existing != nil {
		return thread, existing, false, nil
	}
	inserted, err := repository.InsertThreadRole(ctx, q, thread.ID, roleID, model.ThreadWrite, nil, *thread.JoinEntryID)
	if err != nil {
		return nil, nil, false, err
	}
	if !inserted {
		existing, err = repository.FindThreadRole(ctx, q, thread.ID, roleID)
		if err != nil || existing == nil {
			if err == nil {
				err = errors.New("thread membership changed concurrently")
			}
			return nil, nil, false, err
		}
		return thread, existing, false, nil
	}
	if err := repository.BumpThreadRevision(ctx, q, thread.ID); err != nil {
		return nil, nil, false, err
	}
	joined, err := repository.FindThreadRole(ctx, q, thread.ID, roleID)
	if err != nil || joined == nil {
		if err == nil {
			err = errors.New("joined ThreadRole not found")
		}
		return nil, nil, false, err
	}
	return thread, joined, true, nil
}

// createChildThreadKernel creates only the structural Child and its creator
// participation. Receipt/Todo generation and input consumption belong to T3.
func createChildThreadKernel(ctx context.Context, tx pgx.Tx, actorID, parentThreadID, anchorEntryID int64, subject string) (*model.Thread, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, errors.New("thread subject is required")
	}
	parent, err := repository.FindThreadByID(ctx, tx, parentThreadID)
	if err != nil || parent == nil {
		if err == nil {
			err = errors.New("parent thread not found")
		}
		return nil, err
	}
	if parent.Status != model.ThreadOpen {
		return nil, errors.New("parent thread is closed")
	}
	actorRole, err := repository.FindThreadRole(ctx, tx, parentThreadID, actorID)
	if err != nil {
		return nil, err
	}
	if !threadRoleCanWrite(actorRole) {
		return nil, errors.New("current parent ThreadRole cannot branch")
	}
	belongs, err := repository.EntryBelongsToThread(ctx, tx, parentThreadID, anchorEntryID)
	if err != nil {
		return nil, err
	}
	if !belongs {
		return nil, errors.New("child anchor must belong to the direct parent timeline")
	}

	code, err := repository.GenerateUniqueThreadCode(ctx, tx)
	if err != nil {
		return nil, err
	}
	parentID, anchorID := parentThreadID, anchorEntryID
	childID, err := repository.InsertThread(ctx, tx, code, subject, actorID, &parentID, &anchorID)
	if err != nil {
		return nil, err
	}
	if _, err := repository.InsertThreadRole(ctx, tx, childID, actorID, model.ThreadManage, nil, anchorEntryID); err != nil {
		return nil, err
	}
	if err := repository.BumpThreadRevision(ctx, tx, parentThreadID); err != nil {
		return nil, err
	}
	child, err := repository.FindThreadByID(ctx, tx, childID)
	if err != nil || child == nil {
		if err == nil {
			err = errors.New("created child thread not found")
		}
		return nil, err
	}
	return child, nil
}

// appendThreadMemoryKernel is the timeline persistence primitive. It validates
// real participant write authority and allowed reply scope, but intentionally
// has no Receipt/Todo effects; T3 composes those atomically around it.
func appendThreadMemoryKernel(ctx context.Context, tx pgx.Tx, threadID, authorRoleID int64, content string, replyToEntryID *int64) (*model.ThreadMemory, error) {
	thread, err := repository.FindThreadByID(ctx, tx, threadID)
	if err != nil || thread == nil {
		if err == nil {
			err = errors.New("thread not found")
		}
		return nil, err
	}
	if thread.Status != model.ThreadOpen {
		return nil, errors.New("thread is closed")
	}
	role, err := repository.FindThreadRole(ctx, tx, threadID, authorRoleID)
	if err != nil {
		return nil, err
	}
	if !threadRoleCanWrite(role) {
		return nil, errors.New("current ThreadRole cannot write")
	}
	if replyToEntryID != nil {
		ok, err := repository.EntryAllowedInThreadScope(ctx, tx, threadID, *replyToEntryID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("reply target is outside thread scope")
		}
	}

	memory, err := persistThreadMemory(ctx, tx, authorRoleID, "", nil, "", content)
	if err != nil {
		return nil, err
	}
	seq, _, err := repository.AllocateThreadSeq(ctx, tx, threadID)
	if err != nil {
		return nil, err
	}
	return repository.InsertThreadMemory(ctx, tx, threadID, memory.ID, memory.Revision, seq, authorRoleID, replyToEntryID)
}
