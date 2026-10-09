package service

// Task 1.1 §7.1 contract versions: a revision is a new version that
// binds only engagements formed after it. A claim-carried submission
// is schema-checked and delivered against the version the claim
// bound; a claim-less submission against the current version, which
// is recorded on the submission. The upgrade drill replays 031 over
// a database with pre-1.1 tasks, claims and submissions and asserts
// the backfill plus unchanged behavior.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// versionTestReceiver starts a second accept-everything TLS receiver
// whose body distinguishes it from the package default.
func versionTestReceiver(t *testing.T, body string) *httptest.Server {
	t.Helper()
	rec := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	rec.TLS = &tls.Config{Certificates: []tls.Certificate{pubTestTLSCert}}
	rec.StartTLS()
	t.Cleanup(rec.Close)
	return rec
}

const v1Payload = `{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`
const v2Schema = `{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}}}`
const v2Payload = `{"answer":"yes"}`

// TestContractRevisionBindsOnlyLaterEngagements: v1 schema/receiver
// bind the claims formed under them; after task_update publishes v2
// (and v3), the old claim still validates and delivers against v1,
// new claims bind v2, and claim-less submissions use the current
// version — recorded on the submission.
func TestContractRevisionBindsOnlyLaterEngagements(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	rec2 := versionTestReceiver(t, `{"message":"v2-here"}`)
	rec3 := versionTestReceiver(t, `{"message":"v3-here"}`)

	// v1: default claim contract (bullets schema, default receiver, price 5)
	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim1, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim v1: %v", err)
	}
	if claim1.ContractVersion != 1 || claim1.Amount != 5 {
		t.Fatalf("v1 claim = %+v, want version 1 / amount 5", claim1)
	}

	// publish v2: different schema, different receiver, price 10
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	v2 := claimContract()
	v2.Title = "v2 contract"
	v2.Output.Schema = []byte(v2Schema)
	v2.Receiver.URL = rec2.URL
	v2.Price = 10
	updated, err := UpdateTask(ctx, pool, publisher, code, v2)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated["contract_version"] != int64(2) {
		t.Fatalf("after update contract_version = %v, want 2", updated["contract_version"])
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// the v1 claim's submission is checked against the v1 schema and
	// delivered to the v1 receiver (a v2-only payload is SCHEMA_MISMATCH)
	_, err = SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "old-claim-v2-payload", Payload: []byte(v2Payload),
		ClaimID: &claim1.ClaimID,
	}, testAgentRefKey, time.Now())
	if e := appErrOf(t, err); e.Code != "SCHEMA_MISMATCH" {
		t.Fatalf("v1 claim + v2 payload: %v, want SCHEMA_MISMATCH", err)
	}
	view, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "old-claim-v1-payload", Payload: []byte(v1Payload),
		ClaimID: &claim1.ClaimID,
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("v1 claim submit: %v", err)
	}
	if view.State != task.SubSettled || view.ContractVersion != 1 || view.Amount != 5 {
		t.Fatalf("v1 claim submission = %+v, want settled / version 1 / amount 5", view)
	}
	if view.Reply == nil || !strings.Contains(view.Reply.Body, "accepted") {
		t.Fatalf("v1 submission delivered to the wrong receiver: %+v", view.Reply)
	}
	claimTaskReserved(t, pool, code)

	// a claim formed AFTER the revision binds v2
	claim2, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim v2: %v", err)
	}
	if claim2.ContractVersion != 2 || claim2.Amount != 10 {
		t.Fatalf("v2 claim = %+v, want version 2 / amount 10", claim2)
	}
	if _, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "v2-claim-v1-payload", Payload: []byte(v1Payload),
		ClaimID: &claim2.ClaimID,
	}, testAgentRefKey, time.Now()); err == nil {
		t.Fatal("v1 payload under a v2 claim must be SCHEMA_MISMATCH")
	} else if e := appErrOf(t, err); e.Code != "SCHEMA_MISMATCH" {
		t.Fatalf("v2 claim + v1 payload: %v, want SCHEMA_MISMATCH", err)
	}
	view2, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "v2-claim-v2-payload", Payload: []byte(v2Payload),
		ClaimID: &claim2.ClaimID,
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("v2 claim submit: %v", err)
	}
	if view2.State != task.SubSettled || view2.ContractVersion != 2 || view2.Amount != 10 {
		t.Fatalf("v2 claim submission = %+v, want settled / version 2 / amount 10", view2)
	}
	if view2.Reply == nil || !strings.Contains(view2.Reply.Body, "v2-here") {
		t.Fatalf("v2 submission delivered to the wrong receiver: %+v", view2.Reply)
	}
	claimTaskReserved(t, pool, code)

	// claim-less submissions use the CURRENT version, recorded on the
	// submission row
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause for v3: %v", err)
	}
	v3 := v2
	v3.Title = "v3 contract"
	v3.Claim = task.ClaimConfig{} // claim no longer required
	v3.Receiver.URL = rec3.URL
	v3.Price = 7
	v3.Output.Schema = []byte(`{"type":"object"}`)
	if _, err := UpdateTask(ctx, pool, publisher, code, v3); err != nil {
		t.Fatalf("update v3: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("reopen v3: %v", err)
	}
	view3, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: "claimless-v3", Payload: []byte(`{"anything":"goes"}`),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("claimless submit: %v", err)
	}
	if view3.State != task.SubSettled || view3.ContractVersion != 3 || view3.Amount != 7 {
		t.Fatalf("claimless submission = %+v, want settled / version 3 / amount 7", view3)
	}
	if view3.Reply == nil || !strings.Contains(view3.Reply.Body, "v3-here") {
		t.Fatalf("claimless submission delivered to the wrong receiver: %+v", view3.Reply)
	}

	// the recorded version survives on the row itself
	tr, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || tr == nil {
		t.Fatalf("reload task: %v", err)
	}
	if tr.ContractVersion != 3 {
		t.Fatalf("task contract_version = %d, want 3", tr.ContractVersion)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// TestWorkGetServesBoundVersionToEngagedAgent: while an agent holds
// an active claim, work_get shows the contract version the claim
// bound (kungfu.md §9); everyone else sees the current version.
// task_get reports the publisher's current version.
func TestWorkGetServesBoundVersionToEngagedAgent(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	bystander := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := ClaimTask(ctx, pool, agent, code, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	v2 := claimContract()
	v2.Title = "revised title"
	v2.Output.Schema = []byte(v2Schema)
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := UpdateTask(ctx, pool, publisher, code, v2); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// engaged agent: the bound (v1) contract
	engaged, err := GetWork(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("work_get engaged: %v", err)
	}
	if engaged["contract_version"] != int64(1) {
		t.Fatalf("engaged contract_version = %v, want 1", engaged["contract_version"])
	}
	c := engaged["contract"].(map[string]any)
	if c["title"] != "Summarize a page" {
		t.Fatalf("engaged agent sees title %v, want the v1 title", c["title"])
	}

	// no engagement: the current (v2) contract
	current, err := GetWork(ctx, pool, bystander, code, time.Now())
	if err != nil {
		t.Fatalf("work_get bystander: %v", err)
	}
	if current["contract_version"] != int64(2) {
		t.Fatalf("bystander contract_version = %v, want 2", current["contract_version"])
	}
	cb := current["contract"].(map[string]any)
	if cb["title"] != "revised title" {
		t.Fatalf("bystander sees title %v, want the v2 title", cb["title"])
	}

	// the publisher reads the current version through task_get
	owned, err := GetTask(ctx, pool, publisher, code)
	if err != nil {
		t.Fatalf("task_get: %v", err)
	}
	if owned["contract_version"] != int64(2) {
		t.Fatalf("task_get contract_version = %v, want 2", owned["contract_version"])
	}
}

// -- the 031 upgrade drill --

// task11Migrations returns the migration files split at 031: the
// pre-031 chain and the 031 file itself.
func task11Migrations(t *testing.T) (pre []string, m031 string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v (%d)", err, len(files))
	}
	sort.Strings(files)
	for i, f := range files {
		if strings.Contains(f, "031_") {
			return files[:i], f
		}
	}
	t.Fatal("031 migration not found")
	return nil, ""
}

// TestTask11ContractVersionUpgradeDrill: a database carrying live
// pre-1.1 tasks, claims and submissions is upgraded with 031; the
// backfill freezes each task's current contract as version 1, every
// existing engagement binds it, and behavior is unchanged (an
// upgraded claim still validates and delivers against the version it
// bound, a post-upgrade revision binds only later claims).
func TestTask11ContractVersionUpgradeDrill(t *testing.T) {
	pubTestPool(t) // presence + skip contract; throwaway DB below
	ctx := context.Background()

	dbName := "kf_task11_" + nanoSuffix()
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

	pre, m031 := task11Migrations(t)
	for _, f := range pre {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	// seed a live pre-1.1 shape: publisher + agent, an OPEN task with
	// the v1 contract, an active claim on it, and the claim's
	// reservation
	pubKey := sha256.Sum256([]byte("task11-pub"))
	agentKey := sha256.Sum256([]byte("task11-agent"))
	var publisherID, agentID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('task11pub', 'x', $1, 'ab12', 100000)
		RETURNING id`, pubKey[:]).Scan(&publisherID); err != nil {
		t.Fatalf("seed publisher: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, password_hash, api_key_hash, api_key_last4, balance)
		VALUES ('task11agent', 'x', $1, 'cd34', 0)
		RETURNING id`, agentKey[:]).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	v1JSON := fmt.Sprintf(`{"title":"legacy","requirements":"do it","receiver":{"url":%q},"price":5,"claim":{"required":true}}`, okReceiverURL)
	var taskID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_task (code, publisher_id, status, budget_locked, contract)
		VALUES ('task11upg0', $1, 'open', 100, $2)
		RETURNING id`, publisherID, []byte(v1JSON)).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	var legacyClaimID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO tb_task_claim (task_id, agent_id, expires_at, deadline, amount, status)
		VALUES ($1, $2, NOW() + interval '1 hour', NOW() + interval '2 hours', 5, 'active')
		RETURNING claim_id`, taskID, agentID).Scan(&legacyClaimID); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// the claim's reservation, as work_claim would have taken it
	if _, err := db.Exec(ctx, `UPDATE tb_task SET reserved = 5 WHERE id = $1`, taskID); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	// apply the shipped 031
	sqlBytes, err := os.ReadFile(m031)
	if err != nil {
		t.Fatalf("read %s: %v", m031, err)
	}
	if _, err := db.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply 031: %v", err)
	}

	// backfill assertions: version 1 exists and equals the seeded
	// contract (semantically — JSONB normalizes whitespace/key order);
	// every pre-existing row binds it
	var version int64
	var stored []byte
	if err := db.QueryRow(ctx, `
		SELECT contract_version FROM tb_task WHERE id = $1`, taskID).Scan(&version); err != nil || version != 1 {
		t.Fatalf("tb_task.contract_version = %d err=%v, want 1", version, err)
	}
	if err := db.QueryRow(ctx, `
		SELECT contract FROM task_contract_versions WHERE task_id = $1 AND version = 1`,
		taskID).Scan(&stored); err != nil {
		t.Fatalf("version 1 row: %v", err)
	}
	var seeded, backfilled map[string]any
	if err := json.Unmarshal([]byte(v1JSON), &seeded); err != nil {
		t.Fatalf("seeded json: %v", err)
	}
	if err := json.Unmarshal(stored, &backfilled); err != nil {
		t.Fatalf("backfilled json: %v", err)
	}
	if fmt.Sprint(seeded) != fmt.Sprint(backfilled) {
		t.Fatalf("version 1 contract = %v, want the seeded contract %v", backfilled, seeded)
	}
	var claimVersion int64
	if err := db.QueryRow(ctx, `
		SELECT contract_version FROM tb_task_claim WHERE claim_id = $1`, legacyClaimID).
		Scan(&claimVersion); err != nil || claimVersion != 1 {
		t.Fatalf("legacy claim contract_version = %d err=%v, want 1", claimVersion, err)
	}

	// upgraded behavior: publish v2 through the service, then submit
	// under the UPGRADED claim — checked and delivered against v1
	rec2 := versionTestReceiver(t, `{"message":"v2-here"}`)
	if _, err := PauseTask(ctx, db, publisherID, "task11upg0"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	v2 := claimContract()
	v2.Title = "revised after upgrade"
	v2.Output.Schema = []byte(v2Schema)
	v2.Receiver.URL = rec2.URL
	if _, err := UpdateTask(ctx, db, publisherID, "task11upg0", v2); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := OpenTask(ctx, db, publisherID, "task11upg0"); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	legacyClaim := WireID(legacyClaimID)
	// v1 requirements had no schema: any object payload was valid
	// under v1; a payload that only v2 accepts still passes the v1
	// check and goes to the v1 receiver
	view, err := SubmitWork(ctx, db, agentID, SubmitInput{
		Code: "task11upg0", RequestKey: "upgraded-claim", Payload: []byte(v2Payload),
		ClaimID: &legacyClaim,
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("upgraded claim submit: %v", err)
	}
	if view.State != task.SubSettled || view.ContractVersion != 1 || view.Amount != 5 {
		t.Fatalf("upgraded claim submission = %+v, want settled / version 1 / amount 5", view)
	}
	if view.Reply == nil || !strings.Contains(view.Reply.Body, "accepted") {
		t.Fatalf("upgraded claim delivered to the wrong receiver: %+v", view.Reply)
	}
	if err := task.CheckInvariants(ctx, db, taskID); err != nil {
		t.Fatalf("CheckInvariants after upgrade flow: %v", err)
	}

	// and a claim formed after the revision binds v2
	claim2, err := ClaimTask(ctx, db, agentID, "task11upg0", time.Now())
	if err != nil {
		t.Fatalf("post-upgrade claim: %v", err)
	}
	if claim2.ContractVersion != 2 {
		t.Fatalf("post-upgrade claim binds version %d, want 2", claim2.ContractVersion)
	}
}

// nanoSuffix names the throwaway database of one drill run.
func nanoSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
}
