package security

// Credential-shape detection (WO-19 P2): one hit case and one
// near-miss per pattern. The near-misses are the shapes a loose
// pattern would wrongly reject — test keys, short look-alikes,
// ordinary words containing the prefix.

import (
	"strings"
	"testing"
)

func TestContainsCredentialPatterns(t *testing.T) {
	cases := []struct {
		name    string
		hit     string
		near    string
		hitName string // what the hit is, for failure messages only
	}{
		{
			name:    "kungfu agent key",
			hit:     "kf_live_" + strings.Repeat("ab", 32),
			near:    "kf_live_" + strings.Repeat("ab", 31),
			hitName: "kf_live_ + 64 hex",
		},
		{
			name:    "aws access key",
			hit:     "AKIAIOSFODNN7EXAMPLE",
			near:    "AKIAIOSFODNN7EXAMPL",
			hitName: "AKIA + 16 upper alnum",
		},
		{
			name:    "aws temporary access key",
			hit:     "ASIAIOSFODNN7EXAMPLE",
			near:    "akiaiosfodnn7example",
			hitName: "ASIA + 16 upper alnum (lowercase must not hit)",
		},
		{
			name:    "pem rsa private key",
			hit:     "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA",
			near:    "-----BEGIN PUBLIC KEY-----",
			hitName: "RSA private key header",
		},
		{
			name:    "pem ec private key",
			hit:     "-----BEGIN EC PRIVATE KEY-----",
			near:    "-----BEGIN EC PUBLIC KEY-----",
			hitName: "EC private key header",
		},
		{
			name:    "pem openssh private key",
			hit:     "-----BEGIN OPENSSH PRIVATE KEY-----",
			near:    "-----END OPENSSH PRIVATE KEY-----",
			hitName: "OpenSSH private key header (END must not hit)",
		},
		{
			name:    "pem bare private key",
			hit:     "-----BEGIN PRIVATE KEY-----",
			near:    "-----BEGIN ENCRYPTED PRIVATE KEY-----",
			hitName: "bare private key header (ENCRYPTED is not in the prefix set)",
		},
		{
			name:    "github token",
			hit:     "ghp_" + strings.Repeat("a1B2c3D4e5", 4), // 40 chars
			near:    "ghp_" + strings.Repeat("a1B2c3D4", 2),   // 16 chars
			hitName: "ghp_ + 36+ alnum (short suffix must not hit)",
		},
		{
			name:    "github token other prefixes",
			hit:     "gho_" + strings.Repeat("Z9y8X7w6V5", 4),
			near:    "ghx_" + strings.Repeat("Z9y8X7w6V5", 4),
			hitName: "gho_ token (ghx_ prefix must not hit)",
		},
		{
			name:    "slack token",
			hit:     "xoxb-123456789012-1234567890123456-abcdefabcdef",
			near:    "xoxb-short",
			hitName: "xoxb- + 10+ token chars",
		},
		{
			name:    "slack other prefixes",
			hit:     "xoxp-1234567890-2345678901-3456789012abcdef",
			near:    "xoxx-1234567890-2345678901-3456789012abcdef",
			hitName: "xoxp- token (x in the fourth slot must not hit)",
		},
		{
			name:    "openai style key",
			hit:     "sk-" + strings.Repeat("9f8e7d6c5b", 5), // 50 chars
			near:    "sk-" + strings.Repeat("9f8e7d6c", 2),   // 16 chars
			hitName: "sk- + 32+ alnum (ordinary short words must not hit)",
		},
		{
			name:    "anthropic key",
			hit:     "sk-ant-api03-" + strings.Repeat("Qq1Ww2Ee3R", 5),
			near:    "risk-ant-body",
			hitName: "sk-ant- prefix (words merely containing sk-ant- must not hit)",
		},
		{
			name:    "stripe live secret key",
			hit:     "sk_live_" + strings.Repeat("Kj3Nn4Mm5", 4),
			near:    "sk_test_" + strings.Repeat("Kj3Nn4Mm5", 4),
			hitName: "sk_live_ key (test keys must not hit)",
		},
		{
			name:    "stripe live restricted key",
			hit:     "rk_live_" + strings.Repeat("Pp6Oo7Ii8", 4),
			near:    "rk_live_" + strings.Repeat("Pp6Oo7", 1),
			hitName: "rk_live_ key (short suffix must not hit)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !ContainsCredential(tc.hit) {
				t.Fatalf("hit case %q (%s) not detected", tc.hit, tc.hitName)
			}
			if ContainsCredential(tc.near) {
				t.Fatalf("near-miss %q wrongly detected as a credential", tc.near)
			}
		})
	}
}

func TestContainsCredentialPlainProse(t *testing.T) {
	// Ordinary text that merely contains prefixes as words or short
	// fragments — none of this may be rejected.
	for _, s := range []string{
		"The task asks to summarize a page in three bullets.",
		"Return {\"result\": \"ok\"} with a status code and a message.",
		"sk-opinion piece about sk-ant-ique furniture", // both under the length floors
		"AKIA is a prefix used by AWS for access key IDs.",
		"ghp_ sounds like a GitHub token prefix.",
		"use the https://kungfu.md/llms.txt index",
	} {
		if ContainsCredential(s) {
			t.Fatalf("ordinary text wrongly detected: %q", s)
		}
	}
}

func TestContainsCredentialValueWalksStructures(t *testing.T) {
	// nested map/slice walk finds the credential at any depth
	v := map[string]interface{}{
		"url":  "https://example.com/a",
		"meta": []interface{}{"one", map[string]interface{}{"token": "ghp_" + strings.Repeat("a1B2c3D4e5", 4)}},
	}
	if !ContainsCredentialValue(v) {
		t.Fatal("nested credential not found")
	}
	if err := RejectCredentialInContent(v, "content"); err == nil {
		t.Fatal("RejectCredentialInContent must reject the nested credential")
	}
	clean := map[string]interface{}{"a": []interface{}{"b", 1, nil, true}}
	if ContainsCredentialValue(clean) {
		t.Fatal("clean value wrongly flagged")
	}
	if err := RejectCredentialInContent(clean, "content"); err != nil {
		t.Fatalf("clean value rejected: %v", err)
	}
}
