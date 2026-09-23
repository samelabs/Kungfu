package security

import (
	"strings"
	"testing"
)

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	key, err := ParseSecretBoxKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretBoxRoundTripAndPurposeBinding(t *testing.T) {
	b := testBox(t)
	blob, err := b.Seal("sk_live_123", "creem.api_key")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "sk_live_123") {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := b.Open(blob, "creem.api_key")
	if err != nil || got != "sk_live_123" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := b.Open(blob, "creem.webhook_secret"); err == nil {
		t.Fatal("blob opened under a different purpose")
	}
	blob[len(blob)-1] ^= 1
	if _, err := b.Open(blob, "creem.api_key"); err == nil {
		t.Fatal("tampered blob opened")
	}
}

func TestSecretBoxWrongKeyAndNilBox(t *testing.T) {
	blob, _ := testBox(t).Seal("x", "p")
	other, _ := NewSecretBox([]byte(strings.Repeat("k", 32)))
	if _, err := other.Open(blob, "p"); err == nil {
		t.Fatal("opened with the wrong key")
	}
	var nilBox *SecretBox
	if _, err := nilBox.Seal("x", "p"); err != ErrSecretBoxUnavailable {
		t.Fatalf("nil box seal err = %v", err)
	}
	if _, err := nilBox.Open(blob, "p"); err != ErrSecretBoxUnavailable {
		t.Fatalf("nil box open err = %v", err)
	}
}

func TestParseSecretBoxKey(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("zz", 32), strings.Repeat("ab", 16)} {
		if _, err := ParseSecretBoxKey(bad); err == nil {
			t.Fatalf("accepted bad key %q", bad)
		}
	}
}

func TestMaskSecret(t *testing.T) {
	if MaskSecret("") != "" || MaskSecret("short") != "••••" || MaskSecret("sk_test_abcdef1234") != "••••1234" {
		t.Fatal("mask output unexpected")
	}
}
