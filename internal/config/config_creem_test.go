package config

import "testing"

// Creem configuration gating: all unset → disabled but bootable; partial
// → fail closed; full → enabled with validated values.

func setAllCreem(t *testing.T) {
	t.Helper()
	t.Setenv("CREEM_API_KEY", "sk-test")
	t.Setenv("CREEM_WEBHOOK_SECRET", "whsec-test")
	t.Setenv("CREEM_PRODUCT_ID", "prod_test")
	t.Setenv("CREEM_CREDITS_PER_UNIT", "100")
	t.Setenv("CREEM_MODE", "test")
	t.Setenv("CREEM_SUCCESS_URL", "https://kungfu.md/owner?payment=success")
}

func TestCreemUnsetDisabledButBoots(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	for _, k := range []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT", "CREEM_MODE", "CREEM_SUCCESS_URL"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unset Creem must boot: %v", err)
	}
	if cfg.CreemEnabled() {
		t.Fatal("Creem must be disabled when unset")
	}
}

func TestCreemPartialConfigFailsClosed(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	t.Setenv("CREEM_API_KEY", "sk-test")
	t.Setenv("CREEM_WEBHOOK_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("partial Creem config must fail closed")
	}
}

func TestCreemFullConfigValid(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	setAllCreem(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("full valid Creem config: %v", err)
	}
	if !cfg.CreemEnabled() {
		t.Fatal("full config must be enabled")
	}
	if cfg.CreemAPIBase() != "https://test-api.creem.io" {
		t.Fatalf("test base = %s", cfg.CreemAPIBase())
	}
}

func TestCreemBadModeRejected(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	setAllCreem(t)
	t.Setenv("CREEM_MODE", "sandbox")
	if _, err := Load(); err == nil {
		t.Fatal("bad CREEM_MODE must fail closed")
	}
}

func TestCreemBadUnitsRejected(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	setAllCreem(t)
	t.Setenv("CREEM_CREDITS_PER_UNIT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("CREEM_CREDITS_PER_UNIT=0 must fail closed")
	}
}

func TestCreemBadSuccessURLRejected(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	setAllCreem(t)
	t.Setenv("CREEM_SUCCESS_URL", "ftp://kungfu.md/x")
	if _, err := Load(); err == nil {
		t.Fatal("non-http CREEM_SUCCESS_URL must fail closed")
	}
}

func TestCreemProdModeBase(t *testing.T) {
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
	setAllCreem(t)
	t.Setenv("CREEM_MODE", "prod")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CreemAPIBase() != "https://api.creem.io" {
		t.Fatalf("prod base = %s", cfg.CreemAPIBase())
	}
}
