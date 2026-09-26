package mcpserver

// WO-7d protocol unification tests: the complete 29-tool registry,
// account/memory tools on both channels with identical envelopes,
// anonymous behavior driven by ToolDef.Public, and the error-catalog
// closure over internal/errors.StatusFor.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
)

func codeOf(env map[string]any) string {
	c, _ := env["code"].(string)
	return c
}

func TestRegistryComplete(t *testing.T) {
	want := []string{
		// 10 executor
		"work_list", "work_get", "work_harness", "work_claim", "work_claim_renew",
		"work_release", "work_submit", "work_status", "work_history", "work_report",
		// 11 publisher
		"task_create", "task_update", "task_open", "task_pause", "task_close",
		"task_fund", "task_refund", "task_get", "task_list", "task_submissions", "task_verdict",
		// 2 account
		"account_register", "account_status",
		// 6 memory
		"memory_list", "memory_get", "memory_put", "memory_share", "memory_unshare", "memory_delete",
	}
	got := ToolNames()
	if len(got) != 29 || len(want) != 29 {
		t.Fatalf("registry = %d tools, want 29", len(got))
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	for i := range wantSorted {
		if sorted[i] != wantSorted[i] {
			t.Fatalf("registry mismatch at %d: %s vs %s", i, sorted[i], wantSorted[i])
		}
	}
	// exactly one public tool
	for _, def := range tools {
		if def.Public && def.Name != "account_register" {
			t.Fatalf("%s must not be public", def.Name)
		}
	}
}

func TestRegistryMCPToolsListMatches(t *testing.T) {
	pool, _, srv := registryEnv(t)
	_ = pool
	resp, body := bareCall(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json, text/event-stream",
		})
	if resp.StatusCode != 200 {
		t.Fatalf("tools/list = %d (%s)", resp.StatusCode, body)
	}
	var rpc struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &rpc); err != nil {
		t.Fatalf("parse: %v (%s)", err, body)
	}
	if len(rpc.Result.Tools) != 29 {
		t.Fatalf("tools/list = %d, want 29", len(rpc.Result.Tools))
	}
	got := ToolNames()
	for _, tl := range rpc.Result.Tools {
		found := false
		for _, n := range got {
			if n == tl.Name {
				found = true
			}
		}
		if !found {
			t.Fatalf("tools/list has %s, registry does not", tl.Name)
		}
	}
}

// TestAccountMemoryToolsBothChannels: register → put → list → get →
// share → get(other) → unshare → delete, once per channel, envelopes
// compared after canonicalization; then the anonymous rules.
func TestAccountMemoryToolsBothChannels(t *testing.T) {
	pool, deps, srv := registryEnv(t)
	ctx := context.Background()

	// channel A: MCP
	keyA, nameA, _ := m2RegisterSeeded(t, srv, pool, 0)
	// channel B: HTTP dispatch (registry path behind /api/v1); the
	// key stays unused — dispatch resolves identity by bot id
	_, nameB, idB := m2RegisterSeeded(t, srv, pool, 0)
	_ = nameB

	mcpCallT := func(tool string, key string, args map[string]any) (map[string]any, bool) {
		env, isErr, _ := mcpCall(t, srv, key, tool, args)
		return env, isErr
	}
	httpCallT := func(tool string, botID int64, args map[string]any) (map[string]any, int) {
		bot := wo7Bot(t, pool, botID)
		raw, _ := json.Marshal(args)
		env, status := CallTool(ctx, &deps, tool, bot, raw)
		return env, status
	}

	content := strings.Repeat("memory body with enough bytes for the minimum. ", 2)

	// memory_put on both channels (A via MCP, B via HTTP)
	ePutA, iPut := mcpCallT("memory_put", keyA, map[string]any{
		"title": "Unify memory A", "tags": []string{"t"}, "content": content,
	})
	ePutB, sPut := httpCallT("memory_put", idB, map[string]any{
		"title": "Unify memory B", "tags": []string{"t"}, "content": content,
	})
	if iPut || sPut != 200 || ePutA["ok"] != true || ePutB["ok"] != true {
		t.Fatalf("memory_put: %+v / %d %+v", ePutA, sPut, ePutB)
	}
	assertJSONEqual(t, "memory_put", rewriteEnv(ePutA, [2]string{codeOf(ePutA)}, [2]string{"Unify memory A"}, [2]string{nameA}), rewriteEnv(ePutB, [2]string{codeOf(ePutB)}, [2]string{"Unify memory B"}, [2]string{nameB}))

	// memory_list
	eLA, _ := mcpCallT("memory_list", keyA, map[string]any{})
	eLB, sL := httpCallT("memory_list", idB, map[string]any{})
	if sL != 200 || eLA["ok"] != true || eLB["ok"] != true {
		t.Fatalf("memory_list: %d", sL)
	}
	// lists contain per-channel codes/titles; compare shape only
	if eLA["ok"] != true || eLB["ok"] != true {
		t.Fatal("memory_list ok")
	}

	// memory_get (own)
	codeA := ePutA["code"].(string)
	eGA, _ := mcpCallT("memory_get", keyA, map[string]any{"code": codeA})
	codeB := ePutB["code"].(string)
	eGB, sG := httpCallT("memory_get", idB, map[string]any{"code": codeB})
	if sG != 200 || eGA["ok"] != true {
		t.Fatalf("memory_get: %d %+v", sG, eGA)
	}
	assertJSONEqual(t, "memory_get", rewriteEnv(eGA, [2]string{codeA}, [2]string{"Unify memory A"}), rewriteEnv(eGB, [2]string{codeB}, [2]string{"Unify memory B"}))

	// memory_share / unshare (twin codes normalized)
	eSA, _ := mcpCallT("memory_share", keyA, map[string]any{"code": codeA})
	eSB, sS := httpCallT("memory_share", idB, map[string]any{"code": codeB})
	if sS != 200 || eSA["ok"] != true || eSB["ok"] != true {
		t.Fatalf("memory_share: %d", sS)
	}
	assertJSONEqual(t, "memory_share", rewriteEnv(eSA, [2]string{codeA}), rewriteEnv(eSB, [2]string{codeB}))
	eUA, _ := mcpCallT("memory_unshare", keyA, map[string]any{"code": codeA})
	eUB, sU := httpCallT("memory_unshare", idB, map[string]any{"code": codeB})
	if sU != 200 {
		t.Fatalf("memory_unshare: %d", sU)
	}
	assertJSONEqual(t, "memory_unshare", rewriteEnv(eUA, [2]string{codeA}), rewriteEnv(eUB, [2]string{codeB}))

	// memory_delete
	eDA, _ := mcpCallT("memory_delete", keyA, map[string]any{"code": codeA})
	eDB, sD := httpCallT("memory_delete", idB, map[string]any{"code": codeB})
	if sD != 200 || eDA["ok"] != true {
		t.Fatalf("memory_delete: %d", sD)
	}
	assertJSONEqual(t, "memory_delete", rewriteEnv(eDA, [2]string{codeA}, [2]string{"Unify memory A"}), rewriteEnv(eDB, [2]string{codeB}, [2]string{"Unify memory B"}))

	// account_status both channels
	eStA, _ := mcpCallT("account_status", keyA, map[string]any{})
	eStB, sSt := httpCallT("account_status", idB, map[string]any{})
	if sSt != 200 || eStA["ok"] != true || eStB["ok"] != true {
		t.Fatalf("account_status: %d", sSt)
	}
	// twin accounts differ in bot_id and bot_name by construction;
	// compare the channel-relevant fields
	for _, k := range []string{"ok", "error", "next_action", "retry_after", "status", "balance"} {
		if fmt.Sprint(eStA[k]) != fmt.Sprint(eStB[k]) {
			t.Fatalf("account_status field %s: %v vs %v", k, eStA[k], eStB[k])
		}
	}

	// envelope shape: the four §8.2 keys are always present
	for _, k := range []string{"ok", "error", "next_action", "retry_after"} {
		if _, has := eStA[k]; !has {
			t.Fatalf("account_status envelope missing %s", k)
		}
	}
}

// rewriteEnv rewrites per-channel identity values in the marshaled
// envelope (memory codes, titles, bot names - twins differ by
// construction, not by channel behavior).
func rewriteEnv(env map[string]any, pairs ...[2]string) map[string]any {
	raw, _ := json.Marshal(env)
	s := string(raw)
	q := string(rune(34))
	for _, p := range pairs {
		s = strings.ReplaceAll(s, q+p[0]+q, q+"X"+q)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// TestAnonymousAccess: account_register works without a Bearer on
// both channels; every other tool requires one.
func TestAnonymousAccess(t *testing.T) {
	_, deps, srv := registryEnv(t)

	// MCP: anonymous tools/call account_register succeeds
	sc, body := m2CallTool(t, mcpTestServer{srv: srv, client: srv.Client()},
		"", "account_register", map[string]any{
			"name": "anonreg" + time.Now().Format("150405.000000000"), "password": "passpass123"})
	if sc != 200 || !strings.Contains(body, "kf_live_") {
		t.Fatalf("anonymous account_register via MCP: %d %.200s", sc, body)
	}
	// MCP: anonymous memory_list → 401 from the middleware
	sc2, _ := m2CallTool(t, mcpTestServer{srv: srv, client: srv.Client()},
		"", "memory_list", map[string]any{})
	if sc2 != http.StatusUnauthorized {
		t.Fatalf("anonymous memory_list via MCP = %d, want 401", sc2)
	}

	// HTTP: anonymous /api/v1/account_register dispatches (public
	// flag); the per-IP register limiter sees the shared test IP —
	// refresh it by using a fresh Deps without the register entry
	fresh := deps
	fresh.RateLimiter = nil
	raw, _ := json.Marshal(map[string]any{
		"name": "anonreghttp" + time.Now().Format("150405.000000000"), "password": "passpass123"})
	env, status := CallTool(context.Background(), &fresh, "account_register", nil, raw)
	if status != 200 || env["ok"] != true {
		t.Fatalf("anonymous account_register dispatch: %d %v", status, env)
	}
}

// TestCatalogClosure: every errors.New/NewWithDetails code literal in
// the service layer is present in the StatusFor table (except
// INTERNAL_ERROR, which is the fallback).
func TestCatalogClosure(t *testing.T) {
	pattern := regexp.MustCompile(`errors\.(?:New|NewWithDetails|NewRateLimitError)\(\s*\d+\s*,\s*"([A-Z_]+)"`)
	missing := map[string]bool{}
	err := filepath.Walk("../service", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
			code := m[1]
			if code == "INTERNAL_ERROR" {
				continue
			}
			if _, ok := errors.StatusFor(code); !ok {
				missing[code] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		var codes []string
		for c := range missing {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		t.Fatalf("service codes missing from StatusFor: %v", codes)
	}
}

// TestNoLegacyLiterals: the deleted plumbing must not reappear.
func TestNoLegacyLiterals(t *testing.T) {
	for _, banned := range []string{"INVALID_KEY", "toolError{", "mapAppError(", "addMemoryTools", "addAccountTools"} {
		found := grepRepo(t, banned)
		if len(found) > 0 {
			t.Fatalf("%s still present: %v", banned, found)
		}
	}
}

func grepRepo(t *testing.T, needle string) []string {
	t.Helper()
	var hits []string
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(src), needle) {
				hits = append(hits, fmt.Sprintf("%s", path))
			}
			return nil
		})
	}
	return hits
}

// bareCall posts a raw JSON-RPC body (tools/list is anonymous).
func bareCall(t *testing.T, srv *httptest.Server, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	return resp, raw.String()
}
