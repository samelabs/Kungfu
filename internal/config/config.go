package config

import (
	"encoding/json"
	"fmt"
	"math"
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

	// Creem fixed-package payment runtime. All-or-nothing: every key
	// unset → disabled (server still boots); any subset → Load fails
	// closed. Credits authority is this server-side package config; fiat
	// price authority is the live Creem product.
	CreemAPIKey        string
	CreemWebhookSecret string
	CreemMode          string // "test" | "prod"
	CreemSuccessURL    string
	CreemPackages      map[string]CreemPackage
}

// CreemPackage is one fixed Kungfu credits package backed by exactly one
// Creem onetime product.
type CreemPackage struct {
	Code      string  `json:"code"`
	ProductID string  `json:"product_id"`
	Credits   float64 `json:"credits"`
}

// CreemEnabled reports whether the Creem payment runtime is fully
// configured. Never partial: Load() rejects partial configuration.
func (c *Config) CreemEnabled() bool {
	return c.CreemAPIKey != "" && c.CreemWebhookSecret != "" &&
		(c.CreemMode == "test" || c.CreemMode == "prod") &&
		c.CreemSuccessURL != "" && len(c.CreemPackages) > 0
}

// CreemAPIBase maps the configured mode to the official API host. The
// client never accepts a base URL from outside (tests inject one via the
// server's internal override).
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

	if err := loadCreemConfig(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadCreemConfig applies the all-or-none fixed-package Creem rules.
func loadCreemConfig(cfg *Config) error {
	// Legacy units-mechanism env vars are rejected loudly — never
	// silently ignored.
	for _, legacy := range []string{"CREEM_PRODUCT_ID", "CREEM_CREDITS_PER_UNIT"} {
		if strings.TrimSpace(os.Getenv(legacy)) != "" {
			return fmt.Errorf("%s is set but the legacy units configuration is no longer supported; migrate to CREEM_PACKAGES_JSON", legacy)
		}
	}

	creemKeys := []string{"CREEM_API_KEY", "CREEM_WEBHOOK_SECRET", "CREEM_PACKAGES_JSON", "CREEM_MODE", "CREEM_SUCCESS_URL"}
	setCount := 0
	for _, k := range creemKeys {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			setCount++
		}
	}
	if setCount == 0 {
		return nil // disabled; server boots normally
	}
	if setCount < len(creemKeys) {
		return fmt.Errorf("Creem configuration is partial: set all of %s or none", strings.Join(creemKeys, ", "))
	}

	cfg.CreemAPIKey = envStr("CREEM_API_KEY", "")
	cfg.CreemWebhookSecret = envStr("CREEM_WEBHOOK_SECRET", "")
	cfg.CreemMode = envStr("CREEM_MODE", "")
	cfg.CreemSuccessURL = strings.TrimSpace(envStr("CREEM_SUCCESS_URL", ""))

	if cfg.CreemMode != "test" && cfg.CreemMode != "prod" {
		return fmt.Errorf("CREEM_MODE must be test or prod")
	}
	u, err := url.Parse(cfg.CreemSuccessURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("CREEM_SUCCESS_URL must be a valid http/https URL")
	}

	// Fixed packages: valid JSON array, >=1 package, unique codes AND
	// unique product ids (one fiat product must never map to two credit
	// entitlements), finite positive credits.
	rawPkgs := strings.TrimSpace(envStr("CREEM_PACKAGES_JSON", ""))
	var packages []CreemPackage
	if err := json.Unmarshal([]byte(rawPkgs), &packages); err != nil {
		return fmt.Errorf("CREEM_PACKAGES_JSON must be a valid JSON array: %w", err)
	}
	if len(packages) == 0 {
		return fmt.Errorf("CREEM_PACKAGES_JSON must define at least one package")
	}
	cfg.CreemPackages = make(map[string]CreemPackage, len(packages))
	seenProducts := map[string]string{}
	for _, pkg := range packages {
		if pkg.Code == "" {
			return fmt.Errorf("CREEM_PACKAGES_JSON: package code must not be empty")
		}
		if pkg.ProductID == "" {
			return fmt.Errorf("CREEM_PACKAGES_JSON: package %q product_id must not be empty", pkg.Code)
		}
		if math.IsNaN(pkg.Credits) || math.IsInf(pkg.Credits, 0) || pkg.Credits <= 0 {
			return fmt.Errorf("CREEM_PACKAGES_JSON: package %q credits must be a finite positive number", pkg.Code)
		}
		if _, dup := cfg.CreemPackages[pkg.Code]; dup {
			return fmt.Errorf("CREEM_PACKAGES_JSON: duplicate package code %q", pkg.Code)
		}
		if other, dup := seenProducts[pkg.ProductID]; dup {
			return fmt.Errorf("CREEM_PACKAGES_JSON: product %s is mapped by both %q and %q — one fiat product cannot carry two credit entitlements", pkg.ProductID, other, pkg.Code)
		}
		seenProducts[pkg.ProductID] = pkg.Code
		cfg.CreemPackages[pkg.Code] = pkg
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
		"register":    {Window: 3600, Limit: 5, Enabled: &t},
		"owner_login": {Window: 900, Limit: 20, Enabled: &t},
		"admin_login": {Window: 900, Limit: 10, Enabled: &t},
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
