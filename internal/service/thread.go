package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
	threaddomain "kungfu.md/internal/thread"
)

const (
	threadTitleMax       = 160
	threadObjectiveMax   = 20000
	threadMessageMax     = 20000
	threadDeliveryMax    = 100000
	threadDeliveryTitleMax = 160
	threadActionMax      = 1000
	threadReviewNoteMax  = 2000
	threadListMax        = 100
	threadUpdateMax      = 50
)

type ThreadCreateInput struct {
	Title      string
	Objective  string
	NextAction string
}

type ThreadListFilter struct {
	Status   string
	Page     int
	PageSize int
}

func (f *ThreadListFilter) Normalize() {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 20
	}
	if f.PageSize > threadListMax {
		f.PageSize = threadListMax
	}
}

func CreateThread(ctx context.Context, pool *pg.Pool, actorID int64, in ThreadCreateInput) (map[string]interface{}, error) {
	title := strings.TrimSpace(in.Title)
	objective := strings.TrimSpace(in.Objective)
	nextAction := strings.TrimSpace(in.NextAction)
	if title == "" {
		return nil, validationError("title", "title is required")
	}
	if utf8.RuneCountInString(title) > threadTitleMax {
		return nil, validationError("title", "title maximum 160 characters")
	}
	if utf8.RuneCountInString(objective) > threadObjectiveMax {
		return nil, validationError("objective", "objective maximum 20000 characters")
	}
	if utf8.RuneCountInString(nextAction) > threadActionMax {
		return nil, validationError("next_action", "next_action maximum 1000 characters")
	}
	if security.ContainsCredentialValue(map[string]interface{}{
		"title": title, "objective": objective, "next_action": nextAction,
	}) {
		return nil, apperr.New(422, "SENSITIVE_CONTENT", "thread content must not contain credential-shaped strings")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("create thread")
	}
	defer func() { _ = pg.Rollback(tx) }()

	code, err := repository.GenerateUniqueThreadCode(ctx, tx)
	if err != nil {
		return nil, threadInternal("create thread")
	}
	threadID, err := repository.InsertThread(ctx, tx, code, actorID, title, objective)
	if err != nil {
		return nil, threadInternal("create thread")
	}
	if err := repository.InsertOwnerMembership(ctx, tx, threadID, actorID); err != nil {
		return nil, threadInternal("create thread")
	}
	if nextAction != "" {
		if err := repository.UpdateThreadHandoff(ctx, tx, threadID, &actorID, &nextAction); err != nil {
			return nil, threadInternal("create thread")
		}
	}
	payload := map[string]interface{}{"title": title}
	if objective != "" {
		payload["objective"] = objective
	}
	if nextAction != "" {
		payload["next_actor"] = actorID
		payload["next_action"] = nextAction
	}
	if _, err := insertThreadEvent(ctx, tx, threadID, &actorID, "thread.created", payload); err != nil {
		return nil, threadInternal("create thread")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("create thread")
	}
	threadLog(ctx, pool, actorID, "thread_create", code, map[string]interface{}{"title": title})
	return GetThread(ctx, pool, actorID, code)
}

func ListThreads(ctx context.Context, q pg.Querier, actorID int64, filter ThreadListFilter) (map[string]interface{}, error) {
	filter.Normalize()
	if filter.Status != "" && filter.Status != threaddomain.StatusActive && filter.Status != threaddomain.StatusClosed {
		return nil, validationError("status", "status must be active or closed")
	}
	offset := (filter.Page - 1) * filter.PageSize
	total, err := repository.CountThreadsForBot(ctx, q, actorID, filter.Status)
	if err != nil {
		return nil, threadInternal("list threads")
	}
	rows, err := repository.ListThreadsForBot(ctx, q, actorID, filter.Status, filter.PageSize, offset)
	if err != nil {
		return nil, threadInternal("list threads")
	}
	items := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		items = append(items, threadSummary(&rows[i]))
	}
	return map[string]interface{}{
		"threads": items,
		"total": total,
		"page": filter.Page,
		"page_size": filter.PageSize,
	}, nil
}

func GetThread(ctx context.Context, q pg.Querier, actorID int64, code string) (map[string]interface{}, error) {
	t, member, err := requireThreadMember(ctx, q, actorID, code)
	if err != nil {
		return nil, err
	}
	members, err := repository.ListActiveThreadMembers(ctx, q, t.ID)
	if err != nil {
		return nil, threadInternal("get thread")
	}
	messages, err := repository.ListRecentThreadMessages(ctx, q, t.ID, 50)
	if err != nil {
		return nil, threadInternal("get thread")
	}
	deliveries, err := repository.ListRecentThreadDeliveries(ctx, q, t.ID, 20)
	if err != nil {
		return nil, threadInternal("get thread")
	}
	cursor, err := repository.MaxThreadEventID(ctx, q, t.ID)
	if err != nil {
		return nil, threadInternal("get thread")
	}

	out := threadSummary(t)
	out["my_role"] = member.Role
	out["participants"] = memberViews(members)
	out["messages"] = messageViews(messages)
	out["deliveries"] = deliveryViews(deliveries)
	out["cursor"] = cursor
	return out, nil
}

func GetThreadUpdates(ctx context.Context, q pg.Querier, actorID int64, code string, cursor int64, limit int) (map[string]interface{}, error) {
	if cursor < 0 {
		return nil, validationError("cursor", "cursor must be >= 0")
	}
	if limit == 0 {
		limit = 30
	}
	if limit < 1 {
		limit = 1
	}
	if limit > threadUpdateMax {
		limit = threadUpdateMax
	}
	t, _, err := requireThreadMember(ctx, q, actorID, code)
	if err != nil {
		return nil, err
	}
	rows, err := repository.ListThreadEventsAfter(ctx, q, t.ID, cursor, limit+1)
	if err != nil {
		return nil, threadInternal("get thread updates")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	events := make([]map[string]interface{}, 0, len(rows))
	nextCursor := cursor
	for i := range rows {
		events = append(events, eventView(&rows[i]))
		nextCursor = rows[i].ID
	}
	currentCursor, err := repository.MaxThreadEventID(ctx, q, t.ID)
	if err != nil {
		return nil, threadInternal("get thread updates")
	}
	return map[string]interface{}{
		"code": code,
		"state": map[string]interface{}{
			"status": t.Status,
			"next_actor": participantRef(t.NextActorID, t.NextActorName),
			"next_action": t.NextAction,
		},
		"events": events,
		"next_cursor": nextCursor,
		"current_cursor": currentCursor,
		"has_more": hasMore,
	}, nil
}

func InviteThreadParticipant(ctx context.Context, pool *pg.Pool, actorID int64, code, participant string, expiresInHours int) (map[string]interface{}, error) {
	participant = strings.TrimSpace(participant)
	if !validThreadParticipantName(participant) {
		return nil, validationError("participant", "participant must be 6-32 characters using only letters, digits, _ . -")
	}
	if expiresInHours == 0 {
		expiresInHours = 168
	}
	if expiresInHours < 1 || expiresInHours > 720 {
		return nil, validationError("expires_in_hours", "expires_in_hours must be between 1 and 720")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("invite participant")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, _, err := requireThreadOwner(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	if existing, err := repository.FindActiveThreadMemberByName(ctx, tx, t.ID, participant); err != nil {
		return nil, threadInternal("invite participant")
	} else if existing != nil {
		return nil, apperr.New(409, "THREAD_MEMBER_EXISTS", "participant is already in the thread")
	}

	token, tokenHash, err := threaddomain.NewInviteToken()
	if err != nil {
		return nil, threadInternal("invite participant")
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresInHours) * time.Hour)
	inviteID, createdAt, err := repository.InsertThreadInvite(ctx, tx, t.ID, actorID, tokenHash, &participant, expiresAt)
	if err != nil {
		return nil, threadInternal("invite participant")
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "participant.invited", map[string]interface{}{
		"invite_id": inviteID, "participant": participant, "expires_at": expiresAt.UTC().Format(time.RFC3339),
	}); err != nil {
		return nil, threadInternal("invite participant")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("invite participant")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("invite participant")
	}
	threadLog(ctx, pool, actorID, "thread_invite", code, map[string]interface{}{"participant": participant})
	return map[string]interface{}{
		"code": code,
		"invite_id": inviteID,
		"participant": participant,
		"invite_token": token,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"created_at": createdAt.UTC().Format(time.RFC3339),
		"warning": "The invite token is shown once. Give it only to the invited participant.",
	}, nil
}

func RevokeThreadInvite(ctx context.Context, pool *pg.Pool, actorID int64, code string, inviteID int64) (map[string]interface{}, error) {
	if inviteID <= 0 {
		return nil, validationError("invite_id", "invite_id must be positive")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("revoke invite")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, _, err := requireThreadOwner(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	changed, err := repository.RevokeThreadInvite(ctx, tx, t.ID, inviteID)
	if err != nil {
		return nil, threadInternal("revoke invite")
	}
	if !changed {
		return nil, apperr.New(404, "THREAD_INVITE_NOT_FOUND", "active invite not found")
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "participant.invite_revoked", map[string]interface{}{"invite_id": inviteID}); err != nil {
		return nil, threadInternal("revoke invite")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("revoke invite")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("revoke invite")
	}
	threadLog(ctx, pool, actorID, "thread_invite_revoke", code, map[string]interface{}{"invite_id": inviteID})
	return map[string]interface{}{"code": code, "invite_id": inviteID, "revoked": true}, nil
}

func JoinThread(ctx context.Context, pool *pg.Pool, actorID int64, actorName, token string) (map[string]interface{}, error) {
	tokenHash, err := threaddomain.HashInviteToken(token)
	if err != nil {
		return nil, invalidThreadInvite()
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("join thread")
	}
	defer func() { _ = pg.Rollback(tx) }()

	invite, err := repository.FindThreadInviteByHashForUpdate(ctx, tx, tokenHash)
	if err != nil {
		return nil, threadInternal("join thread")
	}
	if invite == nil || invite.RevokedAt != nil || time.Now().UTC().After(invite.ExpiresAt) ||
		invite.ThreadStatus != threaddomain.StatusActive {
		return nil, invalidThreadInvite()
	}
	if invite.InviteeName == nil || *invite.InviteeName != actorName {
		return nil, invalidThreadInvite()
	}
	if invite.AcceptedAt != nil {
		if invite.AcceptedByID != nil && *invite.AcceptedByID == actorID {
			t, err := findThreadByIDCode(ctx, tx, invite.ThreadID)
			if err != nil {
				return nil, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, threadInternal("join thread")
			}
			return GetThread(ctx, pool, actorID, t.Code)
		}
		return nil, invalidThreadInvite()
	}
	if err := repository.UpsertThreadMember(ctx, tx, invite.ThreadID, actorID); err != nil {
		return nil, threadInternal("join thread")
	}
	if err := repository.MarkThreadInviteAccepted(ctx, tx, invite.ID, actorID); err != nil {
		return nil, threadInternal("join thread")
	}
	t, err := findThreadByIDCode(ctx, tx, invite.ThreadID)
	if err != nil {
		return nil, err
	}
	if _, err := insertThreadEvent(ctx, tx, invite.ThreadID, &actorID, "participant.joined", map[string]interface{}{"participant": actorName}); err != nil {
		return nil, threadInternal("join thread")
	}
	if err := repository.TouchThread(ctx, tx, invite.ThreadID); err != nil {
		return nil, threadInternal("join thread")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("join thread")
	}
	threadLog(ctx, pool, actorID, "thread_join", t.Code, nil)
	return GetThread(ctx, pool, actorID, t.Code)
}

func RemoveThreadParticipant(ctx context.Context, pool *pg.Pool, actorID int64, code, participant string) (map[string]interface{}, error) {
	participant = strings.TrimSpace(participant)
	if participant == "" {
		return nil, validationError("participant", "participant is required")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("remove participant")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, _, err := requireThreadOwner(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	target, err := repository.FindActiveThreadMemberByName(ctx, tx, t.ID, participant)
	if err != nil {
		return nil, threadInternal("remove participant")
	}
	if target == nil {
		return nil, apperr.New(404, "THREAD_MEMBER_NOT_FOUND", "participant not found")
	}
	if target.Role == threaddomain.RoleOwner {
		return nil, apperr.New(409, "THREAD_OWNER_REQUIRED", "the thread owner cannot be removed")
	}
	changed, err := repository.RemoveThreadMember(ctx, tx, t.ID, target.BotID)
	if err != nil || !changed {
		return nil, threadInternal("remove participant")
	}
	if t.NextActorID != nil && *t.NextActorID == target.BotID {
		if err := repository.ClearThreadHandoffIfActor(ctx, tx, t.ID, target.BotID); err != nil {
			return nil, threadInternal("remove participant")
		}
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "participant.removed", map[string]interface{}{"participant": participant}); err != nil {
		return nil, threadInternal("remove participant")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("remove participant")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("remove participant")
	}
	threadLog(ctx, pool, actorID, "thread_remove_member", code, map[string]interface{}{"participant": participant})
	return map[string]interface{}{"code": code, "participant": participant, "removed": true}, nil
}

func AddThreadMessage(ctx context.Context, pool *pg.Pool, actorID int64, code, body string) (map[string]interface{}, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, validationError("body", "body is required")
	}
	if utf8.RuneCountInString(body) > threadMessageMax {
		return nil, validationError("body", "body maximum 20000 characters")
	}
	if security.ContainsCredential(body) {
		return nil, apperr.New(422, "SENSITIVE_CONTENT", "thread messages must not contain credential-shaped strings")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("add message")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, member, err := requireThreadMember(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	id, createdAt, err := repository.InsertThreadMessage(ctx, tx, t.ID, actorID, body)
	if err != nil {
		return nil, threadInternal("add message")
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "message.added", map[string]interface{}{
		"message_id": id, "author": member.BotName, "body": body,
	}); err != nil {
		return nil, threadInternal("add message")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("add message")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("add message")
	}
	return map[string]interface{}{
		"code": code, "message_id": id, "author": member.BotName,
		"body": body, "created_at": createdAt.UTC().Format(time.RFC3339),
	}, nil
}

func SubmitThreadDelivery(ctx context.Context, pool *pg.Pool, actorID int64, code, title, body string, revisesID *int64) (map[string]interface{}, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" {
		return nil, validationError("title", "title is required")
	}
	if body == "" {
		return nil, validationError("body", "body is required")
	}
	if utf8.RuneCountInString(title) > threadDeliveryTitleMax {
		return nil, validationError("title", "title maximum 160 characters")
	}
	if utf8.RuneCountInString(body) > threadDeliveryMax {
		return nil, validationError("body", "body maximum 100000 characters")
	}
	if security.ContainsCredentialValue(map[string]interface{}{"title": title, "body": body}) {
		return nil, apperr.New(422, "SENSITIVE_CONTENT", "thread delivery must not contain credential-shaped strings")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("submit delivery")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, member, err := requireThreadMember(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	if revisesID != nil {
		if *revisesID <= 0 {
			return nil, validationError("revises", "revises must be positive")
		}
		prior, err := repository.FindThreadDeliveryForUpdate(ctx, tx, t.ID, *revisesID)
		if err != nil {
			return nil, threadInternal("submit delivery")
		}
		if prior == nil || prior.AuthorID != actorID || prior.Status != threaddomain.DeliveryRejected {
			return nil, apperr.New(422, "INVALID_REVISES", "revises must identify your rejected delivery in this thread")
		}
	}
	id, createdAt, err := repository.InsertThreadDelivery(ctx, tx, t.ID, actorID, title, body, revisesID)
	if err != nil {
		return nil, threadInternal("submit delivery")
	}
	payload := map[string]interface{}{
		"delivery_id": id, "author": member.BotName, "title": title, "status": threaddomain.DeliverySubmitted,
	}
	if revisesID != nil {
		payload["revises"] = *revisesID
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "delivery.submitted", payload); err != nil {
		return nil, threadInternal("submit delivery")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("submit delivery")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("submit delivery")
	}
	return map[string]interface{}{
		"code": code, "delivery_id": id, "author": member.BotName,
		"title": title, "body": body, "status": threaddomain.DeliverySubmitted,
		"revises": revisesID, "created_at": createdAt.UTC().Format(time.RFC3339),
	}, nil
}

func ReviewThreadDelivery(ctx context.Context, pool *pg.Pool, actorID int64, code string, deliveryID int64, decision, note string) (map[string]interface{}, error) {
	if deliveryID <= 0 {
		return nil, validationError("delivery_id", "delivery_id must be positive")
	}
	decision = strings.TrimSpace(decision)
	if decision != threaddomain.DeliveryAccepted && decision != threaddomain.DeliveryRejected {
		return nil, validationError("decision", "decision must be accepted or rejected")
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > threadReviewNoteMax {
		return nil, validationError("note", "note maximum 2000 characters")
	}
	if security.ContainsCredential(note) {
		return nil, apperr.New(422, "SENSITIVE_CONTENT", "review note must not contain credential-shaped strings")
	}
	var notePtr *string
	if note != "" {
		notePtr = &note
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("review delivery")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, owner, err := requireThreadOwner(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	delivery, err := repository.FindThreadDeliveryForUpdate(ctx, tx, t.ID, deliveryID)
	if err != nil {
		return nil, threadInternal("review delivery")
	}
	if delivery == nil {
		return nil, apperr.New(404, "THREAD_DELIVERY_NOT_FOUND", "delivery not found")
	}
	if delivery.Status != threaddomain.DeliverySubmitted {
		return nil, apperr.NewWithDetails(409, "THREAD_DELIVERY_FINAL", "delivery was already reviewed", map[string]interface{}{"status": delivery.Status})
	}
	if err := repository.ReviewThreadDelivery(ctx, tx, deliveryID, actorID, decision, notePtr); err != nil {
		return nil, threadInternal("review delivery")
	}
	payload := map[string]interface{}{"delivery_id": deliveryID, "decision": decision, "reviewer": owner.BotName}
	if note != "" {
		payload["note"] = note
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "delivery."+decision, payload); err != nil {
		return nil, threadInternal("review delivery")
	}
	if err := repository.TouchThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("review delivery")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("review delivery")
	}
	return map[string]interface{}{"code": code, "delivery_id": deliveryID, "status": decision, "review_note": notePtr}, nil
}

func HandoffThread(ctx context.Context, pool *pg.Pool, actorID int64, code, participant, action string) (map[string]interface{}, error) {
	participant = strings.TrimSpace(participant)
	action = strings.TrimSpace(action)
	if participant == "" {
		return nil, validationError("participant", "participant is required")
	}
	if action == "" {
		return nil, validationError("next_action", "next_action is required")
	}
	if utf8.RuneCountInString(action) > threadActionMax {
		return nil, validationError("next_action", "next_action maximum 1000 characters")
	}
	if security.ContainsCredential(action) {
		return nil, apperr.New(422, "SENSITIVE_CONTENT", "next_action must not contain credential-shaped strings")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("handoff thread")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, member, err := requireThreadMember(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != threaddomain.StatusActive {
		return nil, threadClosed()
	}
	if member.Role != threaddomain.RoleOwner && (t.NextActorID == nil || *t.NextActorID != actorID) {
		return nil, apperr.New(409, "THREAD_NOT_CURRENT_ACTOR", "only the owner or current next actor can hand off the thread")
	}
	target, err := repository.FindActiveThreadMemberByName(ctx, tx, t.ID, participant)
	if err != nil {
		return nil, threadInternal("handoff thread")
	}
	if target == nil {
		return nil, apperr.New(404, "THREAD_MEMBER_NOT_FOUND", "participant not found")
	}
	if err := repository.UpdateThreadHandoff(ctx, tx, t.ID, &target.BotID, &action); err != nil {
		return nil, threadInternal("handoff thread")
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "thread.handoff", map[string]interface{}{
		"from": member.BotName, "to": target.BotName, "next_action": action,
	}); err != nil {
		return nil, threadInternal("handoff thread")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("handoff thread")
	}
	threadLog(ctx, pool, actorID, "thread_handoff", code, map[string]interface{}{"to": target.BotName})
	return map[string]interface{}{
		"code": code,
		"next_actor": map[string]interface{}{"bot_id": target.BotID, "bot_name": target.BotName},
		"next_action": action,
	}, nil
}

func CloseThread(ctx context.Context, pool *pg.Pool, actorID int64, code string) (map[string]interface{}, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, threadInternal("close thread")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, _, err := requireThreadOwner(ctx, tx, actorID, code)
	if err != nil {
		return nil, err
	}
	if t.Status == threaddomain.StatusClosed {
		if err := tx.Commit(ctx); err != nil {
			return nil, threadInternal("close thread")
		}
		return GetThread(ctx, pool, actorID, code)
	}
	if err := repository.CloseThread(ctx, tx, t.ID); err != nil {
		return nil, threadInternal("close thread")
	}
	if _, err := insertThreadEvent(ctx, tx, t.ID, &actorID, "thread.closed", map[string]interface{}{}); err != nil {
		return nil, threadInternal("close thread")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, threadInternal("close thread")
	}
	threadLog(ctx, pool, actorID, "thread_close", code, nil)
	return GetThread(ctx, pool, actorID, code)
}

func requireThreadMember(ctx context.Context, q pg.Querier, actorID int64, code string) (*repository.ThreadRow, *repository.ThreadMemberRow, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	t, err := repository.FindThreadByCode(ctx, q, code)
	if err != nil {
		return nil, nil, threadInternal("read thread")
	}
	if t == nil {
		return nil, nil, threadNotFound()
	}
	member, err := repository.FindActiveThreadMembership(ctx, q, t.ID, actorID)
	if err != nil {
		return nil, nil, threadInternal("read thread")
	}
	if member == nil {
		// Deliberately indistinguishable from a missing thread: thread
		// existence is part of the isolation boundary.
		return nil, nil, threadNotFound()
	}
	return t, member, nil
}

func requireThreadOwner(ctx context.Context, q pg.Querier, actorID int64, code string) (*repository.ThreadRow, *repository.ThreadMemberRow, error) {
	t, member, err := requireThreadMember(ctx, q, actorID, code)
	if err != nil {
		return nil, nil, err
	}
	if member.Role != threaddomain.RoleOwner || t.OwnerID != actorID {
		return nil, nil, apperr.New(403, "THREAD_NOT_OWNER", "only the thread owner can perform this action")
	}
	return t, member, nil
}

func findThreadByIDCode(ctx context.Context, q pg.Querier, threadID int64) (*repository.ThreadRow, error) {
	// Invite rows intentionally do not expose the thread code. Resolve
	// it only after a valid, target-matching invite has authenticated
	// the caller to the thread.
	var code string
	if err := q.QueryRow(ctx, `SELECT code FROM tb_thread WHERE id = $1`, threadID).Scan(&code); err != nil {
		return nil, threadInternal("join thread")
	}
	t, err := repository.FindThreadByCode(ctx, q, code)
	if err != nil || t == nil {
		return nil, threadInternal("join thread")
	}
	return t, nil
}

func insertThreadEvent(ctx context.Context, q pg.Querier, threadID int64, actorID *int64, eventType string, payload map[string]interface{}) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	return repository.InsertThreadEvent(ctx, q, threadID, actorID, eventType, string(raw))
}

func threadSummary(t *repository.ThreadRow) map[string]interface{} {
	return map[string]interface{}{
		"code": t.Code,
		"title": t.Title,
		"objective": t.Objective,
		"status": t.Status,
		"owner": map[string]interface{}{"bot_id": t.OwnerID, "bot_name": t.OwnerName},
		"next_actor": participantRef(t.NextActorID, t.NextActorName),
		"next_action": t.NextAction,
		"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": t.UpdatedAt.UTC().Format(time.RFC3339),
		"closed_at": timePtrRFC3339(t.ClosedAt),
	}
}

func participantRef(id *int64, name *string) interface{} {
	if id == nil || name == nil {
		return nil
	}
	return map[string]interface{}{"bot_id": *id, "bot_name": *name}
}

func memberViews(rows []repository.ThreadMemberRow) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		out = append(out, map[string]interface{}{
			"bot_id": rows[i].BotID, "bot_name": rows[i].BotName,
			"role": rows[i].Role, "joined_at": rows[i].JoinedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func messageViews(rows []repository.ThreadMessageRow) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		out = append(out, map[string]interface{}{
			"message_id": rows[i].ID, "author": map[string]interface{}{"bot_id": rows[i].AuthorID, "bot_name": rows[i].AuthorName},
			"body": rows[i].Body, "created_at": rows[i].CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func deliveryViews(rows []repository.ThreadDeliveryRow) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		r := rows[i]
		out = append(out, map[string]interface{}{
			"delivery_id": r.ID,
			"author": map[string]interface{}{"bot_id": r.AuthorID, "bot_name": r.AuthorName},
			"title": r.Title, "body": r.Body, "status": r.Status, "revises": r.RevisesID,
			"reviewer": participantRef(r.ReviewerID, r.ReviewerName),
			"review_note": r.ReviewNote,
			"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
			"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
			"reviewed_at": timePtrRFC3339(r.ReviewedAt),
		})
	}
	return out
}

func eventView(r *repository.ThreadEventRow) map[string]interface{} {
	var payload interface{} = map[string]interface{}{}
	if r.Payload != "" {
		var decoded interface{}
		if json.Unmarshal([]byte(r.Payload), &decoded) == nil {
			payload = decoded
		}
	}
	return map[string]interface{}{
		"cursor": r.ID, "type": r.Type,
		"actor": participantRef(r.ActorID, r.ActorName),
		"payload": payload, "created_at": r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func timePtrRFC3339(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func validThreadParticipantName(name string) bool {
	if len(name) < 6 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_' || b == '.' || b == '-' {
			continue
		}
		return false
	}
	return true
}

func validationError(field, message string) error {
	return apperr.NewWithDetails(422, "VALIDATION_FAILED", message, map[string]interface{}{
		"errors": []map[string]string{{"field": field, "message": message}},
	})
}

func threadNotFound() error {
	return apperr.New(404, "THREAD_NOT_FOUND", "thread not found")
}

func threadClosed() error {
	return apperr.New(409, "THREAD_CLOSED", "thread is closed and read-only")
}

func invalidThreadInvite() error {
	return apperr.New(404, "THREAD_INVITE_INVALID", "thread invite is invalid or unavailable")
}

func threadInternal(action string) error {
	return apperr.New(500, "INTERNAL_ERROR", "could not "+action)
}

func threadLog(ctx context.Context, q pg.Querier, actorID int64, action, code string, data map[string]interface{}) {
	targetType := "thread"
	_ = repository.InsertOperationLog(ctx, q, repository.LogInsertData{
		BotID: &actorID, Action: action, TargetType: &targetType, TargetID: &code,
		RequestData: data, Success: true,
	})
}
