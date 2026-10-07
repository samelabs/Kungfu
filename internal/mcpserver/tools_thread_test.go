package mcpserver

// Thread tools (D2) wiring tests: registry presence and schema
// shape, the §8.2 envelope over the dispatch path, the one-time key
// disclosure with idempotent replay, handler-level protocol error
// codes (the wire masking of the new codes is the reported D2 stop
// point — see tools_thread.go's header note), and the join failure
// limiter (§2: 20 failures / 15 min, uniform KEY_INVALID).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

var d2ThreadTools = []string{
	"thread_start", "thread_key", "thread_key_revoke", "thread_join",
	"thread_leave", "thread_remove", "thread_set_role", "thread_close",
	"thread_get", "thread_list",
}

// TestThreadToolsRegistered: the ten D2 tools are in the single
// registry, none public, every schema a JSON object, every write
// tool accepting idempotency_key.
func TestThreadToolsRegistered(t *testing.T) {
	reg := map[string]ToolDef{}
	for _, def := range tools {
		reg[def.Name] = def
	}
	for _, name := range d2ThreadTools {
		def, ok := reg[name]
		if !ok {
			t.Fatalf("%s missing from the registry", name)
		}
		if def.Public {
			t.Fatalf("%s must require the Agent key", name)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(def.InputSchema), &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema root is not an object", name)
		}
		if def.Handler == nil {
			t.Fatalf("%s has no handler", name)
		}
	}
	// every write tool carries the L3 parameter
	for _, name := range d2ThreadTools[:8] {
		def := reg[name]
		if !strings.Contains(def.InputSchema, "idempotency_key") {
			t.Fatalf("%s schema lacks idempotency_key", name)
		}
	}
}

// d2Cleanup removes a test's thread rows before its bots.
func d2Cleanup(t *testing.T, pool *pg.Pool, bots []int64, codes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, code := range codes {
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE thread_id IN (SELECT id FROM threads WHERE code = $1)`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE code = $1`, code)
	}
	for _, id := range bots {
		_, _ = pool.Exec(ctx, `DELETE FROM thread_members WHERE account_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM thread_idempotency WHERE account_id = $1`, id)
	}
}

// d2SeedBot inserts a fresh active bot directly and loads it for the
// dispatch path.
func d2SeedBot(t *testing.T, pool *pg.Pool) (int64, *model.Bot) {
	t.Helper()
	name := "d2thr_" + time.Now().Format("150405.000000000") + "_" + time.Now().Format("150405")
	digest := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		VALUES ($1, $2, 'd2d2', 'x', 'active', 0) RETURNING id`,
		name, digest[:]).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, id)
	})
	return id, wo7Bot(t, pool, id)
}

// d2Call drives the registry dispatch (the path behind /api/v1).
func d2Call(t *testing.T, deps *Deps, bot *model.Bot, tool string, args map[string]any) (map[string]any, int) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return CallTool(context.Background(), deps, tool, bot, raw)
}

// d2ErrCode reads the protocol code off a handler error (AppError or
// ToolError — pre-envelope, before any masking).
func d2ErrCode(err error) string {
	if ae, ok := apperr.IsAppError(err); ok {
		return ae.Code
	}
	if te, ok := err.(*ToolError); ok {
		return te.Code
	}
	return ""
}

// d2HandlerErr invokes the bound handler directly so the protocol
// error CODE is visible (the envelope masks the new thread codes —
// the reported D2 stop point).
func d2HandlerErr(t *testing.T, deps *Deps, bot *model.Bot, tool string, args map[string]any) error {
	t.Helper()
	def, ok := Tool(tool)
	if !ok {
		t.Fatalf("unknown tool %s", tool)
	}
	raw, _ := json.Marshal(args)
	_, err := def.bind(deps)(context.Background(), bot, raw)
	return err
}

// TestThreadStartEnvelopeAndReplay: the §8.2 envelope over the
// dispatch path — ok, api_version, null next_action (account/memory
// convention), next[] capability list — and the L3 replay: first
// response carries the raw key, the replay returns the stored facts
// with the key emptied and the fingerprint kept.
func TestThreadStartEnvelopeAndReplay(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	ownerID, owner := d2SeedBot(t, pool)
	args := map[string]any{"subject": "d2 envelope", "key": true, "idempotency_key": "d2-env-1"}
	defer d2Cleanup(t, pool, []int64{ownerID})

	env, status := d2Call(t, &deps, owner, "thread_start", args)
	if status != 200 || env["ok"] != true {
		t.Fatalf("thread_start = %d %v", status, env)
	}
	for _, k := range []string{"ok", "error", "next_action", "retry_after", "api_version"} {
		if _, has := env[k]; !has {
			t.Fatalf("envelope missing %s: %v", k, env)
		}
	}
	if env["next_action"] != nil {
		t.Fatalf("successful thread_start next_action = %v, want null", env["next_action"])
	}
	rawKey, _ := env["key"].(string)
	if !strings.HasPrefix(rawKey, "kf_") {
		t.Fatalf("first response key = %q", rawKey)
	}
	fingerprint, _ := env["key_fingerprint"].(string)
	if fingerprint == "" {
		t.Fatal("first response lacks key_fingerprint")
	}
	next, _ := env["next"].([]string)
	if len(next) == 0 {
		t.Fatalf("first response next = %v", next)
	}

	// L3 replay: same key, same request — stored facts, no raw key
	env2, status2 := d2Call(t, &deps, owner, "thread_start", args)
	if status2 != 200 || env2["ok"] != true {
		t.Fatalf("replay = %d %v", status2, env2)
	}
	if env2["key"] != "" {
		t.Fatalf("replay re-disclosed the key: %v", env2["key"])
	}
	if env2["key_fingerprint"] != fingerprint {
		t.Fatalf("replay fingerprint = %v, want %v", env2["key_fingerprint"], fingerprint)
	}
	if env2["subject"] != "d2 envelope" {
		t.Fatalf("replay subject = %v", env2["subject"])
	}

	// same key, different request → IDEMPOTENCY_CONFLICT (this code
	// IS in the wire table)
	env3, status3 := d2Call(t, &deps, owner, "thread_start",
		map[string]any{"subject": "other", "key": false, "idempotency_key": "d2-env-1"})
	if status3 != 409 || env3["ok"] != false {
		t.Fatalf("idempotency conflict = %d %v", status3, env3)
	}
	if errObj := env3["error"].(map[string]any); errObj["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict code = %v", errObj["code"])
	}
	if env3["next_action"] != "retry" {
		t.Fatalf("conflict next_action = %v, want retry", env3["next_action"])
	}
}

// TestThreadJoinAndGetOverDispatch: join by key and the working-set
// read over the dispatch path; handler-level protocol codes for the
// A16 refusals; the masked envelope still steers with next_action
// (stop for NOT_MEMBER).
func TestThreadJoinAndGetOverDispatch(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	ownerID, owner := d2SeedBot(t, pool)
	guestID, guest := d2SeedBot(t, pool)
	outsiderID, outsider := d2SeedBot(t, pool)
	defer d2Cleanup(t, pool, []int64{ownerID, guestID, outsiderID})

	startEnv, status := d2Call(t, &deps, owner, "thread_start",
		map[string]any{"subject": "join flow", "key": true})
	if status != 200 || startEnv["ok"] != true {
		t.Fatalf("thread_start = %d %v", status, startEnv)
	}
	code := startEnv["thread"].(string)
	rawKey := startEnv["key"].(string)

	joinEnv, statusJ := d2Call(t, &deps, guest, "thread_join",
		map[string]any{"key": rawKey, "idempotency_key": "d2-join-1"})
	if statusJ != 200 || joinEnv["ok"] != true || joinEnv["role"] != "speaker" {
		t.Fatalf("thread_join = %d %v", statusJ, joinEnv)
	}

	getEnv, statusG := d2Call(t, &deps, guest, "thread_get", map[string]any{"thread": code})
	if statusG != 200 || getEnv["ok"] != true {
		t.Fatalf("thread_get = %d %v", statusG, getEnv)
	}
	members := getEnv["members"].([]map[string]any)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	if tl := getEnv["timeline"].([]map[string]any); len(tl) != 0 {
		t.Fatalf("timeline = %v", tl)
	}

	listEnv, statusL := d2Call(t, &deps, guest, "thread_list", map[string]any{})
	if statusL != 200 || listEnv["ok"] != true {
		t.Fatalf("thread_list = %d %v", statusL, listEnv)
	}

	// handler-level codes: the guest cannot govern
	if err := d2HandlerErr(t, &deps, guest, "thread_close", map[string]any{"thread": code}); err == nil {
		t.Fatal("non-governor close must fail")
	} else if ae, ok := apperr.IsAppError(err); !ok || ae.Code != "NOT_GOVERNOR" {
		t.Fatalf("close by speaker: want NOT_GOVERNOR, got %v", err)
	}
	if err := d2HandlerErr(t, &deps, outsider, "thread_get", map[string]any{"thread": code}); err == nil {
		t.Fatal("non-member get must fail")
	} else if ae, ok := apperr.IsAppError(err); !ok || ae.Code != "NOT_MEMBER" {
		t.Fatalf("get by outsider: want NOT_MEMBER, got %v", err)
	}

	// D-009: the thread codes are registered in the status table, so the
	// envelope carries NOT_MEMBER itself (previously masked to
	// INTERNAL_ERROR while the codes were unregistered)
	envOut, statusOut := d2Call(t, &deps, outsider, "thread_get", map[string]any{"thread": code})
	if statusOut != 403 || envOut["ok"] != false {
		t.Fatalf("outsider get = %d %v", statusOut, envOut)
	}
	if errObj := envOut["error"].(map[string]any); errObj["code"] != "NOT_MEMBER" {
		t.Fatalf("code = %v, want NOT_MEMBER", errObj["code"])
	}
	if envOut["next_action"] != "stop" {
		t.Fatalf("outsider get next_action = %v, want stop", envOut["next_action"])
	}
}

// TestThreadJoinFailureLimiter (§2): after 20 failed joins within 15
// minutes, every further join — even with the valid key — answers
// KEY_INVALID, indistinguishable from a bad key.
func TestThreadJoinFailureLimiter(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	ownerID, owner := d2SeedBot(t, pool)
	guestID, guest := d2SeedBot(t, pool)
	defer d2Cleanup(t, pool, []int64{ownerID, guestID})

	startEnv, status := d2Call(t, &deps, owner, "thread_start", map[string]any{"key": true})
	if status != 200 {
		t.Fatalf("start: %d", status)
	}
	rawKey := startEnv["key"].(string)
	badKey := "kf_" + strings.Repeat("0", 32)

	saved := joinFailures
	joinFailures = &joinFailureWindow{hits: map[int64][]int64{}}
	defer func() { joinFailures = saved }()

	for i := 0; i < 20; i++ {
		err := d2HandlerErr(t, &deps, guest, "thread_join", map[string]any{"key": badKey})
		if err == nil {
			t.Fatalf("join %d with a bad key unexpectedly succeeded", i)
		}
		if code := d2ErrCode(err); code != "KEY_INVALID" {
			t.Fatalf("bad-key join %d: want KEY_INVALID, got %v", i, err)
		}
	}
	// the 21st attempt is refused before execution, uniformly KEY_INVALID
	err := d2HandlerErr(t, &deps, guest, "thread_join", map[string]any{"key": rawKey})
	if err == nil {
		t.Fatal("join past the failure limit must be refused")
	}
	if code := d2ErrCode(err); code != "KEY_INVALID" {
		t.Fatalf("limited join: want KEY_INVALID, got %v", err)
	}
	// and it had no effect on membership
	var memberships int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM thread_members WHERE account_id = $1`, guestID).
		Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 0 {
		t.Fatalf("limited joins created %d memberships", memberships)
	}
}
