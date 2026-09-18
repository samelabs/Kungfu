package config

import (
	"strings"
	"testing"
)

// s66Base sets everything except DB_SSLMODE.
func s66Base(t *testing.T) {
	t.Helper()
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
}

func TestS66MissingDBSSLModeFailsClosed(t *testing.T) {
	s66Base(t)
	t.Setenv("DB_SSLMODE", "")
	_, err := Load()
	if err == nil {
		t.Fatal("missing DB_SSLMODE must fail Load")
	}
	if !strings.Contains(err.Error(), "DB_SSLMODE") {
		t.Fatalf("error does not identify DB_SSLMODE: %s", err.Error())
	}
}

func TestS66RejectsDowngradeDBSSLModes(t *testing.T) {
	for _, mode := range []string{"allow", "prefer"} {
		s66Base(t)
		t.Setenv("DB_SSLMODE", mode)
		_, err := Load()
		if err == nil {
			t.Fatalf("DB_SSLMODE=%s must fail closed (plaintext-downgrade path)", mode)
		}
		if !strings.Contains(err.Error(), "DB_SSLMODE") {
			t.Fatalf("error does not identify DB_SSLMODE: %s", err.Error())
		}
	}
}

func TestS66RejectsUnknownDBSSLMode(t *testing.T) {
	s66Base(t)
	t.Setenv("DB_SSLMODE", "requiressl-please")
	_, err := Load()
	if err == nil {
		t.Fatal("unknown DB_SSLMODE must fail")
	}
	if !strings.Contains(err.Error(), "DB_SSLMODE") || !strings.Contains(err.Error(), "requiressl-please") {
		t.Fatalf("error must identify DB_SSLMODE and the rejected value: %s", err.Error())
	}
}

func TestS66ExplicitDisableRemainsSupported(t *testing.T) {
	s66Base(t)
	t.Setenv("DB_SSLMODE", "disable")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("explicit disable must load: %v", err)
	}
	if cfg.DBSSLMode != "disable" {
		t.Fatalf("DBSSLMode = %q", cfg.DBSSLMode)
	}
}

func TestS66EncryptedModesAcceptedExactly(t *testing.T) {
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		s66Base(t)
		t.Setenv("DB_SSLMODE", mode)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("DB_SSLMODE=%s must load: %v", mode, err)
		}
		if cfg.DBSSLMode != mode {
			t.Fatalf("DBSSLMode = %q, want exact %q", cfg.DBSSLMode, mode)
		}
	}
}

func TestS66DatabaseURLCarriesValidatedSSLMode(t *testing.T) {
	for _, mode := range []string{"disable", "require", "verify-ca", "verify-full"} {
		s66Base(t)
		t.Setenv("DB_SSLMODE", mode)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("load %s: %v", mode, err)
		}
		url := cfg.DatabaseURL()
		want := "sslmode=" + mode
		if !strings.Contains(url, want) {
			t.Fatalf("DatabaseURL %q missing %q", url, want)
		}
	}
}

func TestS66MalformedModeDoesNotBecomeDSNQuery(t *testing.T) {
	s66Base(t)
	t.Setenv("DB_SSLMODE", "disable&sslrootcert=/etc/evil")
	cfg, err := Load()
	if err == nil {
		t.Fatal("query-injection-shaped DB_SSLMODE must fail Load")
	}
	if cfg != nil {
		t.Fatal("config must not be returned for a rejected mode")
	}
}
