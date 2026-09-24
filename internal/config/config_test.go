package config

import (
	"strings"
	"testing"
)

// s62 is exactly 32 bytes of secret-shaped material.
const s62secret32 = "0123456789abcdef0123456789abcdef"

func TestLoadRequiresSessionSecret(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("SESSION_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("Load should fail without SESSION_SECRET")
	}
}

func TestLoadRejectsShortSessionSecret(t *testing.T) {
	// exactly 31 bytes — one below the gate
	short := strings.Repeat("s", 31)
	t.Setenv("DB_PASS", "pw")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("SESSION_SECRET", short)
	_, err := Load()
	if err == nil {
		t.Fatal("31-byte SESSION_SECRET must fail startup")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SESSION_SECRET") {
		t.Fatalf("error does not identify SESSION_SECRET: %s", msg)
	}
	if !strings.Contains(msg, "32") {
		t.Fatalf("error does not state the 32-byte minimum: %s", msg)
	}
	if strings.Contains(msg, short) {
		t.Fatal("error echoes the configured secret")
	}
}

func TestLoadAccepts32ByteSessionSecretVerbatim(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("SESSION_SECRET", s62secret32)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("32-byte SESSION_SECRET must load: %v", err)
	}
	if cfg.SessionSecret != s62secret32 {
		t.Fatalf("SessionSecret = %q, want the exact input verbatim", cfg.SessionSecret)
	}
	if len(cfg.SessionSecret) != 32 {
		t.Fatalf("SessionSecret byte length = %d, want 32", len(cfg.SessionSecret))
	}
}

func TestLoadAcceptsLongSessionSecretVerbatim(t *testing.T) {
	// 64-char openssl rand -hex 32 shaped value
	long := strings.Repeat("a1b2c3d4e5f6", 5) + "abcdef" // 66 chars
	t.Setenv("DB_PASS", "pw")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("SESSION_SECRET", long)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("long SESSION_SECRET must load: %v", err)
	}
	if cfg.SessionSecret != long {
		t.Fatalf("SessionSecret = %q, want the exact input verbatim", cfg.SessionSecret)
	}
}
