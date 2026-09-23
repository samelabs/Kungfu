package config

import (
	"strings"
	"testing"
)

// Creem configuration lives in the database; the environment only
// carries SETTINGS_ENC_KEY.

func settingsBase(t *testing.T) {
	t.Helper()
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("DB_SSLMODE", "disable")
	for _, k := range []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PACKAGES_JSON", "CREEM_MODE", "CREEM_SUCCESS_URL", "CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT", "SETTINGS_ENC_KEY"} {
		t.Setenv(k, "")
	}
}

func TestRetiredCreemEnvFailsLoudly(t *testing.T) {
	for _, k := range []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PACKAGES_JSON", "CREEM_MODE", "CREEM_SUCCESS_URL", "CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT"} {
		settingsBase(t)
		t.Setenv(k, "x")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "/samelabs/settings/payment") {
			t.Fatalf("%s set: err = %v, want a pointer to the admin settings page", k, err)
		}
	}
}

func TestSettingsEncKeyOptionalAndValidated(t *testing.T) {
	settingsBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("no key must boot: %v", err)
	}
	if cfg.SettingsEncKey != nil {
		t.Fatal("key must be nil when unset")
	}

	settingsBase(t)
	t.Setenv("SETTINGS_ENC_KEY", strings.Repeat("0f", 32))
	cfg, err = Load()
	if err != nil || len(cfg.SettingsEncKey) != 32 {
		t.Fatalf("valid key: len=%d err=%v", len(cfg.SettingsEncKey), err)
	}

	for _, bad := range []string{"short", strings.Repeat("zz", 32), strings.Repeat("0f", 31)} {
		settingsBase(t)
		t.Setenv("SETTINGS_ENC_KEY", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("bad key %q accepted", bad)
		}
	}
}
