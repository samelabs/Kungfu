package service

// Task 1.2 (WO-32) — the 033 upgrade drill: a database carrying
// live pre-1.2 tasks and claims is upgraded with the shipped
// migration; every existing task stays open-audience (no
// task_audience rows), behaves exactly as before (listed, readable,
// claimable, judgment and settlement unchanged), and the extended
// CheckInvariants passes on the legacy shape. Restricted tasks
// created after the upgrade behave per §7.2 on the same database.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// task12Migrations returns the migration files split at 033: the
// pre-033 chain and everything from 033 on (an upgrade run applies
// every pending file together).
func task12Migrations(t *testing.T) (pre []string, from033 []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v (%d)", err, len(files))
	}
	sort.Strings(files)
	for i, f := range files {
		if strings.Contains(f, "033_") {
			return files[:i], files[i:]
		}
	}
	t.Fatal("033 migration not found")
	return nil, nil
}

// TestTask12AudienceUpgradeDrill: a live pre-1.2 task database is
// upgraded with 033; legacy tasks are open (no audience rows, no
// behavior change), and post-upgrade restricted tasks work through
// the same service code on the upgraded schema.
func TestTask12AudienceUpgradeDrill(t *testing.T) {
	pubTestPool(t) // presence + skip contract; throwaway DB below
	ctx := context.Background()

	dbName := "kf_task12_" + nanoSuffix()
	adminURL := ""
	if i := strings.LastIndex(os.Getenv("KF_TEST_DATABASE_URL"), "/"); i >= 0 {
		adminURL = os.Getenv("KF_TEST_DATABASE_URL")[:i] + "/postgres"
	}
	if adminURL == "" {
		t.Skip("cannot derive admin DSN")
	}
	admin, err := pg.NewPool(adminURL)
	if err != nil {
		t.Skipf("admin connection unavailable: %v", err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Skipf("create db: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`) })
	dbURL := adminURL[:strings.LastIndex(adminURL, "/")] + "/" + dbName
	db, err := pg.NewPool(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	pre, from033 := task12Migrations(t)
	for _, f := range pre {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	// seed a live pre-1.2 shape: publisher + agent + an OPEN task
	// (its 1.1 columns already exist pre-033) + an active claim
	pubKey := sha256.Sum256([]byte("task12-pub"))
	agentKey := sha256.Sum256([]byte("task12-agent"))
	var publisherID, agentID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('task12pub', 'x', $1, 'ab12', 100000)
		RETURNING id`, pubKey[:]).Scan(&publisherID); err != nil {
		t.Fatalf("seed publisher: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('task12agent', 'x', $1, 'cd34', 0)
		RETURNING id`, agentKey[:]).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// sanity: the pre-chain really has no task_audience yet
	if _, err := db.Exec(ctx, `SELECT 1 FROM task_audience LIMIT 1`); err == nil {
		t.Fatal("task_audience exists before 033 — the split is wrong")
	}
	v1JSON := fmt.Sprintf(`{"title":"legacy","requirements":"do it","receiver":{"url":%q},"price":5,"claim":{"required":true}}`, okReceiverURL)
	var taskID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_task (code, publisher_id, status, budget_locked, reserved, contract, contract_version)
		VALUES ('task12upg0', $1, 'open', 100, 5, $2, 1)
		RETURNING id`, publisherID, []byte(v1JSON)).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO task_contract_versions (task_id, version, contract)
		VALUES ($1, 1, $2)`, taskID, []byte(v1JSON)); err != nil {
		t.Fatalf("seed version row: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tb_task_claim (task_id, agent_id, contract_version, expires_at, deadline, amount, status)
		VALUES ($1, $2, 1, NOW() + interval '1 hour', NOW() + interval '2 hours', 5, 'active')`,
		taskID, agentID); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	// apply the shipped 033 as one upgrade run
	for _, f := range from033 {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	// the legacy task is open-audience: no rows, nothing backfilled
	var rows int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM task_audience`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("task_audience rows after upgrade = %d err=%v, want 0", rows, err)
	}

	// legacy behavior is unchanged: listed to a fresh agent and to
	// the anonymous board, readable, and the upgraded claim delivers
	fresh, err := repository.FindBotIDByName(ctx, db, "task12agent")
	if err != nil || fresh == nil {
		t.Fatalf("legacy agent: %v", err)
	}
	items, _, err := ListWork(ctx, db, agentID, time.Now(), WorkListFilter{Code: "task12upg0"})
	if err != nil || len(items) != 1 || items[0]["audience"] != task.AudienceOpen {
		t.Fatalf("legacy task after upgrade: %v %#v, want one open-audience row", err, items)
	}
	board, _, err := ListWorkBoard(ctx, db, "", "task12upg0", 1, 20)
	if err != nil || len(board) != 1 {
		t.Fatalf("legacy task on the board: %v %d", err, len(board))
	}
	if _, err := GetWork(ctx, db, agentID, "task12upg0", time.Now()); err != nil {
		t.Fatalf("legacy work_get: %v", err)
	}
	var claimID int64
	if err := db.QueryRow(ctx, `
		SELECT claim_id FROM tb_task_claim WHERE task_id = $1 AND status = 'active'`, taskID).Scan(&claimID); err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	claim := WireID(claimID)
	view, err := SubmitWork(ctx, db, agentID, SubmitInput{
		Code: "task12upg0", RequestKey: "task12-upgraded", Payload: []byte(submitPayloadOK),
		ClaimID: &claim,
	}, testAgentRefKey, time.Now())
	if err != nil || view.State != task.SubSettled || view.ContractVersion != 1 {
		t.Fatalf("legacy claim submission = %+v err=%v, want settled at version 1", view, err)
	}
	if err := task.CheckInvariants(ctx, db, taskID); err != nil {
		t.Fatalf("CheckInvariants on the upgraded legacy task: %v", err)
	}

	// a revision of the legacy task (with the audience materialized
	// open by the 1.2 code) still passes the audience-drift audit
	if _, err := PauseTask(ctx, db, publisherID, "task12upg0"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	rev, err := GetTask(ctx, db, publisherID, "task12upg0")
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	raw, _ := json.Marshal(rev["contract"])
	var upd task.Contract
	if err := json.Unmarshal(raw, &upd); err != nil {
		t.Fatalf("decode stored contract: %v", err)
	}
	upd.Title = "revised after upgrade"
	if _, err := UpdateTask(ctx, db, publisherID, "task12upg0", upd); err != nil {
		t.Fatalf("update legacy task: %v", err)
	}
	if err := task.CheckInvariants(ctx, db, taskID); err != nil {
		t.Fatalf("CheckInvariants after legacy revision: %v", err)
	}

	// post-upgrade restricted task on the same database: full §7.2
	// behavior through the same service code
	named, err := repository.FindBotIDByName(ctx, db, "task12agent")
	if err != nil || named == nil {
		t.Fatalf("named agent: %v", err)
	}
	rc := pubContract("")
	rc.Audience = &task.Audience{Type: task.AudienceRestricted, Agents: []string{"task12agent"}}
	rCode := pubCreateForTest(t, db, publisherID, rc, 100)
	if _, err := OpenTask(ctx, db, publisherID, rCode); err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	var outsiderID int64
	outKey := sha256.Sum256([]byte("task12-out"))
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('task12out', 'x', $1, 'ef56', 0)
		RETURNING id`, outKey[:]).Scan(&outsiderID); err != nil {
		t.Fatalf("seed outsider: %v", err)
	}
	if _, err := GetWork(ctx, db, outsiderID, rCode, time.Now()); appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("outsider on the restricted task: %v", err)
	}
	// and the legacy task's agent sees the new restricted task
	items, _, err = ListWork(ctx, db, agentID, time.Now(), WorkListFilter{Code: rCode})
	if err != nil || len(items) != 1 || items[0]["audience"] != task.AudienceRestricted {
		t.Fatalf("named agent on the restricted task: %v %#v", err, items)
	}
	rTr, _ := repository.FindTaskByCode(ctx, db, rCode)
	if err := task.CheckInvariants(ctx, db, rTr.ID); err != nil {
		t.Fatalf("CheckInvariants on the restricted task: %v", err)
	}
}
