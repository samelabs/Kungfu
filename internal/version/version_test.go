package version

import "testing"

func TestCommitInjectedValueWins(t *testing.T) {
	old := commit
	t.Cleanup(func() { commit = old })
	commit = "abc123def456"
	if got := Commit(); got != "abc123def456" {
		t.Fatalf("Commit() = %q", got)
	}
	commit = ""
	if Commit() == "" {
		t.Fatal("Commit() must never be empty")
	}
}
