package consumption

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsumptionBoundary: consumption may import credits (it is the only
// credits caller on the capability path) but must not import consumer
// domains, must not write balance/ledger SQL, and credits must not import
// consumption (reverse direction).
func TestConsumptionBoundary(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	bannedImports := []string{
		"kungfu.md/internal/service",
		"kungfu.md/internal/server",
		"kungfu.md/internal/repository",
		"kungfu.md/internal/payment",
		"kungfu.md/internal/store",
		"kungfu.md/internal/delivery",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(data)
		for _, imp := range bannedImports {
			if strings.Contains(src, "\""+imp+"\"") {
				t.Fatalf("%s imports %s — consumption must not know consumers", f, imp)
			}
		}
		for _, b := range []string{"UPDATE tb_bots", "INSERT INTO tb_transactions"} {
			if strings.Contains(src, b) {
				t.Fatalf("%s contains %s — consumption must go through credits", f, b)
			}
		}
	}

	// Reverse direction: credits must not import consumption.
	creditsFiles, _ := filepath.Glob(filepath.Join("..", "credits", "*.go"))
	for _, f := range creditsFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "\"kungfu.md/internal/consumption\"") {
			t.Fatalf("%s imports consumption — credits must not depend on consumption", f)
		}
	}
}

// TestPolicyCoversActions: every declared Action has a policy — adding an
// action without pricing fails here rather than at runtime.
func TestPolicyCoversActions(t *testing.T) {
	actions := []Action{ActionStorageCreate, ActionStorageGetPublic}
	for _, a := range actions {
		p, ok := policies[a]
		if !ok {
			t.Fatalf("action %s has no policy", a)
		}
		if p.amount >= 0 {
			t.Fatalf("action %s policy amount %v must be negative (a charge)", a, p.amount)
		}
		if p.txnType == "" {
			t.Fatalf("action %s policy missing ledger type", a)
		}
	}
	// Historical ledger types preserved (statement continuity).
	if policies[ActionStorageCreate].txnType != "spend_push" {
		t.Fatal("storage.create must book spend_push (historical continuity)")
	}
	if policies[ActionStorageGetPublic].txnType != "spend_get" {
		t.Fatal("storage.get_public must book spend_get (historical continuity)")
	}
}
