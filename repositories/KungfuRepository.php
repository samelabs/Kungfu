<?php

require_once dirname(__DIR__) . '/core/Database.php';
require_once dirname(__DIR__) . '/core/PublicCode.php';

class KungfuRepository
{
    public static function countActiveByBotId(int $botId): int
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) as total
             FROM tb_kungfus
             WHERE bot_id = :bot_id AND status = 'active'",
            [':bot_id' => $botId]
        );

        return (int)($row['total'] ?? 0);
    }

    public static function listActiveByBotId(int $botId, int $limit, int $offset): array
    {
        return Database::getInstance()->query(
            "SELECT code, title, tags_json, description, visibility, created_at, updated_at
             FROM tb_kungfus
             WHERE bot_id = :bot_id AND status = 'active'
             ORDER BY updated_at DESC
             LIMIT :limit OFFSET :offset",
            [':bot_id' => $botId, ':limit' => $limit, ':offset' => $offset]
        );
    }

    public static function findActiveByCode(string $code, string $select = '*'): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT {$select}
             FROM tb_kungfus
             WHERE code = :code AND status = 'active'",
            [':code' => $code]
        );
    }

    public static function findOwnedActiveByCode(int $botId, string $code, string $select = '*'): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT {$select}
             FROM tb_kungfus
             WHERE code = :code AND bot_id = :bot_id AND status = 'active'",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function updateVisibilityById(int $id, string $visibility): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_kungfus SET visibility = :visibility, updated_at = NOW() WHERE id = :id",
            [':visibility' => $visibility, ':id' => $id]
        );
    }

    public static function softDeleteById(int $id): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_kungfus SET status = 'deleted' WHERE id = :id",
            [':id' => $id]
        );
    }

    public static function updateContentById(
        int $id,
        string $title,
        array $tags,
        string $description,
        string $content,
        string $checksum
    ): void {
        Database::getInstance()->exec(
            "UPDATE tb_kungfus
             SET title = :title, tags_json = :tags, description = :description,
                 content = :content, checksum = :checksum, updated_at = NOW()
             WHERE id = :id",
            [
                ':title' => $title,
                ':tags' => json_encode($tags, JSON_UNESCAPED_UNICODE),
                ':description' => $description,
                ':content' => $content,
                ':checksum' => $checksum,
                ':id' => $id
            ]
        );
    }

    public static function generateUniqueCode(): string
    {
        return PublicCode::generateUnique('tb_kungfus');
    }

    public static function insertNewKungfu(
        string $code,
        int $botId,
        string $title,
        array $tags,
        string $description,
        string $content,
        string $checksum
    ): void {
        Database::getInstance()->insert('tb_kungfus', [
            'code' => $code,
            'bot_id' => $botId,
            'title' => $title,
            'tags_json' => json_encode($tags, JSON_UNESCAPED_UNICODE),
            'description' => $description,
            'content' => $content,
            'checksum' => $checksum,
            'visibility' => 'private',
            'status' => 'active'
        ]);
    }
}
