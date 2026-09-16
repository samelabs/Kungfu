package credits

// Architecture guard: RecordAuthoritativeReversal production callers are
// restricted to internal/payment. Test files are exempt. This is the
// capability boundary of the negative-balance mechanism.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthoritativeReversalCallerBoundary(t *testing.T) {
	root := "../../internal"
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil // tests exempt
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "RecordAuthoritativeReversal(") {
			rel, _ := filepath.Rel(root, path)
			pkgDir := filepath.Dir(rel)
			if pkgDir != "payment" && pkgDir != "credits" {
				t.Fatalf("RecordAuthoritativeReversal called outside internal/payment (or defined outside credits): %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
