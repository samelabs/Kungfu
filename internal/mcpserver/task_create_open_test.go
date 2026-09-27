package mcpserver

// WO-11: task_create gains open=true (create + open in one call) and a
// 20-per-hour publisher rate limit.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// w11Register seeds a balance-carrying publisher bot directly (the
// register tool round-trip is covered elsewhere; here it is fixture).
func w11Register(t *testing.T, pool *pg.Pool) (string, string, int64) {
	t.Helper()
	name := fmt.Sprintf("w11_%d", time.Now().UnixNano())
	digest := sha256.Sum256([]byte(name))
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, balance)
		VALUES ($1, $2, 'w11x', 'x', 200) RETURNING id`,
		name, digest[:]).Scan(&id); err != nil {
		t.Fatalf("seed publisher: %v", err)
	}
	return "", name, id
}

// w11Args is the minimal 3-field contract call body.
func w11Args(budget int64, open bool) []byte {
	raw, _ := json.Marshal(map[string]any{
		"contract": map[string]any{
			"title":     "Summarize a page",
			"objective": "Three bullets of the page.",
			"price":     5,
		},
		"budget": budget,
		"open":   open,
	})
	return raw
}

func TestTaskCreateOpenInOneCall(t *testing.T) {
	pool := m1TestPool(t)
	deps := m1Deps(t, pool, nil)
	pubKey, _, pubID := w11Register(t, pool)
	ctx := context.Background()

	env, status := CallTool(ctx, &deps, "task_create", wo7Bot(t, pool, pubID), w11Args(5, true))
	if status != 200 || env["ok"] != true || env["status"] != task.TaskOpen {
		t.Fatalf("task_create open=true: %d %v", status, env)
	}
	code, _ := env["code"].(string)
	// the version snapshot landed in the same call
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Version != 1 {
		t.Fatalf("version = %d, want 1", tr.Version)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
	_ = pubKey

	// open=false keeps the draft (the default)
	env, status = CallTool(ctx, &deps, "task_create", wo7Bot(t, pool, pubID), w11Args(5, false))
	if status != 200 || env["status"] != task.TaskDraft {
		t.Fatalf("task_create open=false: %d %v", status, env)
	}
}

func TestTaskCreateRateLimit20PerHour(t *testing.T) {
	pool := m1TestPool(t)
	enabled := true
	limiter := ratelimit.NewLimiter(map[string]ratelimit.Config{
		"task_create": {Window: 3600, Limit: 20, Enabled: enabled},
	})
	deps := m1Deps(t, pool, limiter)
	_, _, pubID := w11Register(t, pool)
	bot := wo7Bot(t, pool, pubID)

	// 20 publishes pass, the 21st is RATE_LIMIT
	for i := 0; i < 20; i++ {
		env, status := CallTool(context.Background(), &deps, "task_create", bot, w11Args(5, false))
		if status != 200 || env["ok"] != true {
			t.Fatalf("create %d: %d %v", i+1, status, env)
		}
	}
	env, status := CallTool(context.Background(), &deps, "task_create", bot, w11Args(5, false))
	if status != 429 || env["ok"] != false || env["error"].(map[string]any)["code"] != "RATE_LIMIT" {
		t.Fatalf("21st create: %d %v, want 429 RATE_LIMIT", status, env)
	}
	if env["next_action"] != nil {
		t.Fatalf("next_action = %v, want null (publisher tools)", env["next_action"])
	}
}
