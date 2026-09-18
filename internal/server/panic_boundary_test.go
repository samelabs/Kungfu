package server

// R3.1: request panic boundary proofs. The production
// recoverMiddleware wraps deterministic handlers directly — no
// test-only production routes.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func r31Middleware(s *Server, next http.Handler) http.Handler {
	return s.recoverMiddleware(next)
}

func r31Serve(mw, next http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/r31", nil))
	_ = next
	return rec
}

// A. panic before any write -> canonical 500 INTERNAL_ERROR, no leak
func TestR31PanicBeforeWriteReturnsCanonical500(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("secret-panic-token")
	}))
	rec := r31Serve(mw, nil)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "INTERNAL_ERROR") {
		t.Fatalf("body missing INTERNAL_ERROR: %s", body)
	}
	if strings.Contains(body, "secret-panic-token") {
		t.Fatal("panic value leaked to client")
	}
}

// B. panic after explicit WriteHeader + partial bytes -> committed
// response stands exactly; no second JSON appended
func TestR31PanicAfterExplicitWriteHeaderKeepsCommittedResponse(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte("partial-ok"))
		panic("boom-after-commit")
	}))
	rec := r31Serve(mw, nil)

	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202 (committed)", rec.Code)
	}
	if got := rec.Body.String(); got != "partial-ok" {
		t.Fatalf("body = %q, want exactly the pre-panic partial bytes", got)
	}
	if strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Fatal("INTERNAL_ERROR appended to a committed response")
	}
}

// C. panic after implicit commit (first Write) -> 200 + exact partial
func TestR31PanicAfterImplicitCommitKeepsCommittedResponse(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("implicit-partial"))
		panic("boom-implicit")
	}))
	rec := r31Serve(mw, nil)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (implicit commit)", rec.Code)
	}
	if got := rec.Body.String(); got != "implicit-partial" {
		t.Fatalf("body = %q, want exactly the pre-panic bytes", got)
	}
	if strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Fatal("INTERNAL_ERROR appended to an implicitly committed response")
	}
}

// D. boundary reusability: a panic request then a normal request
// through the SAME middleware instance must complete normally
func TestR31BoundaryReusableAfterPanic(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("first-request-panic")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, httptest.NewRequest("GET", "/panic", nil))
	if rec1.Code != 500 {
		t.Fatalf("first (panic) status = %d, want 500", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, httptest.NewRequest("GET", "/normal", nil))
	if rec2.Code != 200 || rec2.Body.String() != `{"ok":true}` {
		t.Fatalf("second (normal) request corrupted: %d %s", rec2.Code, rec2.Body.String())
	}
}

// E. http.ErrAbortHandler must be re-panicked as the same sentinel,
// never converted to a 500
func TestR31ErrAbortHandlerRepanicked(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()
	func() {
		defer func() {
			rec2 := recover()
			if rec2 != http.ErrAbortHandler {
				t.Fatalf("sentinel not preserved: got %v", rec2)
			}
		}()
		mw.ServeHTTP(rec, httptest.NewRequest("GET", "/abort", nil))
	}()

	if strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Fatal("ErrAbortHandler must not become INTERNAL_ERROR")
	}
}

// Normal-path regression: status/header/body unchanged through the
// boundary, including explicit non-200 statuses.
func TestR31NormalPathUnchanged(t *testing.T) {
	s := &Server{}
	mw := r31Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-R31", "kept")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("created-body"))
	}))
	rec := r31Serve(mw, nil)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("X-R31") != "kept" {
		t.Fatal("custom header lost through the wrapper")
	}
	if rec.Body.String() != "created-body" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
