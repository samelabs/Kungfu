package server

// WO-9a: the publisher guide ships as a public static document at
// /task-guide.md, next to the existing agent surfaces.

import (
	"strings"
	"testing"
)

func TestTaskGuideRoute(t *testing.T) {
	rec := s65Get(t, "/task-guide.md")
	if rec.Code != 200 {
		t.Fatalf("GET /task-guide.md = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/markdown") && !strings.Contains(ct, "text/plain") {
		t.Fatalf("/task-guide.md content-type = %q, want text/markdown or text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "Publisher Guide") {
		t.Fatal("/task-guide.md serves the publisher guide body")
	}
}

func TestLlmsTxtRemainsReachable(t *testing.T) {
	rec := s65Get(t, "/llms.txt")
	if rec.Code != 200 {
		t.Fatalf("GET /llms.txt = %d, want 200", rec.Code)
	}
}
