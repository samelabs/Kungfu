package store

// Regression test for the approved -> cancelled review-fact preservation:
// cancellation must not erase review_note / reviewed_at written by an
// earlier approval. Runs against the local dev PostgreSQL.

import (
	"context"
	"database/sql"
	"testing"
)

func TestCancelPreservesReviewFact(t *testing.T) {
	pool := testPool(t)

	// approved redemption with a review note, then cancelled
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_cxkeep_1")

		out, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "approved-note")
		if err != nil || !out.Transitioned {
			t.Fatalf("approve: %v", err)
		}

		cancelOut, err := CancelRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil || !cancelOut.Transitioned {
			t.Fatalf("cancel: %v", err)
		}

		// DB state: cancelled, review facts intact, cancelled_at set.
		var status, reviewNote string
		var reviewedAt, cancelledAt sql.NullTime
		if err := pool.QueryRow(context.Background(), `
			SELECT status, COALESCE(review_note, ''), reviewed_at, cancelled_at
			FROM tb_redemptions WHERE code = $1`, res.Redemption.Code).
			Scan(&status, &reviewNote, &reviewedAt, &cancelledAt); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if status != "cancelled" {
			t.Fatalf("status = %s, want cancelled", status)
		}
		if reviewNote != "approved-note" {
			t.Fatalf("review_note = %q, want %q", reviewNote, "approved-note")
		}
		if !reviewedAt.Valid {
			t.Fatal("reviewed_at was nulled by cancel")
		}
		if !cancelledAt.Valid {
			t.Fatal("cancelled_at missing")
		}

		// Exactly one refund; balance restored.
		_, _, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if refunds != 1 || refundSum != 30 {
			t.Fatalf("refunds = %d/%v, want 1/30", refunds, refundSum)
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100", got)
		}

		// Service-returned object matches a fresh DB read on the fields
		// in question.
		fresh, err := GetRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil {
			t.Fatalf("fresh read: %v", err)
		}
		if fresh.Status != cancelOut.Redemption.Status {
			t.Fatalf("status mismatch: service=%s db=%s", cancelOut.Redemption.Status, fresh.Status)
		}
		if fresh.ReviewNote == nil || *fresh.ReviewNote != "approved-note" {
			t.Fatalf("db review_note = %v, want approved-note", fresh.ReviewNote)
		}
	}

	// pending_review -> cancelled still fine: review_note stays NULL,
	// exactly one refund.
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_cxkeep_2")

		out, err := CancelRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil || !out.Transitioned {
			t.Fatalf("cancel pending: %v", err)
		}
		if statusOf(t, pool, res.Redemption.Code) != "cancelled" {
			t.Fatal("status not cancelled")
		}
		var reviewNote *string
		if err := pool.QueryRow(context.Background(),
			`SELECT review_note FROM tb_redemptions WHERE code = $1`, res.Redemption.Code).
			Scan(&reviewNote); err != nil {
			t.Fatalf("reload note: %v", err)
		}
		if reviewNote != nil {
			t.Fatalf("review_note = %v, want NULL", *reviewNote)
		}
		_, _, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if refunds != 1 || refundSum != 30 {
			t.Fatalf("refunds = %d/%v, want 1/30", refunds, refundSum)
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100", got)
		}
	}
}
