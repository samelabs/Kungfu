package mcpserver

// Lenient request edge ("strict out, lenient in").
//
// The 2026-07-28 Streamable HTTP binding asks every client for an Accept
// header naming both JSON and SSE, the Mcp-Method / Mcp-Name headers,
// the Mcp-Protocol-Version header and a params._meta triple. Spec-aware
// MCP clients send all of it; an agent writing a plain HTTP request
// usually sends none of it. normalizeRequest fills in ONLY what is
// missing, from the request itself, before authentication and before
// the SDK sees the request:
//
//   - Content-Type: application/json when the body is a JSON object
//     (curl -d defaults to form encoding).
//   - Accept: "application/json, text/event-stream" when it does not
//     literally name both (responses are plain JSON: JSONResponse).
//   - Mcp-Method / Mcp-Name derived from the JSON-RPC body.
//   - Mcp-Protocol-Version and params._meta (protocolVersion,
//     clientInfo, clientCapabilities) defaulted to this server's single
//     protocol revision, so every request takes the same modern path.
//
// Values the client DID send are never overwritten: a header that
// disagrees with the body is still rejected by the SDK's consistency
// check, and an unsupported protocol version is still refused. The
// derived headers come from the body itself, so public/private
// classification cannot be lied to. Batches are left untouched (they
// must carry their own headers).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo      = "io.modelcontextprotocol/clientInfo"
	metaClientCaps      = "io.modelcontextprotocol/clientCapabilities"
)

// errBodyTooLarge signals the 1 MiB request cap.
var errBodyTooLarge = errors.New("request body too large")

// normalizeRequest returns the request with missing protocol details
// filled in; errBodyTooLarge means the body exceeded the 1 MiB cap.
func normalizeRequest(r *http.Request) (*http.Request, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, MaxRequestBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	r = r.Clone(r.Context())
	h := r.Header

	msg, isObject := decodeCall(raw)
	if isObject {
		if ct, _, _ := mime.ParseMediaType(h.Get("Content-Type")); ct != "application/json" {
			h.Set("Content-Type", "application/json")
		}
	}
	if !acceptsBoth(h.Values("Accept")) {
		h.Set("Accept", "application/json, text/event-stream")
	}

	if msg != nil && msg.Method != "" {
		if h.Get("Mcp-Method") == "" {
			h.Set("Mcp-Method", msg.Method)
		}
		if msg.Method == "tools/call" && h.Get("Mcp-Name") == "" {
			if name, ok := msg.Params["name"].(string); ok && name != "" {
				h.Set("Mcp-Name", name)
			}
		}
		if msg.isCall() {
			raw = fillProtocolVersion(h, msg, raw)
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return r, nil
}

// rpcCall is the part of a single JSON-RPC message the edge reads.
type rpcCall struct {
	ID     json.RawMessage
	Method string
	Params map[string]interface{}
	all    map[string]interface{}
}

func (c *rpcCall) isCall() bool {
	id := bytes.TrimSpace(c.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// decodeCall parses a single JSON-RPC object. Numbers stay json.Number
// so re-encoding never changes an integer argument.
func decodeCall(raw []byte) (*rpcCall, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var all map[string]interface{}
	if err := dec.Decode(&all); err != nil || dec.More() {
		return nil, false
	}
	c := &rpcCall{all: all}
	c.Method, _ = all["method"].(string)
	c.Params, _ = all["params"].(map[string]interface{})
	if id, ok := all["id"]; ok {
		c.ID, _ = json.Marshal(id)
	}
	return c, true
}

// fillProtocolVersion aligns the Mcp-Protocol-Version header and the
// params._meta triple, filling whichever side is missing. When the body
// changes it is re-encoded; otherwise the original bytes are kept.
func fillProtocolVersion(h http.Header, c *rpcCall, raw []byte) []byte {
	params := c.Params
	if params == nil {
		params = map[string]interface{}{}
	}
	meta, _ := params["_meta"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
	}
	headerVersion := h.Get("Mcp-Protocol-Version")
	metaVersion, _ := meta[metaProtocolVersion].(string)

	switch {
	case headerVersion == "" && metaVersion == "":
		headerVersion, metaVersion = ProtocolVersion, ProtocolVersion
	case headerVersion == "":
		headerVersion = metaVersion
	case metaVersion == "":
		metaVersion = headerVersion
	}
	h.Set("Mcp-Protocol-Version", headerVersion)

	changed := false
	if _, ok := meta[metaProtocolVersion].(string); !ok {
		meta[metaProtocolVersion] = metaVersion
		changed = true
	}
	if _, ok := meta[metaClientInfo]; !ok {
		meta[metaClientInfo] = map[string]interface{}{"name": "http-client", "version": "0"}
		changed = true
	}
	if _, ok := meta[metaClientCaps]; !ok {
		meta[metaClientCaps] = map[string]interface{}{}
		changed = true
	}
	if !changed {
		return raw
	}
	params["_meta"] = meta
	c.all["params"] = params
	out, err := json.Marshal(c.all)
	if err != nil {
		return raw
	}
	return out
}

// acceptsBoth reports whether the Accept values literally name both
// application/json and text/event-stream (what the SDK requires).
func acceptsBoth(values []string) bool {
	var jsonOK, sseOK bool
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			switch mt {
			case "application/json":
				jsonOK = true
			case "text/event-stream":
				sseOK = true
			}
		}
	}
	return jsonOK && sseOK
}
