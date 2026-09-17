package admin

// B2 Store Administration domain integration tests (real PG, private
// DBs from the full migration chain — proving 001→007 order).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/store"
)

// b2SeedBot creates a bot with a balance for redemption flows.
func b2SeedBot(t *testing.T, dbPool *pg.Pool, balance float64) int64 {
	t.Helper()
	var id int64
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	err := dbPool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', $3) RETURNING id`,
		"b2bot_"+suffix, "kf_live_"+suffix+strings.Repeat("a", 64-len(suffix)), balance).Scan(&id)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=$1`, id)
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_redemptions WHERE bot_id=$1`, id)
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, id)
	})
	return id
}

// b2SeedRedemption creates a pending_review redemption with its spend.
func b2SeedRedemption(t *testing.T, dbPool *pg.Pool, botID int64, price float64) string {
	t.Helper()
	p, err := store.CreateProduct(context.Background(), dbPool, store.ProductInput{
		Title: "B2 Prod " + time.Now().Format("150405.000000000"), CreditsPrice: price,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	rk := "rk" + time.Now().Format("150405000000000")
	res, err := store.Redeem(context.Background(), dbPool, botID, p.Code, rk)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return res.Redemption.Code
}

func b2Balance(t *testing.T, dbPool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := dbPool.QueryRow(context.Background(),
		`SELECT balance FROM tb_bots WHERE id=$1`, botID).Scan(&b); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func b2RefundCount(t *testing.T, dbPool *pg.Pool, code string) int {
	t.Helper()
	var n int
	_ = dbPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type='refund_redemption' AND ref_type='redemption' AND ref_id=$1`, code).Scan(&n)
	return n
}

// -- permissions seeded (007) --

func TestB2StorePermissionsSeeded(t *testing.T) {
	dbPool := createPrivateDB(t) // runs the full 001→007 chain
	ctx := context.Background()
	for _, code := range []string{"store.products.read", "store.products.manage",
		"store.redemptions.read", "store.redemptions.manage"} {
		var exists bool
		if err := dbPool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM tb_admin_permissions WHERE code=$1)`, code).Scan(&exists); err != nil || !exists {
			t.Fatalf("permission %s missing (err=%v)", code, err)
		}
	}
	// no new system role
	var n int
	_ = dbPool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_admin_roles WHERE is_system`).Scan(&n)
	if n != 1 {
		t.Fatalf("system roles = %d, want exactly 1 (superadmin)", n)
	}
}

// -- superadmin wildcard uses all Store APIs --

func TestB2SuperadminFullStoreFlow(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	// wildcard sees the store permissions
	if !root.HasPermission("store.products.manage") || !root.HasPermission("store.redemptions.manage") {
		t.Fatal("superadmin wildcard must cover store permissions")
	}

	// create → edit → deactivate → activate
	created, err := CreateStoreProduct(ctx, dbPool, root, store.ProductInput{
		Title: "Flow Item", Description: "first", CreditsPrice: 12.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != "active" {
		t.Fatalf("new product status = %s", created.Status)
	}
	newTitle := "Flow Item v2"
	newPrice := 15.0
	updated, err := UpdateStoreProduct(ctx, dbPool, root, created.Code, store.ProductPatch{
		Title: &newTitle, CreditsPrice: &newPrice,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Title != "Flow Item v2" || updated.CreditsPrice != 15.0 || updated.Description == nil || *updated.Description != "first" {
		t.Fatalf("partial patch broken: %+v", updated)
	}
	off, err := SetStoreProductStatus(ctx, dbPool, root, created.Code, "inactive")
	if err != nil || off.Status != "inactive" {
		t.Fatalf("deactivate: %v %+v", err, off)
	}
	on, err := SetStoreProductStatus(ctx, dbPool, root, created.Code, "active")
	if err != nil || on.Status != "active" {
		t.Fatalf("activate: %v %+v", err, on)
	}
	// idempotent repeats
	on2, err := SetStoreProductStatus(ctx, dbPool, root, created.Code, "active")
	if err != nil || on2.Status != "active" {
		t.Fatalf("idempotent activate: %v", err)
	}

	// redemption full cycle: approve → fulfill
	botID := b2SeedBot(t, dbPool, 100)
	code := b2SeedRedemption(t, dbPool, botID, 12.5)
	oc, err := ApproveStoreRedemption(ctx, dbPool, root, code, "ok")
	if err != nil || !oc.Transitioned || oc.After.Status != "approved" {
		t.Fatalf("approve: %v %+v", err, oc)
	}
	// idempotent re-approve
	oc2, err := ApproveStoreRedemption(ctx, dbPool, root, code, "ok")
	if err != nil || oc2.Transitioned {
		t.Fatalf("re-approve must be idempotent: %v", err)
	}
	oc3, err := FulfillStoreRedemption(ctx, dbPool, root, code, "done")
	if err != nil || !oc3.Transitioned || oc3.After.Status != "fulfilled" {
		t.Fatalf("fulfill: %v", err)
	}
	// fulfilled → cancel 409
	_, err = CancelStoreRedemption(ctx, dbPool, root, code)
	if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 {
		t.Fatalf("cancel from fulfilled must 409, got %v", err)
	}
}

// -- custom role scoped permissions --

func TestB2ScopedCustomRoles(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	ctx := context.Background()

	// products.read only
	role, err := CreateRole(ctx, dbPool, root, CreateRoleInput{Code: "storeview", Name: "Store Viewer"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	if err := SetRolePermissions(ctx, dbPool, root, role.ID, []string{"store.products.read"}); err != nil {
		t.Fatalf("perms: %v", err)
	}
	viewer := b12CreateUserDirect(t, dbPool, "storeview.user", "viewer-pass-1", []string{"storeview"})
	vp := b12PrincipalOf(t, dbPool, viewer.Username, "viewer-pass-1")

	// read allowed
	if _, _, err := ListStoreProducts(ctx, dbPool, vp, store.ProductListFilter{}); err != nil {
		t.Fatalf("products read should be allowed: %v", err)
	}
	// mutation 403
	if _, err := CreateStoreProduct(ctx, dbPool, vp, store.ProductInput{Title: "X", CreditsPrice: 1}); err == nil {
		t.Fatal("products manage must be denied")
	} else if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 403 {
		t.Fatalf("expected 403, got %v", err)
	}
	// redemptions read denied too
	if _, _, err := ListStoreRedemptions(ctx, dbPool, vp, store.RedemptionListFilter{}); err == nil {
		t.Fatal("redemptions read must be denied without permission")
	}

	// redemptions.read/manage role
	role2, _ := CreateRole(ctx, dbPool, root, CreateRoleInput{Code: "redops", Name: "Redemption Ops"})
	_ = SetRolePermissions(ctx, dbPool, root, role2.ID, []string{"store.redemptions.read", "store.redemptions.manage"})
	ops := b12CreateUserDirect(t, dbPool, "redops.user", "ops-pass-123", []string{"redops"})
	op := b12PrincipalOf(t, dbPool, ops.Username, "ops-pass-123")
	if _, _, err := ListStoreRedemptions(ctx, dbPool, op, store.RedemptionListFilter{}); err != nil {
		t.Fatalf("redemptions read denied: %v", err)
	}
	// products mutation still denied
	if _, err := CreateStoreProduct(ctx, dbPool, op, store.ProductInput{Title: "X", CreditsPrice: 1}); err == nil {
		t.Fatal("products manage must be denied for redops")
	}
}

// live RBAC: grant store permission to an existing session's role →
// immediately effective.
func TestB2LivePermissionGrant(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	_, root := b12Super(t, dbPool)
	ctx := context.Background()

	role, _ := CreateRole(ctx, dbPool, root, CreateRoleInput{Code: "livegrant", Name: "Live"})
	user := b12CreateUserDirect(t, dbPool, "live.user", "live-pass-123", []string{"livegrant"})
	p := b12PrincipalOf(t, dbPool, user.Username, "live-pass-123")
	if p.HasPermission("store.products.read") {
		t.Fatal("precondition: must not hold store.products.read")
	}
	if err := SetRolePermissions(ctx, dbPool, root, role.ID, []string{"store.products.read"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	p2 := b12PrincipalOf(t, dbPool, user.Username, "live-pass-123")
	if !p2.HasPermission("store.products.read") {
		t.Fatal("grant must be immediately effective on the existing session")
	}
}

// -- economic transitions: exactly one refund --

func TestB2RejectAndCancelExactlyOneRefund(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	// reject path
	bot1 := b2SeedBot(t, dbPool, 50)
	code1 := b2SeedRedemption(t, dbPool, bot1, 10)
	if b2Balance(t, dbPool, bot1) != 40 {
		t.Fatalf("post-redeem balance = %v", b2Balance(t, dbPool, bot1))
	}
	if _, err := RejectStoreRedemption(ctx, dbPool, root, code1, "no"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if b2Balance(t, dbPool, bot1) != 50 {
		t.Fatalf("post-reject balance = %v", b2Balance(t, dbPool, bot1))
	}
	// idempotent re-reject: no second refund
	if _, err := RejectStoreRedemption(ctx, dbPool, root, code1, "no"); err != nil {
		t.Fatalf("re-reject: %v", err)
	}
	if n := b2RefundCount(t, dbPool, code1); n != 1 {
		t.Fatalf("reject refunds = %d, want exactly 1", n)
	}
	if b2Balance(t, dbPool, bot1) != 50 {
		t.Fatal("double refund!")
	}

	// cancel-from-pending
	bot2 := b2SeedBot(t, dbPool, 50)
	code2 := b2SeedRedemption(t, dbPool, bot2, 10)
	if _, err := CancelStoreRedemption(ctx, dbPool, root, code2); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if b2Balance(t, dbPool, bot2) != 50 || b2RefundCount(t, dbPool, code2) != 1 {
		t.Fatal("cancel-from-pending economics broken")
	}

	// cancel-from-approved
	bot3 := b2SeedBot(t, dbPool, 50)
	code3 := b2SeedRedemption(t, dbPool, bot3, 10)
	if _, err := ApproveStoreRedemption(ctx, dbPool, root, code3, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := CancelStoreRedemption(ctx, dbPool, root, code3); err != nil {
		t.Fatalf("cancel approved: %v", err)
	}
	if b2Balance(t, dbPool, bot3) != 50 || b2RefundCount(t, dbPool, code3) != 1 {
		t.Fatal("cancel-from-approved economics broken")
	}

	// review history not clobbered by cancel: approve with an empty
	// note stores NULL, and cancel must leave it NULL (no note write)
	var reviewNote *string
	_ = dbPool.QueryRow(ctx,
		`SELECT review_note FROM tb_redemptions WHERE code=$1`, code3).Scan(&reviewNote)
	if reviewNote != nil {
		t.Fatalf("cancel rewrote review history: %v", *reviewNote)
	}
	// and with a NON-empty approve note, the note survives cancel
	bot4 := b2SeedBot(t, dbPool, 50)
	code4 := b2SeedRedemption(t, dbPool, bot4, 10)
	if _, err := ApproveStoreRedemption(ctx, dbPool, root, code4, "keep me"); err != nil {
		t.Fatalf("approve4: %v", err)
	}
	if _, err := CancelStoreRedemption(ctx, dbPool, root, code4); err != nil {
		t.Fatalf("cancel4: %v", err)
	}
	var kept *string
	_ = dbPool.QueryRow(ctx,
		`SELECT review_note FROM tb_redemptions WHERE code=$1`, code4).Scan(&kept)
	if kept == nil || *kept != "keep me" {
		t.Fatalf("approved review note must survive cancel: %v", kept)
	}
}

// -- concurrent edit vs redeem: snapshot wholly old or new --

func TestB2ConcurrentEditVsRedeemSnapshotIntegrity(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	p, err := CreateStoreProduct(ctx, dbPool, root, store.ProductInput{
		Title: "Old Title", CreditsPrice: 10,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	botID := b2SeedBot(t, dbPool, 1000)

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	newTitle, newPrice := "New Title", 20.0

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				// editor (idempotent-ish: same target state each time)
				_, err := UpdateStoreProduct(ctx, dbPool, root, p.Code, store.ProductPatch{
					Title: &newTitle, CreditsPrice: &newPrice,
				})
				errs <- err
			} else {
				_, err := store.Redeem(ctx, dbPool, botID, p.Code, fmt.Sprintf("rk%d-%d", i, time.Now().UnixNano()))
				errs <- err
			}
		}(i)
	}
	close(start)
	go func() { wg.Wait(); close(errs) }()
	for err := range errs {
		if err != nil {
			t.Fatalf("racer error: %v", err)
		}
	}

	// every redemption snapshot must be internally consistent
	rows, err := dbPool.Query(ctx, `
		SELECT product_title, credits_cost FROM tb_redemptions WHERE bot_id=$1`, botID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		var cost float64
		if err := rows.Scan(&title, &cost); err != nil {
			t.Fatal(err)
		}
		oldRow := title == "Old Title" && cost == 10
		newRow := title == "New Title" && cost == 20
		if !oldRow && !newRow {
			t.Fatalf("MIXED SNAPSHOT: title=%q cost=%v", title, cost)
		}
	}
}

// -- audit: every mutation has a row with real transaction-local facts --

func TestB2StoreAuditFacts(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	created, _ := CreateStoreProduct(ctx, dbPool, root, store.ProductInput{
		Title: "Audit Item", Description: "d1", CreditsPrice: 5,
	})
	var after map[string]interface{}
	var targetID string
	err := dbPool.QueryRow(ctx, `
		SELECT target_id, after_json FROM tb_admin_audit_logs
		WHERE action='store.product.create' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&targetID, json.RawMessage(nil))
	_ = err
	// re-query properly with []byte
	var afterBytes []byte
	err = dbPool.QueryRow(ctx, `
		SELECT target_id, after_json FROM tb_admin_audit_logs
		WHERE action='store.product.create' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&targetID, &afterBytes)
	if err != nil {
		t.Fatalf("create audit: %v", err)
	}
	if targetID != created.Code {
		t.Fatalf("create audit target = %s, want code %s", targetID, created.Code)
	}
	_ = json.Unmarshal(afterBytes, &after)
	if after["title"] != "Audit Item" || after["credits_price"] != 5.0 || after["status"] != "active" {
		t.Fatalf("create audit facts: %v", after)
	}

	// update: real before/after
	newTitle := "Audit Item v2"
	if _, err := UpdateStoreProduct(ctx, dbPool, root, created.Code, store.ProductPatch{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var beforeBytes, after2 []byte
	err = dbPool.QueryRow(ctx, `
		SELECT before_json, after_json FROM tb_admin_audit_logs
		WHERE action='store.product.update' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&beforeBytes, &after2)
	if err != nil {
		t.Fatalf("update audit: %v", err)
	}
	var b4, af map[string]interface{}
	_ = json.Unmarshal(beforeBytes, &b4)
	_ = json.Unmarshal(after2, &af)
	if b4["title"] != "Audit Item" || af["title"] != "Audit Item v2" {
		t.Fatalf("update audit before/after: %v → %v", b4, af)
	}
	if af["credits_price"] != 5.0 || af["status"] != "active" {
		t.Fatalf("update audit must carry full snapshot: %v", af)
	}

	// redemption transition audit: real before/after + transitioned
	botID := b2SeedBot(t, dbPool, 20)
	code := b2SeedRedemption(t, dbPool, botID, 5)
	if _, err := RejectStoreRedemption(ctx, dbPool, root, code, "bad"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var rb, ra, rm []byte
	err = dbPool.QueryRow(ctx, `
		SELECT before_json, after_json, metadata_json FROM tb_admin_audit_logs
		WHERE action='store.redemption.reject' AND success ORDER BY id DESC LIMIT 1`).
		Scan(&rb, &ra, &rm)
	if err != nil {
		t.Fatalf("reject audit: %v", err)
	}
	var before2, after3, meta map[string]interface{}
	_ = json.Unmarshal(rb, &before2)
	_ = json.Unmarshal(ra, &after3)
	_ = json.Unmarshal(rm, &meta)
	if before2["status"] != "pending_review" || after3["status"] != "rejected" {
		t.Fatalf("reject audit states: %v → %v", before2, after3)
	}
	if before2["code"] != code || after3["credits_cost"] != 5.0 {
		t.Fatalf("reject audit facts incomplete: %v %v", before2, after3)
	}
	if meta["transitioned"] != true {
		t.Fatalf("transitioned metadata: %v", meta)
	}

	// idempotent replay audit: transitioned=false, before==after
	if _, err := RejectStoreRedemption(ctx, dbPool, root, code, "bad"); err != nil {
		t.Fatalf("re-reject: %v", err)
	}
	_ = dbPool.QueryRow(ctx, `
		SELECT metadata_json FROM tb_admin_audit_logs
		WHERE action='store.redemption.reject' AND success ORDER BY id DESC LIMIT 1`).Scan(&rm)
	_ = json.Unmarshal(rm, &meta)
	if meta["transitioned"] != false {
		t.Fatalf("idempotent replay must record transitioned=false: %v", meta)
	}
}

// -- audit rollback invariant (economics) --

func TestB2AuditInsertFailureRollsBackEverything(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	// inject a constraint that makes audit INSERTs fail for probe actions
	if _, err := dbPool.Exec(ctx,
		`ALTER TABLE tb_admin_audit_logs ADD CONSTRAINT chk_b2_probe
		 CHECK (action NOT LIKE 'store.%')`); err != nil {
		t.Fatalf("inject: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dbPool.Exec(context.Background(), `ALTER TABLE tb_admin_audit_logs DROP CONSTRAINT IF EXISTS chk_b2_probe`)
	})

	botID := b2SeedBot(t, dbPool, 50)
	code := b2SeedRedemption(t, dbPool, botID, 10)
	beforeBalance := b2Balance(t, dbPool, botID) // 40

	// reject with failing audit → everything rolls back
	_, err := RejectStoreRedemption(ctx, dbPool, root, code, "no")
	if err == nil {
		t.Fatal("reject must fail when the audit insert fails")
	}
	// status rollback
	var status string
	_ = dbPool.QueryRow(ctx, `SELECT status FROM tb_redemptions WHERE code=$1`, code).Scan(&status)
	if status != "pending_review" {
		t.Fatalf("status rolled forward without audit: %s", status)
	}
	// balance rollback
	if b := b2Balance(t, dbPool, botID); b != beforeBalance {
		t.Fatalf("balance changed: %v want %v", b, beforeBalance)
	}
	// ledger rollback
	if n := b2RefundCount(t, dbPool, code); n != 0 {
		t.Fatalf("refund ledger rows = %d, want 0", n)
	}

	// product mutation with failing audit → rollback
	created, err := store.CreateProduct(ctx, dbPool, store.ProductInput{Title: "Probe", CreditsPrice: 1})
	if err != nil {
		t.Fatalf("setup product: %v", err)
	}
	newTitle := "Probe v2"
	if _, err := UpdateStoreProduct(ctx, dbPool, root, created.Code, store.ProductPatch{Title: &newTitle}); err == nil {
		t.Fatal("product update must fail when the audit insert fails")
	}
	var title string
	_ = dbPool.QueryRow(ctx, `SELECT title FROM tb_store_products WHERE code=$1`, created.Code).Scan(&title)
	if title != "Probe" {
		t.Fatalf("product title rolled forward without audit: %s", title)
	}
	// no audit rows landed
	var n int
	_ = dbPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_admin_audit_logs WHERE action LIKE 'store.%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("store audit rows = %d, want 0", n)
	}
}

// -- reads: filters, pagination, search, inactive visibility --

func TestB2AdminReads(t *testing.T) {
	dbPool := createPrivateDB(t)
	bootstrapForTest(t, dbPool)
	root := b12PrincipalOf(t, dbPool, "root", "password-123")
	ctx := context.Background()

	p1, _ := CreateStoreProduct(ctx, dbPool, root, store.ProductInput{Title: "Visible One", CreditsPrice: 3})
	p2, _ := CreateStoreProduct(ctx, dbPool, root, store.ProductInput{Title: "Hidden Two", CreditsPrice: 4})
	if _, err := SetStoreProductStatus(ctx, dbPool, root, p2.Code, "inactive"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// admin list includes inactive
	items, total, err := ListStoreProducts(ctx, dbPool, root, store.ProductListFilter{Status: "all"})
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("all-products list: total=%d items=%d err=%v", total, len(items), err)
	}
	// status filter
	items, _, _ = ListStoreProducts(ctx, dbPool, root, store.ProductListFilter{Status: "inactive"})
	if len(items) != 1 || items[0].Code != p2.Code {
		t.Fatalf("inactive filter: %+v", items)
	}
	// search by code and title
	items, _, _ = ListStoreProducts(ctx, dbPool, root, store.ProductListFilter{Q: p1.Code})
	if len(items) != 1 || items[0].Code != p1.Code {
		t.Fatalf("code search: %+v", items)
	}
	items, _, _ = ListStoreProducts(ctx, dbPool, root, store.ProductListFilter{Q: "Hidden"})
	if len(items) != 1 || items[0].Code != p2.Code {
		t.Fatalf("title search: %+v", items)
	}
	// invalid status filter → 400
	if _, _, err := ListStoreProducts(ctx, dbPool, root, store.ProductListFilter{Status: "bogus"}); err == nil {
		t.Fatal("invalid status filter must 400")
	}

	// inactive product still NOT redeemable by owner
	botID := b2SeedBot(t, dbPool, 100)
	if _, err := store.Redeem(ctx, dbPool, botID, p2.Code, "rk-inactive-1"); err == nil {
		t.Fatal("inactive product must not be redeemable")
	} else if ae, _ := errors.IsAppError(err); ae == nil || ae.HTTPCode != 409 {
		t.Fatalf("expected 409 PRODUCT_INACTIVE, got %v", err)
	}

	// redemption list filters — redeem p1 ("Visible One") directly so
	// the title search has a real target
	codeA := b2RedeemDirect(t, dbPool, botID, p1.Code)
	bot2 := b2SeedBot(t, dbPool, 100)
	codeB := b2RedeemDirect(t, dbPool, bot2, p1.Code)
	_, _ = ApproveStoreRedemption(ctx, dbPool, root, codeB, "")

	reds, total, err := ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Status: "all"})
	if err != nil || total != 2 {
		t.Fatalf("redemptions all: total=%d err=%v", total, err)
	}
	// stable order: newest first
	if reds[0].Code != codeB || reds[1].Code != codeA {
		t.Fatalf("order: %s then %s", reds[0].Code, reds[1].Code)
	}
	// status filter
	reds, _, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Status: "pending_review"})
	if len(reds) != 1 || reds[0].Code != codeA {
		t.Fatalf("pending filter: %+v", reds)
	}
	// bot filter
	reds, _, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{BotID: bot2})
	if len(reds) != 1 || reds[0].Code != codeB {
		t.Fatalf("bot filter: %+v", reds)
	}
	// q search by code / title / request_key
	reds, _, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Q: codeA})
	if len(reds) != 1 || reds[0].Code != codeA {
		t.Fatalf("code search: %+v", reds)
	}
	reds, _, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Q: "Visible One"})
	if len(reds) != 2 {
		t.Fatalf("title search: %+v", reds)
	}
	// pagination
	reds, total, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Page: 1, PageSize: 1})
	if len(reds) != 1 || total != 2 {
		t.Fatalf("page 1: len=%d total=%d", len(reds), total)
	}
	reds, _, _ = ListStoreRedemptions(ctx, dbPool, root, store.RedemptionListFilter{Page: 2, PageSize: 1})
	if len(reds) != 1 || reds[0].Code != codeA {
		t.Fatalf("page 2: %+v", reds)
	}
}

// -- legacy Store Core wrappers still work (single state machine) --

func TestB2LegacyStoreWrappersUnchanged(t *testing.T) {
	dbPool := createPrivateDB(t)
	ctx := context.Background()

	botID := b2SeedBot(t, dbPool, 30)
	code := b2SeedRedemption(t, dbPool, botID, 10)

	// legacy approve path (store.ApproveRedemption)
	if _, err := store.ApproveRedemption(ctx, dbPool, code, ""); err != nil {
		t.Fatalf("legacy approve: %v", err)
	}
	// legacy reject on approved → 409
	if _, err := store.RejectRedemption(ctx, dbPool, code, ""); err == nil {
		t.Fatal("legacy reject from approved must 409")
	}
	// legacy cancel from approved works + refunds once
	if _, err := store.CancelRedemption(ctx, dbPool, code); err != nil {
		t.Fatalf("legacy cancel: %v", err)
	}
	if b2Balance(t, dbPool, botID) != 30 {
		t.Fatal("legacy cancel economics broken")
	}
	if n := b2RefundCount(t, dbPool, code); n != 1 {
		t.Fatalf("legacy refund count = %d", n)
	}
	_ = repository.StoreProductCodeExists // keep import parity
}

// b2RedeemDirect redeems an EXISTING product (helper creates a new one).
func b2RedeemDirect(t *testing.T, dbPool *pg.Pool, botID int64, productCode string) string {
	t.Helper()
	rk := "rk" + fmt.Sprintf("%d", time.Now().UnixNano())
	res, err := store.Redeem(context.Background(), dbPool, botID, productCode, rk)
	if err != nil {
		t.Fatalf("redeem direct: %v", err)
	}
	return res.Redemption.Code
}

// ===========================================================================
// B2 repair: Finding 3 — legacy product status wrappers delegate to the
// single Tx primitive.
// ===========================================================================

func TestB2RepairLegacyProductStatusWrappers(t *testing.T) {
	dbPool := createPrivateDB(t)
	ctx := context.Background()

	p, err := store.CreateProduct(ctx, dbPool, store.ProductInput{Title: "Legacy Status", CreditsPrice: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// deactivate → status correct
	ok, err := store.SetProductInactive(ctx, dbPool, p.Code)
	if err != nil || !ok {
		t.Fatalf("deactivate: %v %v", ok, err)
	}
	got := productStatus(t, dbPool, p.Code)
	if got != "inactive" {
		t.Fatalf("status = %s", got)
	}
	// repeat deactivate → idempotent, still inactive
	if ok, err := store.SetProductInactive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("idempotent deactivate: %v %v", ok, err)
	}
	if got = productStatus(t, dbPool, p.Code); got != "inactive" {
		t.Fatalf("status after repeat = %s", got)
	}

	// activate → correct; repeat → idempotent
	if ok, err := store.SetProductActive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	if got = productStatus(t, dbPool, p.Code); got != "active" {
		t.Fatalf("status = %s", got)
	}
	if ok, err := store.SetProductActive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("idempotent activate: %v %v", ok, err)
	}
	if got = productStatus(t, dbPool, p.Code); got != "active" {
		t.Fatalf("status after repeat = %s", got)
	}
}

func productStatus(t *testing.T, dbPool *pg.Pool, code string) string {
	t.Helper()
	var s string
	if err := dbPool.QueryRow(context.Background(),
		`SELECT status FROM tb_store_products WHERE code=$1`, code).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Source contract: the legacy wrappers must delegate to the single Tx
// primitive and no second repository status-mutation entrypoint may
// exist. AST-level: SetProductActive/Inactive bodies call
// SetProductStatusTx via runInTx, and repository has no remaining
// status-mutation function besides the Returning (row-locked) one.
func TestB2RepairSingleStatusMutationImplementation(t *testing.T) {
	root := repoRoot(t)

	storeSrc := readSourceForGuard(t, filepath.Join(root, "internal", "store", "store.go"))
	// both public wrappers delegate to the shared legacy helper, which
	// is the ONLY tx-owner over SetProductStatusTx for the legacy path
	for _, fn := range []string{"SetProductActive", "SetProductInactive"} {
		body := extractFuncBody(storeSrc, fn)
		if body == "" {
			t.Fatalf("%s not found in store.go", fn)
		}
		if !strings.Contains(body, "legacySetProductStatus") {
			t.Fatalf("%s must delegate to legacySetProductStatus", fn)
		}
		if strings.Contains(body, "repository.SetStoreProductStatus") {
			t.Fatalf("%s still calls the removed direct repository path", fn)
		}
	}
	helper := extractFuncBody(storeSrc, "legacySetProductStatus")
	if helper == "" {
		t.Fatal("legacySetProductStatus not found")
	}
	if !strings.Contains(helper, "SetProductStatusTx") || !strings.Contains(helper, "runInTx") {
		t.Fatal("legacySetProductStatus must delegate to SetProductStatusTx inside runInTx (single implementation)")
	}

	// repository: the only status writer is the row-locked Returning variant
	repoSrc := readSourceForGuard(t, filepath.Join(root, "internal", "repository", "store.go"))
	if strings.Contains(repoSrc, "func SetStoreProductStatus(") {
		t.Fatal("repository.SetStoreProductStatus (second status entrypoint) must not exist")
	}
	if !strings.Contains(repoSrc, "func SetStoreProductStatusReturning(") {
		t.Fatal("SetStoreProductStatusReturning missing")
	}
	// no other production package calls a status mutation on products
	// besides the Tx primitive
	txSrc := readSourceForGuard(t, filepath.Join(root, "internal", "store", "store_tx.go"))
	if !strings.Contains(txSrc, "SetStoreProductStatusReturning") {
		t.Fatal("SetProductStatusTx must be the sole caller of the status write")
	}
}

// readSourceForGuard reads a production source file for contract tests.
func readSourceForGuard(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// extractFuncBody returns the source text of one top-level function
// (balanced-brace scan from the "func NAME(" signature).
func extractFuncBody(src, name string) string {
	idx := strings.Index(src, "func "+name+"(")
	if idx < 0 {
		return ""
	}
	depth := 0
	start := -1
	for i := idx; i < len(src); i++ {
		switch src[i] {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				return src[start : i+1]
			}
		}
	}
	return ""
}

// ===========================================================================
// B2 repair-2: legacy wrapper contract preserved (missing → false,nil).
// ===========================================================================

func TestB2Repair2LegacyStatusNotFoundContract(t *testing.T) {
	dbPool := createPrivateDB(t)
	ctx := context.Background()

	// nonexistent codes → (false, nil), NOT an error
	ok, err := store.SetProductInactive(ctx, dbPool, "nonexistent1")
	if err != nil || ok {
		t.Fatalf("inactive(nonexistent) = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = store.SetProductActive(ctx, dbPool, "nonexistent2")
	if err != nil || ok {
		t.Fatalf("active(nonexistent) = (%v, %v), want (false, nil)", ok, err)
	}

	// existing product: correct status, repeated idempotent
	p, err := store.CreateProduct(ctx, dbPool, store.ProductInput{Title: "R2 Status", CreditsPrice: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := store.SetProductInactive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("inactive = (%v, %v)", ok, err)
	}
	if s := productStatus(t, dbPool, p.Code); s != "inactive" {
		t.Fatalf("status = %s", s)
	}
	if ok, err := store.SetProductInactive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("repeat inactive = (%v, %v)", ok, err)
	}
	if ok, err := store.SetProductActive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("active = (%v, %v)", ok, err)
	}
	if s := productStatus(t, dbPool, p.Code); s != "active" {
		t.Fatalf("status = %s", s)
	}
	if ok, err := store.SetProductActive(ctx, dbPool, p.Code); err != nil || !ok {
		t.Fatalf("repeat active = (%v, %v)", ok, err)
	}
}
