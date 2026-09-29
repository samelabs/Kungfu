package service

// ListKungfusForBot search tests (WO-21): keyword over title/tags/
// description, exact code, matching total, and no list operation log.

import (
	"context"
	"testing"
)

func TestListKungfusForBotSearch(t *testing.T) {
	pool := pubTestPool(t)
	bot := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	seed := func(code, title, tags, description string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tb_kungfus (code, bot_id, title, tags_json, description, content, checksum, visibility, status)
			VALUES ($1, $2, $3, $4::jsonb, $5, 'content body', $1, 'private', 'active')`,
			code, bot, title, tags, description); err != nil {
			t.Fatalf("seed %s: %v", code, err)
		}
	}
	seed("w21alpha0001", "Alpha workflow", `["searching"]`, "summarizes pages")
	seed("w21beta00002", "Beta skill", `["other"]`, "contains keyword ALPHA in the description")
	seed("w21gamma0003", "Gamma script", `["matched-tag"]`, "unrelated")

	res, err := ListKungfusForBot(ctx, pool, bot, "alpha", "", 20, 0)
	if err != nil {
		t.Fatalf("q: %v", err)
	}
	// title hit + description hit (case-insensitive), no tag hit
	if total := res["meta"].(map[string]interface{})["total"].(int64); total != 2 {
		t.Fatalf("q=alpha total = %d, want 2", total)
	}

	// tag match
	res, err = ListKungfusForBot(ctx, pool, bot, "matched-tag", "", 20, 0)
	if err != nil {
		t.Fatalf("q tag: %v", err)
	}
	if total := res["meta"].(map[string]interface{})["total"].(int64); total != 1 {
		t.Fatalf("q=matched-tag total = %d, want 1", total)
	}

	// LIKE wildcards match literally: % and _ are not operators
	seed("w21wild00004", "100% done", `[]`, "wildcard title")
	res, err = ListKungfusForBot(ctx, pool, bot, "100%", "", 20, 0)
	if err != nil {
		t.Fatalf("q wildcard: %v", err)
	}
	if total := res["meta"].(map[string]interface{})["total"].(int64); total != 1 {
		t.Fatalf("q=100%% total = %d, want 1 (literal match only)", total)
	}

	// exact code: 12-hex style
	res, err = ListKungfusForBot(ctx, pool, bot, "", "w21gamma0003", 20, 0)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	items := res["kungfus"].([]map[string]interface{})
	if len(items) != 1 || items[0]["code"] != "w21gamma0003" {
		t.Fatalf("code exact: %v", items)
	}

	// no filter: everything, with paging intact
	res, err = ListKungfusForBot(ctx, pool, bot, "", "", 2, 2)
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if total := res["meta"].(map[string]interface{})["total"].(int64); total != 4 {
		t.Fatalf("unfiltered total = %d, want 4", total)
	}

	// the list writes no operation log
	var n int64
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_logs WHERE bot_id = $1 AND action = 'kungfus_list'`, bot).Scan(&n)
	if n != 0 {
		t.Fatalf("list wrote %d operation logs, want 0", n)
	}
}
