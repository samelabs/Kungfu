package service

// D7 notify tests (A21): registration challenge, content-free signed
// dispatch on new obligations, and the harmlessness of loss — the
// turn list recomputes from facts with or without delivery.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestNotifyRegisterAndDispatch(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	var mu sync.Mutex
	var payloads []map[string]any
	var sawChallenge bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		defer mu.Unlock()
		if m["type"] == "verification" {
			sawChallenge = true
			w.WriteHeader(200)
			return
		}
		payloads = append(payloads, m)
		if r.Header.Get("X-Kungfu-Signature") == "" {
			t.Error("dispatch without signature header")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	// the test client trusts the self-signed cert of the test server
	prevClient := notifyHTTPClient
	srvClient := srv.Client()
	srvClient.Timeout = prevClient.Timeout
	notifyHTTPClient = srvClient
	t.Cleanup(func() { notifyHTTPClient = prevClient })

	// unverified endpoints are refused: a 404 server rejects first
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer bad.Close()
	if _, err := NotifyRegister(ctx, pool, a, bad.URL); err == nil {
		t.Fatal("endpoint failing the challenge must be refused")
	}
	if _, err := NotifyRegister(ctx, pool, a, "http://plain.example/x"); err == nil {
		t.Fatal("non-https endpoint must be refused")
	}

	if _, err := NotifyRegister(ctx, pool, a, srv.URL); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !sawChallenge {
		t.Fatal("registration must probe with a challenge")
	}

	// a new obligation queues a notification in the same transaction
	code, raw := threadStart(t, pool, owner, "notify", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "a, wake up", "", "", nil, []int64{a}, nil, "n1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	// A21 loss: do NOT dispatch — the turn list still has the item
	todo, err := TodoList(ctx, pool, a, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	if n := len(todo["todos"].([]map[string]any)); n != 1 {
		t.Fatalf("lost notification lost the obligation too: %d items", n)
	}
	// deliver now
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	mu.Lock()
	if len(payloads) != 1 || payloads[0]["kind"] != "reply" || payloads[0]["account"].(float64) != float64(a) {
		t.Fatalf("dispatches = %v", payloads)
	}
	if _, has := payloads[0]["content"]; has {
		t.Fatal("dispatch carried content")
	}
	mu.Unlock()
	// dispatch again: nothing left (at-least-once, not spam)
	if n, err := DispatchNotifyOutbox(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second dispatch: n=%d err=%v", n, err)
	}

	// delete stops delivery; the obligation itself is untouched
	if _, err := NotifyDelete(ctx, pool, a); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "again", "", "", nil, []int64{a}, nil, "n2"); err != nil {
		t.Fatalf("post2: %v", err)
	}
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	mu.Lock()
	if len(payloads) != 1 {
		t.Fatalf("deleted endpoint still received: %d", len(payloads))
	}
	mu.Unlock()
	todo2, err := TodoList(ctx, pool, a, 0, "")
	if err != nil {
		t.Fatalf("todo2: %v", err)
	}
	if n := len(todo2["todos"].([]map[string]any)); n != 2 {
		t.Fatalf("obligations changed by notify delete: %d", n)
	}
}
