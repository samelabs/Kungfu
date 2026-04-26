<?php
/**
 * Auth - API Key Authentication
 * Kungfu Platform Core Component
 */

require_once dirname(__DIR__) . '/repositories/BotRepository.php';
require_once dirname(__DIR__) . '/validators/AuthValidator.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class Auth
{
    private static $currentBot = null;

    /**
     * Verify API Key
     * @return array|false Returns Bot info on success, false on failure
     */
    public static function verify(bool $allowQueryKey = false)
    {
        $key = self::getKeyFromRequest($allowQueryKey);

        if (empty($key)) {
            return false;
        }

        if (!AuthValidator::validateKeyFormat($key)) {
            return false;
        }

        try {
            $bot = BotRepository::findActiveBotByApiKey($key);

            if (!$bot) {
                return false;
            }

            self::updateLastActiveAsync($bot['id']);
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
            throw new AppException(401, 'INVALID_KEY', 'API Key is invalid or expired, please use X-Bot-Key header');
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
        if ($allowQueryKey && !empty($_GET['key'])) {
            return trim($_GET['key']);
        }

        foreach (self::serverHeaderCandidates() as $header) {
            if (!empty($_SERVER[$header])) {
                return trim((string)$_SERVER[$header]);
            }
        }

        return self::headerFromGetAllHeaders();
    }

    private static function serverHeaderCandidates(): array
    {
        return [
            'HTTP_X_BOT_KEY',
            'X_BOT_KEY',
        ];
    }

    private static function headerFromGetAllHeaders(): ?string
    {
        if (!function_exists('getallheaders')) {
            return null;
        }

        $headers = getallheaders();
        foreach ($headers as $name => $value) {
            if (strcasecmp($name, 'X-Bot-Key') === 0) {
                return trim((string)$value);
            }
        }

        return null;
    }

    private static function sampleLastActiveUpdate(): bool
    {
        return mt_rand(1, 10) === 1;
    }

    private static function updateLastActive(int $botId): void
    {
        BotRepository::updateLastActiveAt($botId);
    }

    /**
     * Update last active time (async)
     */
    private static function updateLastActiveAsync(int $botId): void
    {
        register_shutdown_function(function () use ($botId) {
            try {
                if (self::sampleLastActiveUpdate()) {
                    self::updateLastActive($botId);
                }
            } catch (Exception $e) {
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
        return AuthValidator::validatePassword($password);
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
        return AuthValidator::validateBotName($name);
    }
}
