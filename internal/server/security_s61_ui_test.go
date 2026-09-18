package server

// S6.1: UI one-time-disclosure static contract + no-plaintext-storage
// architecture guard.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func s61Read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestS61OwnerUIOneTimeDisclosureOnly locks the frontend invariants:
//   - registration displays the key from the registration RESPONSE
//     (no /api/key recovery flow)
//   - reset does NOT auto-load the current raw key; manual input
//   - copy applies only to a newly issued one-time key
//   - no localStorage/sessionStorage/URL transport for agent secrets
//   - normal /api/key rendering is masked-only
func TestS61OwnerUIOneTimeDisclosureOnly(t *testing.T) {
	auth := s61Read(t, "web/assets/owner/auth.js")
	api := s61Read(t, "web/assets/owner/api.js")
	overview := s61Read(t, "web/assets/owner/render-overview.js")

	// registration: raw key comes from the registration response and
	// stays in page state until explicit continue
	if !strings.Contains(auth, "state.newKeyOnce = json.data.key") {
		t.Fatal("registration must display the key from the registration response (one-time)")
	}
	if !strings.Contains(auth, "json.data.key") {
		t.Fatal("registration key disclosure missing")
	}
	// registration must NOT call /api/key to recover the new key
	if strings.Contains(auth, "loadOwnerKey()") && strings.Contains(auth, "activateSession();\n            setNotice") {
		t.Fatal("registration flow still uses a /api/key recovery path")
	}

	// reset: manual current-key input; no auto-load of a stored key
	if !strings.Contains(auth, "current_key: currentKey") {
		t.Fatal("reset must send the manually entered current key")
	}
	if strings.Contains(auth, "current_key: state.ownerKey") {
		t.Fatal("reset auto-loads the stored key")
	}
	if strings.Contains(auth, "loadOwnerKey()") && strings.Contains(auth, "bindResetKey") {
		t.Fatal("reset flow still fetches key material")
	}

	// copy applies only to the NEW one-time key
	if !strings.Contains(auth, "navigator.clipboard.writeText(state.newKeyOnce)") {
		t.Fatal("copy action must target only the newly issued key")
	}
	if strings.Contains(auth, "writeText(state.ownerKey)") {
		t.Fatal("copy-current-key action still exists")
	}

	// masked-only /api/key rendering
	if !strings.Contains(api, "json.data.key_masked") {
		t.Fatal("/api/key loader must consume masked metadata only")
	}
	if strings.Contains(api, "json.data.key") && !strings.Contains(api, "json.data.key_masked") {
		t.Fatal("/api/key loader reads a raw key field")
	}
	if !strings.Contains(overview, "state.keyMasked") {
		t.Fatal("rendering must use masked metadata")
	}
	if strings.Contains(overview, "state.ownerKey") {
		t.Fatal("rendering still displays a stored raw key")
	}

	// no storage/URL transport for agent secrets anywhere in owner JS
	for _, rel := range []string{
		"web/assets/owner/auth.js", "web/assets/owner/api.js",
		"web/assets/owner/core.js", "web/assets/owner/render-overview.js",
	} {
		src := s61Read(t, rel)
		if strings.Contains(src, "localStorage") || strings.Contains(src, "sessionStorage") {
			t.Fatalf("%s uses web storage for agent material", rel)
		}
		if strings.Contains(src, "newKeyOnce=") || strings.Contains(src, "key="+regKeyMarker) {
			t.Fatalf("%s transports the key via URL", rel)
		}
	}
}

const regKeyMarker = "URLPARAM"

// TestS61NoRuntimePlaintextAgentKeyStoragePath: architecture guard —
// no production runtime read/write/lookup path against a plaintext
// tb_bots.api_key. The ONLY allowed plaintext-column reference is the
// one-time backfill/drop inside migrations/008_agent_key_hash.sql
// (plus the frozen historical 001 definition, which is immutable).
func TestS61NoRuntimePlaintextAgentKeyStoragePath(t *testing.T) {
	violations := []string{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel("..", path)
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "web" || name == "docs" {
				if name == "web" || name == "docs" {
					// web assets have their own contract; docs may
					// describe history — neither owns runtime SQL
				}
				if strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		src := string(b)
		// code lines only (strip //-comments; raw SQL strings are code)
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// plaintext agent-key column in SQL context, inside Go:
			if strings.Contains(line, "api_key =") || strings.Contains(line, "api_key,") || strings.Contains(line, "SET api_key") {
				if strings.Contains(line, "api_key_hash") || strings.Contains(line, "api_key_last4") {
					continue
				}
				violations = append(violations, rel+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("plaintext agent-key runtime SQL paths:\n%s", strings.Join(violations, "\n"))
	}

	// migrations: only 001 (frozen historical definition) and 008
	// (one-time backfill/drop) may reference the plaintext column.
	entries, _ := os.ReadDir("../migrations") // this file lives in internal/server → ../.. is repo root; ../migrations wrong
	_ = entries
	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if !strings.HasSuffix(m.Name(), ".sql") {
			continue
		}
		if m.Name() == "001_schema.sql" || m.Name() == "008_agent_key_hash.sql" {
			continue
		}
		b, _ := os.ReadFile(filepath.Join("..", "..", "migrations", m.Name()))
		if strings.Contains(string(b), "api_key") {
			t.Fatalf("%s references the agent api_key column", m.Name())
		}
	}
}
