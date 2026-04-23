<?php
/**
 * Auth - API Key Authentication
 * Kungfu Platform Core Component
 */

class Auth
{
    private static $currentBot = null;
    
    // Explicitly require Database to avoid implicit dependency errors
    private static function getDb() {
        require_once __DIR__ . '/Database.php';
        return Database::getInstance();
    }

    /**
     * Verify API Key
     * @return array|false Returns Bot info on success, false on failure
     */
    public static function verify(bool $allowQueryKey = false)
    {
        // Get Key
        $key = self::getKeyFromRequest($allowQueryKey);

        if (empty($key)) {
            return false;
        }

        // Validate format
        if (!self::isValidKeyFormat($key)) {
            return false;
        }

        try {
            $db = self::getDb();

            // Query database
            $bot = $db->fetchOne(
                "SELECT id, bot_name, balance, status
                 FROM tb_bots
                 WHERE api_key = :key AND status = 'active'",
                [':key' => $key]
            );

            if (!$bot) {
                return false;
            }

            // Update last active time (async, non-blocking)
            self::updateLastActiveAsync($bot['id']);

            // Cache current Bot info
            self::$currentBot = $bot;

            return $bot;

        } catch (Exception $e) {
            error_log("[Auth Error] " . $e->getMessage());
            return false;
        }
    }

    /**
     * Require authentication (returns error response on failure)
     */
    public static function requireAuth(bool $allowQueryKey = false): array
    {
        $bot = self::verify($allowQueryKey);

        if (!$bot) {
            Response::error(401, 'INVALID_KEY', 'API Key is invalid or expired, please use X-Bot-Key header');
        }

        return $bot;
    }

    /**
     * Require API key in X-Bot-Key header only.
     */
    public static function requireHeaderAuth(): array
    {
        return self::requireAuth(false);
    }

    /**
     * Get current authenticated Bot ID
     */
    public static function getCurrentBotId(): ?int
    {
        return self::$currentBot['id'] ?? null;
    }

    /**
     * Get current authenticated Bot info
     */
    public static function getCurrentBot(): ?array
    {
        return self::$currentBot;
    }

    /**
     * Get Key from request header or URL parameter
     */
    private static function getKeyFromRequest(bool $allowQueryKey = false): ?string
    {
        // URL keys are disabled by default because URLs are commonly logged.
        if ($allowQueryKey && !empty($_GET['key'])) {
            return trim($_GET['key']);
        }

        $headers = [
            'HTTP_X_BOT_KEY',
            'X_BOT_KEY',
        ];

        foreach ($headers as $header) {
            if (!empty($_SERVER[$header])) {
                return trim($_SERVER[$header]);
            }
        }

        if (function_exists('getallheaders')) {
            $headers = getallheaders();
            foreach ($headers as $name => $value) {
                if (strcasecmp($name, 'X-Bot-Key') === 0) {
                    return trim($value);
                }
            }
        }

        return null;
    }

    /**
     * Validate Key format
     */
    private static function isValidKeyFormat(string $key): bool
    {
        // Format: kf_live_64 hex characters
        // Total length: 8 + 64 = 72

        if (strlen($key) !== 72) {
            return false;
        }

        if (strpos($key, 'kf_live_') !== 0) {
            return false;
        }

        $randomPart = substr($key, 8);
        if (!ctype_xdigit($randomPart)) {
            return false;
        }

        return true;
    }

    /**
     * Update last active time (async)
     */
    private static function updateLastActiveAsync(int $botId): void
    {
        // Use register_shutdown_function for async update
        // Avoid updating database synchronously on every request

        register_shutdown_function(function () use ($botId) {
            try {
                // Simple probability sampling to avoid updating every request
                // 10% chance to update, or if last update was more than 5 minutes ago
                if (mt_rand(1, 10) === 1) {
                    // Use require_once inside the closure as context might be different
                    require_once __DIR__ . '/Database.php';
                    $db = Database::getInstance();
                    $db->exec(
                        "UPDATE tb_bots SET last_active_at = NOW() WHERE id = :id",
                        [':id' => $botId]
                    );
                }
            } catch (Exception $e) {
                // Async update failure should not affect main flow
                error_log("[Auth Async] Failed to update last_active: " . $e->getMessage());
            }
        });
    }

    /**
     * Generate new API Key
     */
    public static function generateKey(): string
    {
        $prefix = 'kf_live_';
        $random = bin2hex(random_bytes(32)); // 64 hex characters
        return $prefix . $random;
    }

    /**
     * Validate human owner password.
     */
    public static function validatePassword(string $password): array
    {
        $errors = [];
        $len = strlen($password);

        if ($len < 6) {
            $errors[] = 'Password too short (minimum 6 characters)';
        }
        if ($len > 128) {
            $errors[] = 'Password too long (maximum 128 characters)';
        }
        if (preg_match('/kf_live_[a-f0-9]{64}/i', $password)) {
            $errors[] = 'Password must not contain an API key';
        }

        return [
            'valid' => empty($errors),
            'errors' => $errors
        ];
    }

    public static function hashPassword(string $password): string
    {
        return password_hash($password, PASSWORD_DEFAULT);
    }

    public static function verifyPassword(string $password, string $hash): bool
    {
        return password_verify($password, $hash);
    }

    /**
     * Check if name conforms to naming rules
     */
    public static function validateBotName(string $name): array
    {
        $errors = [];

        // Length check
        $len = strlen($name);
        if ($len < 6) {
            $errors[] = 'Name too short (minimum 6 characters)';
        }
        if ($len > 32) {
            $errors[] = 'Name too long (maximum 32 characters)';
        }

        // Format check
        if (!preg_match('/^[a-zA-Z0-9_.-]+$/', $name)) {
            $errors[] = 'Name contains invalid characters (only letters, numbers, _, ., - allowed)';
        }

        // Reserved word check
        $reserved = ['admin', 'root', 'system', 'api', 'web'];
        if (in_array(strtolower($name), $reserved)) {
            $errors[] = 'Name is a system reserved word';
        }

        return [
            'valid' => empty($errors),
            'errors' => $errors
        ];
    }
}
