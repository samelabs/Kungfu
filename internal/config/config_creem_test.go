package config

import "testing"

// Fixed-package Creem configuration gating.

func creemBase(t *testing.T) {
	t.Helper()
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "s")
}

func setFullCreem(t *testing.T, packagesJSON string) {
	t.Helper()
	creemBase(t)
	t.Setenv("CREEM_API_KEY", "sk-test")
	t.Setenv("CREEM_WEBHOOK_SECRET", "whsec-test")
	t.Setenv("CREEM_PACKAGES_JSON", packagesJSON)
	t.Setenv("CREEM_MODE", "test")
	t.Setenv("CREEM_SUCCESS_URL", "https://kungfu.md/owner?payment=success")
}

const validPackages = `[{"code":"starter","product_id":"prod_a","credits":1000},{"code":"standard","product_id":"prod_b","credits":5000}]`

func TestCreemUnsetDisabledButBoots(t *testing.T) {
	creemBase(t)
	for _, k := range []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PACKAGES_JSON", "CREEM_MODE", "CREEM_SUCCESS_URL", "CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT"} {
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
	creemBase(t)
	t.Setenv("CREEM_API_KEY", "sk")
	if _, err := Load(); err == nil {
		t.Fatal("partial config must fail closed")
	}
}

func TestCreemLegacyUnitsEnvFails(t *testing.T) {
	setFullCreem(t, validPackages)
	t.Setenv("CREEM_PRODUCT_ID", "prod_old")
	if _, err := Load(); err == nil {
		t.Fatal("legacy CREEM_PRODUCT_ID must fail closed")
	}
	setFullCreem(t, validPackages)
	t.Setenv("CREEM_CREDITS_PER_UNIT", "100")
	if _, err := Load(); err == nil {
		t.Fatal("legacy CREEM_CREDITS_PER_UNIT must fail closed")
	}
}

func TestCreemPackagesValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"malformed json", `{not json`},
		{"not an array", `{"code":"x"}`},
		{"empty array", `[]`},
		{"empty code", `[{"code":"","product_id":"p","credits":1}]`},
		{"empty product", `[{"code":"a","product_id":"","credits":1}]`},
		{"zero credits", `[{"code":"a","product_id":"p","credits":0}]`},
		{"negative credits", `[{"code":"a","product_id":"p","credits":-5}]`},
		{"duplicate code", `[{"code":"a","product_id":"p1","credits":1},{"code":"a","product_id":"p2","credits":2}]`},
		{"duplicate product", `[{"code":"a","product_id":"p","credits":1},{"code":"b","product_id":"p","credits":2}]`},
	}
	for _, tc := range cases {
		setFullCreem(t, tc.json)
		if _, err := Load(); err == nil {
			t.Fatalf("%s: must fail closed", tc.name)
		}
	}
}

func TestCreemFullConfigParsesPackages(t *testing.T) {
	setFullCreem(t, validPackages)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if !cfg.CreemEnabled() {
		t.Fatal("must be enabled")
	}
	if len(cfg.CreemPackages) != 2 {
		t.Fatalf("packages = %d", len(cfg.CreemPackages))
	}
	if p := cfg.CreemPackages["starter"]; p.ProductID != "prod_a" || p.Credits != 1000 {
		t.Fatalf("starter = %+v", p)
	}
	if cfg.CreemAPIBase() != "https://test-api.creem.io" {
		t.Fatalf("test base = %s", cfg.CreemAPIBase())
	}
}
