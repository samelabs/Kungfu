package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestZZJudgeSchemaRepro(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	defer srv.Close()
	defer pool.Close()
	ctx := context.Background()
	_, _, idLead := m2RegisterSeeded(t, srv, pool, 0)
	_, _, idW := m2RegisterSeeded(t, srv, pool, 0)
	call := func(id int64, tool string, args map[string]any) map[string]any {
		bot := wo7Bot(t, pool, id)
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		if status != 200 || env["ok"] != true {
			t.Fatalf("%s: %d %v", tool, status, env)
		}
		return env
	}
	st := call(idLead, "thread_start", map[string]any{"subject": "repro", "key": true, "idempotency_key": "zz-s"})
	code, key := st["thread"].(string), st["key"].(string)
	call(idW, "thread_join", map[string]any{"key": key, "idempotency_key": "zz-j"})
	a := call(idLead, "thread_post", map[string]any{"thread": code, "content": "r", "ask": []int64{idW},
		"idempotency_key": "zz-a",
		"assign": map[string]any{"to": idW, "requirements": "r",
			"output_schema": `{"type":"object","required":["s"],"properties":{"s":{"type":"integer"}}}`,
			"deliver_due":   3600, "judge_due": 3600}})["assign"]
	call(idW, "assign_take", map[string]any{"assign": a, "payload": `{"s":1}`, "idempotency_key": "zz-t"})
	call(idLead, "assign_judge", map[string]any{"assign": a, "verdict": "adopt", "idempotency_key": "zz-jj"})
}
