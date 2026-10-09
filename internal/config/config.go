package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"kungfu.md/internal/security"
	"kungfu.md/internal/version"
)

// Config holds all application configuration.

type Config struct {
	// Database
	DBHost    string
	DBName    string
	DBUser    string
	DBPass    string
	DBPort    int
	DBSSLMode string

	// System
	APIVersion string
	KeyPrefix  string
	DebugMode  bool

	// Content limits
	MaxContentSize       int
	MaxDescriptionLength int
	MaxTitleLength       int
	MaxTags              int
	MaxTagLength         int

	MaxOffset int

	// Rate limiting
	RateLimits map[string]RateLimitConfig

	// HTTP server
	ListenAddr    string
	SessionSecret string

	// PostAPI HTTP client timeouts

	// Trusted proxy networks, parsed ONCE at Config.Load from the
	// TRUSTED_PROXY_CIDRS comma-separated env var. The direct TCP peer
	// must belong to one of these for forwarded request metadata
	// (X-Forwarded-For / X-Forwarded-Proto) to be honored. Any invalid
	// non-empty entry fails Load (no silent drop, no default fallback).
	TrustedProxyCIDRs []*net.IPNet

	// SettingsEncKey (SETTINGS_ENC_KEY, 32 bytes as 64 hex chars) seals
	// operator secrets stored in the database — the Creem API key and
	// webhook secret. Optional: when unset, payment settings cannot be
	// saved and payments stay unavailable, but the server boots.
	SettingsEncKey []byte
}

type RateLimitConfig struct {
	Window  int   `json:"window"`
	Limit   int   `json:"limit"`
	Enabled *bool `json:"enabled,omitempty"`
}

func Load() (*Config, error) {
	cfg := &Config{
		DBHost:    envStr("DB_HOST", "localhost"),
		DBName:    envStr("DB_NAME", "kungfu_md"),
		DBUser:    envStr("DB_USER", "kungfu_app"),
		DBPass:    envStr("DB_PASS", ""),
		DBPort:    envInt("DB_PORT", 5432),
		DBSSLMode: envStr("DB_SSLMODE", ""),

		APIVersion: version.Get(),
		KeyPrefix:  "kf_live_",
		DebugMode:  envStr("DEBUG_MODE", "false") == "true",

		MaxContentSize:       102400, // 100KB
		MaxDescriptionLength: 500,
		MaxTitleLength:       128,
		MaxTags:              10,
		MaxTagLength:         32,

		MaxOffset: 10000,

		ListenAddr:    envStr("LISTEN_ADDR", "127.0.0.1:8090"),
		SessionSecret: envStr("SESSION_SECRET", ""),

		RateLimits: defaultRateLimits(),
	}

	if cfg.DBPass == "" {
		return nil, fmt.Errorf("DB_PASS environment variable is required")
	}
	if cfg.SessionSecret == "" {
		return nil, fmt.Errorf("SESSION_SECRET environment variable is required")
	}

	// Parse trusted proxies ONCE, fail closed on any invalid
	// non-empty entry (a bare IP stays legal as a /32 or /128 host
	// network, preserving the historical contract).
	trustedCIDRs, err := parseTrustedProxyCIDRs(envStr("TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128"))
	if err != nil {
		return nil, err
	}
	cfg.TrustedProxyCIDRs = trustedCIDRs
	// Fail closed when SESSION_SECRET is too short to serve as
	// the HMAC key for Owner session signing and Admin CSRF
	// derivation. The check is on the exact raw env bytes — no trim,
	// no normalization, no re-encoding; the accepted value is stored
	// verbatim. Length cannot prove entropy; 32 bytes is the minimum
	// configuration gate (operators should use `openssl rand -hex 32`).
	if len(cfg.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 bytes for HMAC signing; generate one with `openssl rand -hex 32`")
	}

	// DB_SSLMODE has NO implicit default — the transport posture
	// must be an explicit, conscious choice. Only the four approved
	// modes pass; allow/prefer (plaintext-downgrade paths), empty, and
	// unknown values fail closed before any database open.
	switch cfg.DBSSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		if cfg.DBSSLMode == "" {
			return nil, fmt.Errorf("DB_SSLMODE environment variable is required (disable | require | verify-ca | verify-full)")
		}
		return nil, fmt.Errorf("DB_SSLMODE value %q is not supported (allowed: disable | require | verify-ca | verify-full)", cfg.DBSSLMode)
	}

	if err := loadSettingsConfig(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadSettingsConfig parses SETTINGS_ENC_KEY and rejects the retired
// CREEM_* environment configuration loudly: payment settings now live
// in the database (platform admin → Settings → Payments), and a stale
// env var must never be mistaken for the active configuration.
func loadSettingsConfig(cfg *Config) error {
	for _, k := range []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PACKAGES_JSON", "CREEM_MODE", "CREEM_SUCCESS_URL", "CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return fmt.Errorf("%s is set, but Creem configuration now lives in the database: configure it in the platform admin (/samelabs/settings/payment) and remove all CREEM_* environment variables", k)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("SETTINGS_ENC_KEY")); raw != "" {
		key, err := security.ParseSecretBoxKey(raw)
		if err != nil {
			return fmt.Errorf("SETTINGS_ENC_KEY %v", err)
		}
		cfg.SettingsEncKey = key
	}
	return nil
}

func (c *Config) DatabaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.DBUser), url.QueryEscape(c.DBPass),
		c.DBHost, c.DBPort, c.DBName, c.DBSSLMode)
}

func defaultRateLimits() map[string]RateLimitConfig {
	t := true
	return map[string]RateLimitConfig{
		"register":         {Window: 3600, Limit: 5, Enabled: &t},
		"owner_login":      {Window: 900, Limit: 20, Enabled: &t},
		"admin_login":      {Window: 900, Limit: 10, Enabled: &t},
		"reset_key":        {Window: 86400, Limit: 50, Enabled: &t},
		"list":             {Window: 60, Limit: 120, Enabled: &t},
		"get":              {Window: 60, Limit: 300, Enabled: &t},
		"push":             {Window: 3600, Limit: 60, Enabled: &t},
		"task_submit":      {Window: 60, Limit: 120, Enabled: &t},
		"task_create":      {Window: 3600, Limit: 20, Enabled: &t},
		"payment_checkout": {Window: 3600, Limit: 20, Enabled: &t},
		// room face (kungfu PRD §2)
		"thread_write":    {Window: 60, Limit: 120, Enabled: &t},
		"thread_start":    {Window: 3600, Limit: 30, Enabled: &t},
		"thread_read":     {Window: 60, Limit: 600, Enabled: &t},
		"notify_register": {Window: 3600, Limit: 5, Enabled: &t},
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseTrustedProxyCIDRs parses the TRUSTED_PROXY_CIDRS env value into
// typed networks. Every non-empty entry must be a valid CIDR or a
// bare literal IPv4/IPv6 address (treated as /32 or /128); anything
// else is a configuration error — invalid entries are never silently
// dropped and never fall back to defaults.
func parseTrustedProxyCIDRs(s string) ([]*net.IPNet, error) {
	if s == "" {
		return nil, nil
	}
	var result []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS entry %q is not a valid IP or CIDR", entry)
			}
			if ip.To4() != nil {
				entry += "/32"
			} else {
				entry += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS entry %q is not a valid IP or CIDR", entry)
		}
		result = append(result, cidr)
	}
	return result, nil
}
