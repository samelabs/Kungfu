<?php

require_once dirname(__DIR__) . '/core/Database.php';

class BotRepository
{
    public static function findActiveBotAccountById(int $botId): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT bot_name, status, balance
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => $botId]
        );
    }

    public static function findActiveBotKeyById(int $botId): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, bot_name, api_key, balance, status, key_issued_at
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => $botId]
        );
    }

    public static function findActiveBotByApiKey(string $key): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, bot_name, balance, status
             FROM tb_bots
             WHERE api_key = :key AND status = 'active'",
            [':key' => $key]
        );
    }

    public static function findActiveBotSummaryById(int $botId): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, bot_name, balance, status
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => $botId]
        );
    }

    public static function botNameExists(string $name): bool
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT 1 FROM tb_bots WHERE bot_name = :name",
            [':name' => $name]
        );

        return (bool)$row;
    }

    public static function findActiveBotCredentialsByName(string $name): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, bot_name, password_hash, status
             FROM tb_bots
             WHERE bot_name = :name AND status = 'active'",
            [':name' => $name]
        );
    }

    public static function findOwnerSessionBotById(int $botId): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, bot_name, balance, status, key_issued_at
             FROM tb_bots
             WHERE id = :id AND status = 'active'",
            [':id' => $botId]
        );
    }

    public static function kungfuStatsByBotId(int $botId): array
    {
        return Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS total,
                    SUM(CASE WHEN visibility = 'public' THEN 1 ELSE 0 END) AS public_total
             FROM tb_kungfus
             WHERE bot_id = :bot_id AND status = 'active'",
            [':bot_id' => $botId]
        ) ?: ['total' => 0, 'public_total' => 0];
    }

    public static function platformTaskCountByBotId(int $botId): int
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_tasks
             WHERE bot_id = :bot_id",
            [':bot_id' => $botId]
        );

        return (int)($row['total'] ?? 0);
    }

    public static function updatePasswordHashById(int $botId, string $passwordHash): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_bots SET password_hash = :password_hash WHERE id = :id",
            [
                ':password_hash' => $passwordHash,
                ':id' => $botId
            ]
        );
    }

    public static function updateApiKeyById(int $botId, string $newKey): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_bots SET api_key = :new_key, key_issued_at = NOW() WHERE id = :id",
            [
                ':new_key' => $newKey,
                ':id' => $botId
            ]
        );
    }

    public static function updateLastActiveAt(int $botId): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_bots SET last_active_at = NOW() WHERE id = :id",
            [':id' => $botId]
        );
    }

    public static function insertRegisteredBot(
        string $name,
        string $apiKey,
        string $passwordHash,
        string $ip,
        string $keyIssuedAt,
        string $lastActiveAt
    ): int {
        return Database::getInstance()->insert('tb_bots', [
            'bot_name' => $name,
            'api_key' => $apiKey,
            'password_hash' => $passwordHash,
            'key_issued_at' => $keyIssuedAt,
            'balance' => 66,
            'register_ip' => $ip,
            'status' => 'active',
            'last_active_at' => $lastActiveAt
        ]);
    }
}
