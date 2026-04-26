<?php
/**
 * RateLimiter - Request Rate Limiting Control (file-lock fallback)
 * Kungfu Platform Core Component
 * Production environment can migrate this to Redis without changing callers.
 */

class RateLimiter {
    // Conservative defaults. Environment config can override window/limit/enabled.
    private static $limits = [
        'register' => ['window' => 3600, 'limit' => 5, 'enabled' => true],
        'owner_login' => ['window' => 900, 'limit' => 20, 'enabled' => true],
        'reset_key' => ['window' => 86400, 'limit' => 50, 'enabled' => true],

        // Bot-scoped API limits allow normal agent parallelism while bounding runaway loops.
        'list' => ['window' => 60, 'limit' => 120, 'enabled' => true],
        'get' => ['window' => 60, 'limit' => 300, 'enabled' => true],
        'push' => ['window' => 3600, 'limit' => 60, 'enabled' => true],
        'task_submit' => ['window' => 60, 'limit' => 120, 'enabled' => true],
    ];
    
    /**
     * Check registration rate limit (IP level)
     */
    public static function checkRegister(string $ip): bool {
        $key = "reg:{$ip}";
        $config = self::limitFor('register');
        return self::check($key, $config)['allowed'];
    }
    
    /**
     * Check registration rate limit and get retry time
     */
    public static function checkRegisterWithRetry(string $ip): array {
        $key = "reg:{$ip}";
        $config = self::limitFor('register');
        return self::check($key, $config);
    }

    /**
     * Check owner login rate limit (IP level)
     */
    public static function checkOwnerLogin(string $ip): bool {
        $key = "owner_login:{$ip}";
        $config = self::limitFor('owner_login');
        return self::check($key, $config)['allowed'];
    }

    /**
     * Check owner login rate limit and get retry time
     */
    public static function checkOwnerLoginWithRetry(string $ip): array {
        $key = "owner_login:{$ip}";
        $config = self::limitFor('owner_login');
        return self::check($key, $config);
    }
    
    /**
     * Check API rate limit (Bot level)
     */
    public static function checkApi(int $botId, string $action): bool {
        if (!self::isEnabled($action)) {
            return true; // Unconfigured endpoints are not rate limited
        }
        
        $key = "api:{$botId}:{$action}";
        $config = self::limitFor($action);
        
        return self::check($key, $config)['allowed'];
    }
    
    /**
     * Check API rate limit and get detailed info
     */
    public static function checkApiWithDetails(int $botId, string $action): array {
        if (!self::isEnabled($action)) {
            return ['allowed' => true, 'retry_after' => 0, 'limit' => 0, 'window' => 0];
        }
        
        $key = "api:{$botId}:{$action}";
        $config = self::limitFor($action);
        return self::inspect($key, $config);
    }
    
    /**
     * Core check logic
     */
    private static function check(string $key, array $config): array {
        $now = time();
        $window = (int)$config['window'];
        $limit = (int)$config['limit'];

        return self::withStorageLock($key, function (array $timestamps) use ($now, $window, $limit) {
            $timestamps = self::filterWindow($timestamps, $now, $window);

            if (count($timestamps) >= $limit) {
                return [
                    'state' => $timestamps,
                    'result' => self::formatResult(false, $timestamps, $window, $limit, $now)
                ];
            }

            $timestamps[] = $now;

            return [
                'state' => $timestamps,
                'result' => self::formatResult(true, $timestamps, $window, $limit, $now)
            ];
        });
    }

    /**
     * Get wait time required
     */
    private static function inspect(string $key, array $config): array {
        $now = time();
        $window = (int)$config['window'];
        $limit = (int)$config['limit'];

        return self::withStorageLock($key, function (array $timestamps) use ($now, $window, $limit) {
            $timestamps = self::filterWindow($timestamps, $now, $window);
            $allowed = count($timestamps) < $limit;

            return [
                'state' => $timestamps,
                'result' => self::formatResult($allowed, $timestamps, $window, $limit, $now)
            ];
        });
    }

    private static function filterWindow(array $timestamps, int $now, int $window): array {
        return array_values(array_filter($timestamps, function ($timestamp) use ($now, $window) {
            return is_int($timestamp) && $timestamp > ($now - $window);
        }));
    }

    private static function formatResult(bool $allowed, array $timestamps, int $window, int $limit, int $now): array {
        $retryAfter = 0;
        if (!$allowed && !empty($timestamps)) {
            $retryAfter = max(0, min($timestamps) + $window - $now);
        }

        return [
            'allowed' => $allowed,
            'retry_after' => $retryAfter,
            'limit' => $limit,
            'window' => $window
        ];
    }

    private static function withStorageLock(string $key, callable $callback): array {
        $dir = self::storageDir();
        if (!is_dir($dir) && !mkdir($dir, 0700, true) && !is_dir($dir)) {
            error_log("[RateLimiter] Failed to create storage dir: {$dir}");
            return ['allowed' => true, 'retry_after' => 0, 'limit' => 0, 'window' => 0];
        }

        $file = $dir . '/' . hash('sha256', $key) . '.json';
        $handle = fopen($file, 'c+');
        if (!$handle) {
            error_log("[RateLimiter] Failed to open storage file: {$file}");
            return ['allowed' => true, 'retry_after' => 0, 'limit' => 0, 'window' => 0];
        }

        try {
            if (!flock($handle, LOCK_EX)) {
                return ['allowed' => true, 'retry_after' => 0, 'limit' => 0, 'window' => 0];
            }

            rewind($handle);
            $raw = stream_get_contents($handle);
            $timestamps = $raw ? json_decode($raw, true) : [];
            if (!is_array($timestamps)) {
                $timestamps = [];
            }

            $callbackResult = $callback($timestamps);
            $state = $callbackResult['state'] ?? [];
            $result = $callbackResult['result'] ?? ['allowed' => true, 'retry_after' => 0, 'limit' => 0, 'window' => 0];

            ftruncate($handle, 0);
            rewind($handle);
            fwrite($handle, json_encode(array_values($state)));
            fflush($handle);
            flock($handle, LOCK_UN);

            return $result;
        } finally {
            fclose($handle);
        }
    }
    
    /**
     * Get remaining requests in current window
     */
    public static function getRemaining(int $botId, string $action): int {
        if (!self::isEnabled($action)) {
            return PHP_INT_MAX;
        }
        
        $key = "api:{$botId}:{$action}";
        $config = self::limitFor($action);
        $now = time();

        $details = self::withStorageLock($key, function (array $timestamps) use ($now, $config) {
            $timestamps = self::filterWindow($timestamps, $now, (int)$config['window']);

            return [
                'state' => $timestamps,
                'result' => ['used' => count($timestamps)]
            ];
        });

        return max(0, (int)$config['limit'] - (int)($details['used'] ?? 0));
    }
    
    /**
     * Clean expired data (can be called periodically to prevent infinite memory growth)
     */
    public static function gc(): void {
        $now = time();

        foreach (glob(self::storageDir() . '/*.json') ?: [] as $file) {
            $timestamps = json_decode((string)file_get_contents($file), true);
            if (!is_array($timestamps)) {
                @unlink($file);
                continue;
            }

            $timestamps = array_values(array_filter($timestamps, function ($ts) use ($now) {
                return is_int($ts) && $ts > ($now - 2592000);
            }));

            if (empty($timestamps)) {
                @unlink($file);
            } else {
                file_put_contents($file, json_encode($timestamps), LOCK_EX);
            }
        }
    }
    
    /**
     * Get statistics (for debugging)
     */
    public static function getStats(): array {
        $files = glob(self::storageDir() . '/*.json') ?: [];
        $totalRecords = 0;

        foreach ($files as $file) {
            $timestamps = json_decode((string)file_get_contents($file), true);
            if (is_array($timestamps)) {
                $totalRecords += count($timestamps);
            }
        }

        return [
            'keys_count' => count($files),
            'total_records' => $totalRecords,
            'memory_usage_mb' => memory_get_usage(true) / 1024 / 1024
        ];
    }

    private static function isEnabled(string $action): bool {
        return !empty(self::limitFor($action)['enabled']);
    }

    private static function limitFor(string $action): array {
        if (!isset(self::$limits[$action])) {
            return ['window' => 0, 'limit' => 0, 'enabled' => false];
        }

        $limit = self::$limits[$action];
        $configured = self::configuredLimits()[$action] ?? [];
        foreach (['window', 'limit'] as $field) {
            if (isset($configured[$field]) && is_numeric($configured[$field]) && (int)$configured[$field] > 0) {
                $limit[$field] = (int)$configured[$field];
            }
        }
        if (array_key_exists('enabled', $configured)) {
            $limit['enabled'] = (bool)$configured['enabled'];
        }

        return $limit;
    }

    private static function configuredLimits(): array {
        static $configured = null;
        if ($configured !== null) {
            return $configured;
        }

        $configured = [];
        $configFile = __DIR__ . '/../config/config.php';
        if (is_file($configFile)) {
            $config = require $configFile;
            if (isset($config['rate_limits']) && is_array($config['rate_limits'])) {
                $configured = $config['rate_limits'];
            }
        }

        return $configured;
    }

    private static function storageDir(): string {
        return getenv('RATE_LIMIT_DIR') ?: rtrim(sys_get_temp_dir(), '/') . '/kungfu_rate_limits';
    }
}
