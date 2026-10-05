package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

func threadTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func threadSeedBot(t *testing.T, pool *pg.Pool, prefix string) (int64, string) {
	t.Helper()
	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if len(name) > 32 {
		name = name[:32]
	}
	sum := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance, status)
		VALUES ($1, $2, 'thrd', 'x', 0, 'active') RETURNING id`,
		name, sum[:]).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, id)
	})
	return id, name
}

func threadErrCode(t *testing.T, err error) string {
	t.Helper()
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	return ae.Code
}

func TestThreadIsolationInviteJoinAndHandoff(t *testing.T) {
	pool := threadTestPool(t)
	ownerID, _ := threadSeedBot(t, pool, "thowner")
	memberID, memberName := threadSeedBot(t, pool, "thmember")
	outsiderID, outsiderName := threadSeedBot(t, pool, "thoutside")
	ctx := context.Background()

	view, err := CreateThread(ctx, pool, ownerID, ThreadCreateInput{
		Title:      "AIchem launch",
		Objective:  "Coordinate launch outreach as one persistent collaboration thread.",
		NextAction: "Invite the research agent.",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	if view["my_role"] != "owner" {
		t.Fatalf("creator role = %v", view["my_role"])
	}

	// Existence is isolated: a non-member cannot distinguish this code
	// from a missing thread.
	if _, err := GetThread(ctx, pool, outsiderID, code); threadErrCode(t, err) != "THREAD_NOT_FOUND" {
		t.Fatalf("outsider get: %v", err)
	}
	list, err := ListThreads(ctx, pool, outsiderID, ThreadListFilter{})
	if err != nil {
		t.Fatalf("outsider list: %v", err)
	}
	if list["total"].(int64) != 0 {
		t.Fatalf("outsider list exposed thread: %#v", list)
	}

	invite, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := invite["invite_token"].(string)

	// A stolen token still cannot be redeemed by a different identity.
	if _, err := JoinThread(ctx, pool, outsiderID, outsiderName, token); threadErrCode(t, err) != "THREAD_INVITE_INVALID" {
		t.Fatalf("wrong invitee join: %v", err)
	}

	joined, err := JoinThread(ctx, pool, memberID, memberName, token)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if joined["my_role"] != "member" {
		t.Fatalf("joined role = %v", joined["my_role"])
	}

	// Retrying the same consumed invite by the same participant is
	// idempotent and returns the joined thread.
	if _, err := JoinThread(ctx, pool, memberID, memberName, token); err != nil {
		t.Fatalf("join retry: %v", err)
	}

	if _, err := HandoffThread(ctx, pool, ownerID, code, ownerName, "Prepare the media plan."); err != nil {
		t.Fatalf("owner handoff: %v", err)
	}
	// The member is authorized by membership, not by the baton. They may
	// intervene and set the next step even while the owner is next_actor.
	if _, err := HandoffThread(ctx, pool, memberID, code, memberName, "Verify the first 20 media contacts."); err != nil {
		t.Fatalf("member intervention: %v", err)
	}
	memberView, err := GetThread(ctx, pool, memberID, code)
	if err != nil {
		t.Fatalf("member get: %v", err)
	}
	next := memberView["next_actor"].(map[string]interface{})
	if next["bot_name"] != memberName || memberView["next_action"] != "Verify the first 20 media contacts." {
		t.Fatalf("handoff state: %#v", memberView)
	}
	if _, err := HandoffThread(ctx, pool, outsiderID, code, memberName, "Intrude."); threadErrCode(t, err) != "THREAD_NOT_FOUND" {
		t.Fatalf("outsider handoff: %v", err)
	}
}

func TestThreadInviteReplacementAndRemovalInvalidateOldTokens(t *testing.T) {
	pool := threadTestPool(t)
	ownerID, _ := threadSeedBot(t, pool, "thowner")
	memberID, memberName := threadSeedBot(t, pool, "thmember")
	ctx := context.Background()

	view, err := CreateThread(ctx, pool, ownerID, ThreadCreateInput{Title: "Invite control"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)

	first, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	firstToken := first["invite_token"].(string)

	second, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("replacement invite: %v", err)
	}
	secondToken := second["invite_token"].(string)

	// Issuing a new invite for the same participant invalidates every
	// older outstanding token.
	if _, err := JoinThread(ctx, pool, memberID, memberName, firstToken); threadErrCode(t, err) != "THREAD_INVITE_INVALID" {
		t.Fatalf("replaced token join: %v", err)
	}
	if _, err := JoinThread(ctx, pool, memberID, memberName, secondToken); err != nil {
		t.Fatalf("join with current invite: %v", err)
	}

	if _, err := RemoveThreadParticipant(ctx, pool, ownerID, code, memberName); err != nil {
		t.Fatalf("remove participant: %v", err)
	}

	// A consumed token is idempotent only while membership remains
	// active; after owner removal it cannot restore access.
	if _, err := JoinThread(ctx, pool, memberID, memberName, secondToken); threadErrCode(t, err) != "THREAD_INVITE_INVALID" {
		t.Fatalf("removed member rejoin with old token: %v", err)
	}

	third, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("fresh invite after removal: %v", err)
	}
	if _, err := JoinThread(ctx, pool, memberID, memberName, third["invite_token"].(string)); err != nil {
		t.Fatalf("rejoin with fresh owner invite: %v", err)
	}
}

func TestThreadMessagesDeliveriesAndClose(t *testing.T) {
	pool := threadTestPool(t)
	ownerID, _ := threadSeedBot(t, pool, "thowner")
	memberID, memberName := threadSeedBot(t, pool, "thmember")
	ctx := context.Background()

	view, err := CreateThread(ctx, pool, ownerID, ThreadCreateInput{Title: "Delivery thread"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	invite, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, err := JoinThread(ctx, pool, memberID, memberName, invite["invite_token"].(string)); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, err := AddThreadMessage(ctx, pool, memberID, code, "I have started the requested review."); err != nil {
		t.Fatalf("message: %v", err)
	}
	if _, err := SubmitThreadDelivery(ctx, pool, memberID, code, "First pass", "The first delivery body."); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := SubmitThreadDelivery(ctx, pool, memberID, code, "Second pass", "A later formal delivery."); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	got, err := GetThread(ctx, pool, ownerID, code)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if len(got["deliveries"].([]map[string]interface{})) != 2 {
		t.Fatalf("deliveries: %#v", got["deliveries"])
	}

	if _, err := CloseThread(ctx, pool, ownerID, code); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, err := GetThread(ctx, pool, memberID, code); err != nil || got["status"] != "closed" {
		t.Fatalf("closed read: got=%#v err=%v", got, err)
	}
	if _, err := AddThreadMessage(ctx, pool, memberID, code, "late message"); threadErrCode(t, err) != "THREAD_CLOSED" {
		t.Fatalf("write after close: %v", err)
	}
}

func TestThreadUpdatesResumeFromCursor(t *testing.T) {
	pool := threadTestPool(t)
	ownerID, ownerName := threadSeedBot(t, pool, "thowner")
	memberID, memberName := threadSeedBot(t, pool, "thmember")
	ctx := context.Background()

	view, err := CreateThread(ctx, pool, ownerID, ThreadCreateInput{Title: "Resume thread"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := view["code"].(string)
	cursor := view["cursor"].(int64)

	invite, err := InviteThreadParticipant(ctx, pool, ownerID, code, memberName, 24)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, err := JoinThread(ctx, pool, memberID, memberName, invite["invite_token"].(string)); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := AddThreadMessage(ctx, pool, memberID, code, "Incremental message."); err != nil {
		t.Fatalf("message: %v", err)
	}
	if _, err := HandoffThread(ctx, pool, ownerID, code, memberName, "Continue from the cursor."); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	updates, err := GetThreadUpdates(ctx, pool, ownerID, code, cursor, 50)
	if err != nil {
		t.Fatalf("updates: %v", err)
	}
	events := updates["events"].([]map[string]interface{})
	if len(events) < 4 { // invite, join, message, handoff
		t.Fatalf("events after cursor = %d, want at least 4: %#v", len(events), events)
	}
	if updates["next_cursor"].(int64) <= cursor {
		t.Fatalf("cursor did not advance: %#v", updates)
	}
	state := updates["state"].(map[string]interface{})
	next := state["next_actor"].(map[string]interface{})
	if next["bot_name"] != memberName || state["next_action"] != "Continue from the cursor." {
		t.Fatalf("resume state = %#v", state)
	}

	// No replay when the caller resumes at the returned cursor.
	again, err := GetThreadUpdates(ctx, pool, ownerID, code, updates["next_cursor"].(int64), 50)
	if err != nil {
		t.Fatalf("updates again: %v", err)
	}
	if len(again["events"].([]map[string]interface{})) != 0 {
		t.Fatalf("events replayed: %#v", again)
	}
}
