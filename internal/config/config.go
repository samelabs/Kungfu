package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
)

// Config holds all application configuration.
// Mirrors PHP config/config.php structure exactly.
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

	// Content limits (must match PHP exactly)
	MaxContentSize      int
	MaxDescriptionLength int
	MaxTitleLength       int
	MaxTags              int
	MaxTagLength         int

	// Pagination
	DefaultLimit int
	MaxLimit     int
	MaxOffset    int

	// Rate limiting (must match PHP RateLimiter defaults exactly)
	RateLimits map[string]RateLimitConfig

	// HTTP server
	ListenAddr   string
	SessionSecret string

	// PostAPI HTTP client timeouts (matches CURLOPT)
	PostAPITimeout       int // seconds (CURLOPT_TIMEOUT = 10)
	PostAPIConnectTimeout int // seconds (CURLOPT_CONNECTTIMEOUT = 5)
}

type RateLimitConfig struct {
	Window  int `json:"window"`
	Limit   int `json:"limit"`
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

		APIVersion: "1.0.0",
		KeyPrefix:  "kf_live_",
		DebugMode:  envStr("DEBUG_MODE", "false") == "true",

		MaxContentSize:       102400, // 100KB
		MaxDescriptionLength: 500,
		MaxTitleLength:       128,
		MaxTags:              10,
		MaxTagLength:         32,

		DefaultLimit: 10,
		MaxLimit:     50,
		MaxOffset:    10000,

		ListenAddr:    envStr("LISTEN_ADDR", "127.0.0.1:8090"),
		SessionSecret: envStr("SESSION_SECRET", ""),

		PostAPITimeout:        10,
		PostAPIConnectTimeout: 5,

		RateLimits: defaultRateLimits(),
	}

	if cfg.DBPass == "" {
		return nil, fmt.Errorf("DB_PASS environment variable is required")
	}
	if cfg.SessionSecret == "" {
		// Generate a random one at startup if not set
		cfg.SessionSecret = generateRandomHex(32)
	}

	return cfg, nil
}

func (c *Config) DatabaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.DBUser, c.DBPass, c.DBHost, c.DBPort, c.DBName, c.DBSSLMode)
}

func defaultRateLimits() map[string]RateLimitConfig {
	// Must match PHP RateLimiter::$limits exactly
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

func generateRandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
