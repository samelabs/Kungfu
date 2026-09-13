package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

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

	// Trusted proxy CIDRs for client IP extraction (comma-separated env var)
	// When set, X-Forwarded-For is only honored from these IPs.
	TrustedProxyCIDRs []string

	// Creem payment runtime. All-or-nothing: every key unset → disabled
	// (server still boots); any subset set → Load fails closed.
	CreemAPIKey         string
	CreemWebhookSecret  string
	CreemProductID      string
	CreemCreditsPerUnit int64
	CreemMode           string // "test" | "prod"
	CreemSuccessURL     string
}

// CreemEnabled reports whether the Creem payment runtime is fully
// configured. Never partial: Load() rejects partial configuration.
func (c *Config) CreemEnabled() bool {
	return c.CreemAPIKey != "" && c.CreemWebhookSecret != "" &&
		c.CreemProductID != "" && c.CreemCreditsPerUnit > 0 &&
		(c.CreemMode == "test" || c.CreemMode == "prod") && c.CreemSuccessURL != ""
}

// CreemAPIBase maps the configured mode to the official API host. The
// client never accepts a base URL from outside (tests inject one via the
// client constructor instead).
func (c *Config) CreemAPIBase() string {
	if c.CreemMode == "prod" {
		return "https://api.creem.io"
	}
	return "https://test-api.creem.io"
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
		DBSSLMode: envStr("DB_SSLMODE", "disable"),

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

		TrustedProxyCIDRs: parseCIDRList(envStr("TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128")),

		RateLimits: defaultRateLimits(),
	}

	if cfg.DBPass == "" {
		return nil, fmt.Errorf("DB_PASS environment variable is required")
	}
	if cfg.SessionSecret == "" {
		return nil, fmt.Errorf("SESSION_SECRET environment variable is required")
	}

	cfg.CreemAPIKey = envStr("CREEM_API_KEY", "")
	cfg.CreemWebhookSecret = envStr("CREEM_WEBHOOK_SECRET", "")
	cfg.CreemProductID = envStr("CREEM_PRODUCT_ID", "")
	cfg.CreemCreditsPerUnit = int64(envInt("CREEM_CREDITS_PER_UNIT", 0))
	cfg.CreemMode = envStr("CREEM_MODE", "")
	cfg.CreemSuccessURL = strings.TrimSpace(envStr("CREEM_SUCCESS_URL", ""))

	creemKeys := []string{
		"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PRODUCT_ID",
		"CREEM_CREDITS_PER_UNIT", "CREEM_MODE", "CREEM_SUCCESS_URL",
	}
	setCount := 0
	for _, k := range creemKeys {
		if os.Getenv(k) != "" {
			setCount++
		}
	}
	if setCount > 0 && setCount < len(creemKeys) {
		return nil, fmt.Errorf("Creem configuration is partial: set all of %s or none", strings.Join(creemKeys, ", "))
	}
	if setCount == len(creemKeys) {
		if cfg.CreemMode != "test" && cfg.CreemMode != "prod" {
			return nil, fmt.Errorf("CREEM_MODE must be test or prod")
		}
		if cfg.CreemCreditsPerUnit <= 0 {
			return nil, fmt.Errorf("CREEM_CREDITS_PER_UNIT must be a positive integer")
		}
		u, err := url.Parse(cfg.CreemSuccessURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("CREEM_SUCCESS_URL must be a valid http/https URL")
		}
	}

	return cfg, nil
}

func (c *Config) DatabaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.DBUser), url.QueryEscape(c.DBPass),
		c.DBHost, c.DBPort, c.DBName, c.DBSSLMode)
}

func defaultRateLimits() map[string]RateLimitConfig {
	t := true
	return map[string]RateLimitConfig{
		"register":    {Window: 3600, Limit: 5, Enabled: &t},
		"owner_login": {Window: 900, Limit: 20, Enabled: &t},
		"reset_key":   {Window: 86400, Limit: 50, Enabled: &t},
		"list":        {Window: 60, Limit: 120, Enabled: &t},
		"get":         {Window: 60, Limit: 300, Enabled: &t},
		"push":        {Window: 3600, Limit: 60, Enabled: &t},
		"task_submit": {Window: 60, Limit: 120, Enabled: &t},
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

func parseCIDRList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		c := strings.TrimSpace(p)
		if c != "" {
			result = append(result, c)
		}
	}
	return result
}
