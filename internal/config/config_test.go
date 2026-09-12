package config

import "testing"

func TestLoadRequiresSessionSecret(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("Load should fail without SESSION_SECRET")
	}
}

func TestLoadAcceptsExplicitSessionSecret(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "my-secret-value")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionSecret != "my-secret-value" {
		t.Fatalf("SessionSecret = %q, want the explicit value verbatim", cfg.SessionSecret)
	}
}
