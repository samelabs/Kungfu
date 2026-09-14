package delivery

// Deterministic bounded-read proofs: a custom RoundTripper returns a
// synthetic response whose body counts every byte actually read. No
// sockets, no timing — the counter is the proof.
//
// The sharedClient is swapped for the duration of each test and restored
// unconditionally (t.Cleanup), so no other test is affected.

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// countingBody wraps a fixed payload and counts the bytes the consumer
// actually read (including reads internal to http.Client machinery).
type countingBody struct {
	payload []byte
	read    int
	readOps int
	closed  bool
}

func (c *countingBody) Read(p []byte) (int, error) {
	if c.read >= len(c.payload) {
		return 0, io.EOF
	}
	c.readOps++
	n := copy(p, c.payload[c.read:])
	c.read += n
	return n, nil
}

func (c *countingBody) Close() error {
	c.closed = true
	return nil
}

// stubTransport returns one canned response per test.
type stubTransport struct {
	resp *http.Response
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.resp, nil
}

// withStubClient swaps sharedClient for one answering with the given
// response and restores the original afterwards.
func withStubClient(t *testing.T, resp *http.Response) {
	t.Helper()
	orig := sharedClient
	sharedClient = &http.Client{Transport: &stubTransport{resp: resp}}
	t.Cleanup(func() { sharedClient = orig })
}

func makeBody(n int) *countingBody {
	return &countingBody{payload: bytes.Repeat([]byte("a"), n)}
}

// 2xx: success, bounded read, captured cap, body closed.
func TestPostJSONBoundedReadDeterministic2xx(t *testing.T) {
	const total = 1 << 20 // 1MB >> 65536
	body := makeBody(total)
	withStubClient(t, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	})

	res := PostJSON("http://stub.local/hook", []byte(`{}`), TestTaskErrorConfig())

	if !res.Success {
		t.Fatalf("2xx must succeed: %v", res.ErrorCode)
	}
	if body.read > 65536 {
		t.Fatalf("underlying bytes read = %d, must be <= 65536", body.read)
	}
	if body.read >= total {
		t.Fatalf("underlying bytes read = %d, must be < total %d", body.read, total)
	}
	if res.ResponseBody == nil || len(*res.ResponseBody) != 65535 {
		t.Fatalf("captured body = %d bytes, want exactly 65535", len(derefStrPtr(res.ResponseBody)))
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

// 500: existing rejection semantics, bounded read, captured cap, closed.
func TestPostJSONBoundedReadDeterministic500(t *testing.T) {
	const total = 1 << 20
	body := makeBody(total)
	withStubClient(t, &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
		Body:       body,
	})

	res := PostJSON("http://stub.local/hook", []byte(`{}`), AgentSubmitErrorConfig())

	if res.Success {
		t.Fatal("500 must not succeed")
	}
	if res.ErrorCode != "POSTAPI_REJECTED" {
		t.Fatalf("error code = %s, want POSTAPI_REJECTED", res.ErrorCode)
	}
	if body.read > 65536 {
		t.Fatalf("underlying bytes read = %d, must be <= 65536", body.read)
	}
	if body.read >= total {
		t.Fatalf("underlying bytes read = %d, must be < total %d", body.read, total)
	}
	if res.ResponseBody != nil && len(*res.ResponseBody) > 65535 {
		t.Fatalf("captured body = %d bytes, must be <= 65535", len(*res.ResponseBody))
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func derefStrPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
