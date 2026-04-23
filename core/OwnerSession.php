<?php

require_once __DIR__ . '/Database.php';
require_once __DIR__ . '/Auth.php';
require_once __DIR__ . '/Response.php';
require_once __DIR__ . '/Security.php';

class OwnerSession
{
    private const SESSION_KEY = 'owner_bot_id';
    private const SESSION_LIFETIME = 60 * 60 * 24;

    private static function boot(): void
    {
        if (session_status() === PHP_SESSION_ACTIVE) {
            return;
        }

        ini_set('session.gc_maxlifetime', (string)self::SESSION_LIFETIME);
        session_set_cookie_params([
            'lifetime' => self::SESSION_LIFETIME,
            'path' => '/',
            'secure' => !empty($_SERVER['HTTPS']) && $_SERVER['HTTPS'] !== 'off',
            'httponly' => true,
            'samesite' => 'Lax',
        ]);

        session_start();
    }

    public static function login(string $name, string $password): array
    {
        $name = trim($name);
        $password = (string)$password;

        $nameValidation = Auth::validateBotName($name);
        if (!$nameValidation['valid']) {
            Response::error(400, 'INVALID_NAME', $nameValidation['errors'][0]);
        }

        $passwordValidation = Auth::validatePassword($password);
        if (!$passwordValidation['valid']) {
            Response::error(400, 'INVALID_PASSWORD', $passwordValidation['errors'][0]);
        }

        Security::rejectApiKeyInContent([$name, $password], 'human credentials');

        $db = Database::getInstance();
        $bot = $db->fetchOne(
            "SELECT id, bot_name, password_hash, status
             FROM tb_bots
             WHERE bot_name = :name AND status = 'active'",
            [':name' => $name]
        );

        if (!$bot || empty($bot['password_hash']) || !Auth::verifyPassword($password, $bot['password_hash'])) {
            Response::error(401, 'INVALID_CREDENTIALS', 'Bot name or password is incorrect');
        }

        self::boot();
        session_regenerate_id(true);
        $_SESSION[self::SESSION_KEY] = (int)$bot['id'];

        return [
            'id' => (int)$bot['id'],
            'bot_name' => $bot['bot_name'],
            'status' => $bot['status'],
        ];
    }

    public static function current(): ?array
    {
        self::boot();
        $botId = (int)($_SESSION[self::SESSION_KEY] ?? 0);
        if ($botId <= 0) {
            return null;
        }

        $db = Database::getInstance();
        $bot = $db->fetchOne(
            "SELECT id, bot_name, balance, status, key_issued_at
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => $botId]
        );

        if (!$bot) {
            unset($_SESSION[self::SESSION_KEY]);
            return null;
        }

        return $bot;
    }

    public static function require(): array
    {
        $bot = self::current();
        if (!$bot) {
            Response::error(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        return $bot;
    }

    public static function logout(): void
    {
        self::boot();
        $_SESSION = [];
        if (ini_get('session.use_cookies')) {
            $params = session_get_cookie_params();
            setcookie(session_name(), '', time() - 42000, $params['path'], $params['domain'], (bool)$params['secure'], (bool)$params['httponly']);
        }
        session_destroy();
    }
}
