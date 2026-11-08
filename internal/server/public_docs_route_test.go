package server

// WO-9a: the publisher guide ships as a public static document at
// /bounty-guide.md (3.2.0 rename; /task-guide.md answers 301), and the
// skill answers at /skill.md (/kungfu.md answers 301).

import (
	"net/http"
	"strings"
	"testing"
)

func TestBountyGuideRoute(t *testing.T) {
	rec := s65Get(t, "/bounty-guide.md")
	if rec.Code != 200 {
		t.Fatalf("GET /bounty-guide.md = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/markdown") && !strings.Contains(ct, "text/plain") {
		t.Fatalf("/bounty-guide.md content-type = %q, want text/markdown or text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "Publisher Guide") {
		t.Fatal("/bounty-guide.md serves the publisher guide body")
	}
}

func TestSkillRouteAndLegacyRedirects(t *testing.T) {
	rec := s65Get(t, "/skill.md")
	if rec.Code != 200 {
		t.Fatalf("GET /skill.md = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Agent Skill") {
		t.Fatal("/skill.md serves the skill body")
	}
	for _, legacy := range []string{"/kungfu.md", "/task-guide.md"} {
		rec := s65Get(t, legacy)
		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("GET %s = %d, want 301", legacy, rec.Code)
		}
	}
}

func TestLlmsTxtRemainsReachable(t *testing.T) {
	rec := s65Get(t, "/llms.txt")
	if rec.Code != 200 {
		t.Fatalf("GET /llms.txt = %d, want 200", rec.Code)
	}
}
