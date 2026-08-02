package ratelimit

import (
	"strconv"
	"sync"
	"time"
)

// Result holds rate limit check results.
// Matches PHP format: ['allowed' => bool, 'retry_after' => int, 'limit' => int, 'window' => int]
type Result struct {
	Allowed    bool
	RetryAfter int
	Limit      int
	Window     int
}

// Config holds the rate limit configuration for one action.
type Config struct {
	Window  int
	Limit   int
	Enabled bool
}

// Limiter is the in-memory rate limiter.
type Limiter struct {
	mu      sync.Mutex
	stores  map[string][]int64 // key -> sorted timestamps (unix seconds)
	configs map[string]Config
}

// NewLimiter creates a new rate limiter with the given configuration.
func NewLimiter(configs map[string]Config) *Limiter {
	return &Limiter{
		stores:  make(map[string][]int64),
		configs: configs,
	}
}

// CheckRegister checks IP-level registration rate limit.
// PHP: checkRegister(ip) / checkRegisterWithRetry(ip)
func (l *Limiter) CheckRegister(ip string) Result {
	return l.check("reg:"+ip, "register")
}

// CheckOwnerLogin checks IP-level owner login rate limit.
// PHP: checkOwnerLogin(ip) / checkOwnerLoginWithRetry(ip)
func (l *Limiter) CheckOwnerLogin(ip string) Result {
	return l.check("owner_login:"+ip, "owner_login")
}

// CheckAPI checks bot-level API rate limit.
// PHP: checkApi(botId, action)
func (l *Limiter) CheckAPI(botID int64, action string) bool {
	cfg, exists := l.configs[action]
	if !exists || !cfg.Enabled {
		return true // Unconfigured endpoints are not rate limited
	}
	key := apiKey(botID, action)
	result := l.check(key, action)
	return result.Allowed
}

// CheckAPIWithDetails checks bot-level API rate limit and returns full details.
// PHP: checkApiWithDetails(botId, action)
func (l *Limiter) CheckAPIWithDetails(botID int64, action string) Result {
	cfg, exists := l.configs[action]
	if !exists || !cfg.Enabled {
		return Result{Allowed: true, RetryAfter: 0, Limit: 0, Window: 0}
	}
	key := apiKey(botID, action)
	return l.inspect(key, action)
}

// GetRemaining returns the remaining requests for a bot/action.
// PHP: getRemaining(botId, action)
func (l *Limiter) GetRemaining(botID int64, action string) int {
	cfg, exists := l.configs[action]
	if !exists || !cfg.Enabled {
		return int(^uint(0) >> 1) // max int
	}
	key := apiKey(botID, action)
	now := time.Now().Unix()

	l.mu.Lock()
	defer l.mu.Unlock()

	timestamps := l.filterWindow(l.stores[key], now, cfg.Window)
	used := len(timestamps)
	l.stores[key] = timestamps

	remaining := cfg.Limit - used
	if remaining < 0 {
		return 0
	}
	return remaining
}

// GC cleans up expired entries. Can be called periodically.
// PHP: gc()
func (l *Limiter) GC() {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()

	for key, timestamps := range l.stores {
		filtered := l.filterWindow(timestamps, now, 2592000) // 30 days max
		if len(filtered) == 0 {
			delete(l.stores, key)
		} else {
			l.stores[key] = filtered
		}
	}
}

// GetStats returns statistics for debugging.
// PHP: getStats()
func (l *Limiter) GetStats() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	totalRecords := 0
	for _, timestamps := range l.stores {
		totalRecords += len(timestamps)
	}

	return map[string]interface{}{
		"keys_count":    len(l.stores),
		"total_records": totalRecords,
	}
}

// check is the core check logic.
// PHP: RateLimiter::check(key, config) - consumes a slot when allowed.
func (l *Limiter) check(key string, action string) Result {
	cfg, exists := l.configs[action]
	if !exists || !cfg.Enabled {
		return Result{Allowed: true, RetryAfter: 0, Limit: 0, Window: 0}
	}

	now := time.Now().Unix()
	window := int64(cfg.Window)
	limit := cfg.Limit

	l.mu.Lock()
	defer l.mu.Unlock()

	timestamps := l.filterWindow(l.stores[key], now, cfg.Window)

	if len(timestamps) >= limit {
		l.stores[key] = timestamps
		return formatResult(false, timestamps, window, limit, now)
	}

	timestamps = append(timestamps, now)
	l.stores[key] = timestamps

	return formatResult(true, timestamps, window, limit, now)
}

// inspect is the read-only variant used by CheckAPIWithDetails.
// PHP: RateLimiter::inspect(key, config) - does NOT consume a slot.
func (l *Limiter) inspect(key string, action string) Result {
	cfg, exists := l.configs[action]
	if !exists || !cfg.Enabled {
		return Result{Allowed: true, RetryAfter: 0, Limit: 0, Window: 0}
	}

	now := time.Now().Unix()
	window := int64(cfg.Window)
	limit := cfg.Limit

	l.mu.Lock()
	defer l.mu.Unlock()

	timestamps := l.filterWindow(l.stores[key], now, cfg.Window)
	l.stores[key] = timestamps

	allowed := len(timestamps) < limit
	return formatResult(allowed, timestamps, window, limit, now)
}

func (l *Limiter) filterWindow(timestamps []int64, now int64, window int) []int64 {
	cutoff := now - int64(window)
	result := make([]int64, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts > cutoff {
			result = append(result, ts)
		}
	}
	return result
}

func formatResult(allowed bool, timestamps []int64, window int64, limit int, now int64) Result {
	retryAfter := int64(0)
	if !allowed && len(timestamps) > 0 {
		// PHP: max(0, min(timestamps) + window - now)
		minTs := timestamps[0]
		for _, ts := range timestamps[1:] {
			if ts < minTs {
				minTs = ts
			}
		}
		retryAfter = minTs + window - now
		if retryAfter < 0 {
			retryAfter = 0
		}
	}

	return Result{
		Allowed:    allowed,
		RetryAfter: int(retryAfter),
		Limit:      limit,
		Window:     int(window),
	}
}

func apiKey(botID int64, action string) string {
	return "api:" + strconv.FormatInt(botID, 10) + ":" + action
}
