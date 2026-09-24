package consumption

import (
	"context"
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

// TestPolicyCoversActions: every declared Action has a policy with a valid
// price semantics — amount > 0 is illegal (a charge must be negative);
// amount == 0 is a legal FREE policy (txnType retained for future
// re-charging but must not book anything); amount < 0 is a legal charged
// policy and must carry a txnType.
func TestPolicyCoversActions(t *testing.T) {
	actions := []Action{ActionStorageCreate, ActionStorageGetPublic}
	for _, a := range actions {
		p, ok := policies[a]
		if !ok {
			t.Fatalf("action %s has no policy", a)
		}
		if p.amount > 0 {
			t.Fatalf("action %s policy amount %v is positive — illegal", a, p.amount)
		}
		if p.amount < 0 && p.txnType == "" {
			t.Fatalf("action %s is charged but missing ledger type", a)
		}
	}
	// Historical ledger types preserved for the day charging returns.
	if policies[ActionStorageCreate].txnType != "spend_push" {
		t.Fatal("storage.create must keep txnType spend_push (future re-charging)")
	}
	if policies[ActionStorageGetPublic].txnType != "spend_get" {
		t.Fatal("storage.get_public must keep txnType spend_get (future re-charging)")
	}
	// Current storage policy is FREE.
	if policies[ActionStorageCreate].amount != 0 || policies[ActionStorageGetPublic].amount != 0 {
		t.Fatal("storage actions must currently be free (amount 0)")
	}
}

// TestFreeActionNeverTouchesCredits: a free action must short-circuit
// before credits.Record — verified by pointing Apply at a nil pool: any
// credits call would panic/fail, so a nil error proves the free path
// never reached credits.
func TestFreeActionNeverTouchesCredits(t *testing.T) {
	if err := Apply(context.Background(), nil, nil, 1,
		ActionStorageCreate, "kungfu", "deadbeef0000"); err != nil {
		t.Fatalf("free storage.create must not touch credits: %v", err)
	}
	if err := Apply(context.Background(), nil, nil, 1,
		ActionStorageGetPublic, "kungfu", "deadbeef0000"); err != nil {
		t.Fatalf("free storage.get_public must not touch credits: %v", err)
	}
}
