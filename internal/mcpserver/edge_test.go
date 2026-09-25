package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Plain-HTTP contract: a JSON-RPC body plus (for protected tools) a
// Bearer key is enough. Everything else is filled in by the edge, and
// what the client does send keeps its full strictness.

// bare posts exactly what a plain HTTP client would: the body, the
// default curl -d content type, and optional extra headers.
func bare(t *testing.T, srv *httptest.Server, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, m1ReadBody(t, resp)
}

type rpcResult struct {
	Result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Tools             []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeRPC(t *testing.T, resp *http.Response, body string) rpcResult {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q (want plain JSON), body %q", ct, body)
	}
	var r rpcResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("response is not one JSON document: %v: %q", err, body)
	}
	return r
}

func TestEdgePlainHTTPEndToEnd(t *testing.T) {
	srv, _ := m1SetupServer(t)

	// 1. Discovery without any MCP headers.
	resp, body := bare(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("tools/list = %d %q", resp.StatusCode, body)
	}
	if r := decodeRPC(t, resp, body); len(r.Result.Tools) == 0 {
		t.Fatalf("no tools listed: %q", body)
	}

	// 2. Anonymous registration: the public tool is recognised from the body.
	name := fmt.Sprintf("edge_%d", time.Now().UnixNano()%1_000_000_000)
	m1CleanupBot(t, name)
	resp, body = bare(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"account_register","arguments":{"name":"`+name+`","password":"edge-pass-1"}}}`, nil)
	r := decodeRPC(t, resp, body)
	var reg struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(r.Result.StructuredContent, &reg)
	if resp.StatusCode != 200 || r.Result.IsError || !strings.HasPrefix(reg.APIKey, "kf_live_") {
		t.Fatalf("register = %d %q", resp.StatusCode, body)
	}

	// 3. A protected tool without a key is refused with a Bearer challenge.
	statusCall := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"account_status","arguments":{}}}`
	resp, _ = bare(t, srv, statusCall, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("keyless protected call = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// 4. With the key it works.
	resp, body = bare(t, srv, statusCall, map[string]string{"Authorization": "Bearer " + reg.APIKey})
	if r := decodeRPC(t, resp, body); resp.StatusCode != 200 || r.Result.IsError || !strings.Contains(string(r.Result.StructuredContent), name) {
		t.Fatalf("account_status = %d %q", resp.StatusCode, body)
	}

	// 5. Accept: application/json only (no SSE) is accepted.
	resp, body = bare(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, map[string]string{"Accept": "application/json"})
	if resp.StatusCode != 200 {
		t.Fatalf("json-only Accept = %d %q", resp.StatusCode, body)
	}
}

func TestEdgeKeepsClientSuppliedStrictness(t *testing.T) {
	srv, _ := m1SetupServer(t)

	// A header that disagrees with the body is still rejected before the
	// tool runs: the edge never overwrites what the client sent.
	resp, body := bare(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"account_status","arguments":{}}}`,
		map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "account_register"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "mismatch") {
		t.Fatalf("lying Mcp-Name = %d %q", resp.StatusCode, body)
	}

	// An explicitly requested unsupported protocol version is refused.
	resp, body = bare(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"Mcp-Protocol-Version": "2025-06-18"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("old protocol version = %d %q", resp.StatusCode, body)
	}

	// A spec-complete client gets the same plain JSON response.
	resp, body = bare(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"sdk","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`,
		map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Mcp-Method": "tools/list", "Mcp-Protocol-Version": "2026-07-28"})
	if r := decodeRPC(t, resp, body); resp.StatusCode != 200 || len(r.Result.Tools) == 0 {
		t.Fatalf("spec client = %d %q", resp.StatusCode, body)
	}
}

func TestEdgeReencodingPreservesIntegers(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"memory_list","arguments":{"limit":9007199254740993}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(in))
	out, err := normalizeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(out.Body)
	got := buf.String()
	if !strings.Contains(got, `"limit":9007199254740993`) {
		t.Fatalf("integer changed by re-encoding: %s", got)
	}
	if !strings.Contains(got, `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`) {
		t.Fatalf("_meta not filled: %s", got)
	}
	h := out.Header
	if h.Get("Mcp-Method") != "tools/call" || h.Get("Mcp-Name") != "memory_list" ||
		h.Get("Mcp-Protocol-Version") != ProtocolVersion || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers not derived: %v", h)
	}

	// Notifications (no id) get no _meta; batches are left alone.
	for _, body := range []string{`{"jsonrpc":"2.0","method":"notifications/x"}`, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`} {
		out, _ := normalizeRequest(httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
		buf.Reset()
		_, _ = buf.ReadFrom(out.Body)
		if buf.String() != body {
			t.Fatalf("body rewritten: %s", buf.String())
		}
	}
}
