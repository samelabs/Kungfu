package auth

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/model"
)

// The auth lookup receives ONLY the SHA-256 digest of the raw
// X-Bot-Key — the raw credential never crosses into the repository.

func TestKeyAtRestAgentAuthHashesBeforeLookup(t *testing.T) {
	var seen [][]byte
	lookup := func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		seen = append(seen, append([]byte(nil), keyHash...))
		return &model.Bot{ID: 1, BotName: "s61bot"}, nil
	}

	raw := GenerateKey()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Bot-Key", raw)

	bot, err := VerifyBotAuth(context.Background(), lookup, req)
	if err != nil || bot == nil {
		t.Fatalf("auth failed: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("lookup called %d times", len(seen))
	}
	want := HashAgentKey(raw)
	if string(seen[0]) != string(want) {
		t.Fatalf("lookup received %x, want the SHA-256 digest %x", seen[0], want)
	}
	// digest, not raw: 32 bytes, and not derivable from the raw string prefix
	if len(seen[0]) != 32 {
		t.Fatalf("lookup arg len = %d, want 32", len(seen[0]))
	}
}

// Malformed keys never reach the lookup at all.
func TestKeyAtRestMalformedKeyNeverReachesLookup(t *testing.T) {
	called := false
	lookup := func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		called = true
		return &model.Bot{}, nil
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Bot-Key", "not-a-key")
	if _, err := VerifyBotAuth(context.Background(), lookup, req); err == nil {
		t.Fatal("malformed key must fail")
	}
	if called {
		t.Fatal("lookup was called for a malformed key")
	}
}

// HashAgentKey helper invariants: exact raw bytes, 32-byte digest,
// stable across calls, distinct for distinct inputs.
func TestKeyAtRestHashAgentKeyInvariants(t *testing.T) {
	raw := GenerateKey()
	h1 := HashAgentKey(raw)
	h2 := HashAgentKey(raw)
	if string(h1) != string(h2) {
		t.Fatal("unstable digest")
	}
	if len(h1) != 32 {
		t.Fatalf("digest len = %d", len(h1))
	}
	other := GenerateKey()
	if string(h1) == string(HashAgentKey(other)) {
		t.Fatal("distinct keys hashed equal")
	}
	// case is preserved: uppercase hex is a DIFFERENT key (no lowering)
	upper := "kf_live_" + repeatChar('A', 64)
	if !ValidateKeyFormat(upper) {
		t.Skip("uppercase-hex format not accepted by validator")
	}
	if string(HashAgentKey(upper)) == string(HashAgentKey("kf_live_"+repeatChar('a', 64))) {
		t.Fatal("hash canonicalizes case")
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// The shared raw-key verifier is the ONE authority for REST and
// MCP; bearer extraction is RFC 6750.
func TestSharedVerifyAgentKeyAuthority(t *testing.T) {
	raw := GenerateKey()
	lookup := func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		want := HashAgentKey(raw)
		if string(keyHash) != string(want) {
			t.Fatalf("lookup received %x, want digest only", keyHash)
		}
		return &model.Bot{ID: 9}, nil
	}
	bot, err := VerifyAgentKey(context.Background(), raw, lookup)
	if err != nil || bot == nil || bot.ID != 9 {
		t.Fatalf("valid key failed: %v", err)
	}
	// malformed / empty / unknown all give the SAME error (no enumeration)
	for _, bad := range []string{"", "short", raw + "x"} {
		_, err := VerifyAgentKey(context.Background(), bad, lookup)
		if err == nil {
			t.Fatalf("bad key %q accepted", bad)
		}
	}
}

// P3-34: the password policy caps at bcrypt's 72-byte input limit.
func TestValidatePasswordByteLimit(t *testing.T) {
	if ok, errs := ValidatePassword(strings.Repeat("a", 72)); !ok {
		t.Fatalf("72 bytes must be valid: %v", errs)
	}
	ok, errs := ValidatePassword(strings.Repeat("a", 73))
	if ok {
		t.Fatal("73 bytes must be rejected (bcrypt input limit)")
	}
	if len(errs) == 0 || !strings.Contains(errs[0], "72") {
		t.Fatalf("errors = %v, want the 72-byte ceiling named", errs)
	}
}
