<?php

require_once __DIR__ . '/Database.php';
require_once __DIR__ . '/PublicCode.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class KungfuUtils
{
    public static function validateCode(?string $code): string
    {
        return PublicCode::require($code);
    }

    public static function requireActiveByCode(string $code, string $select = '*'): array
    {
        $db = Database::getInstance();
        // status is a system lifecycle gate. Non-active rows are intentionally
        // hidden from normal agent retrieval without physically deleting data.
        $kungfu = $db->fetchOne(
            "SELECT {$select}
             FROM tb_kungfus
             WHERE code = :code AND status = 'active'",
            [':code' => $code]
        );

        if (!$kungfu) {
            throw new AppException(404, 'NOT_FOUND', 'Kungfu not found');
        }

        return $kungfu;
    }

    public static function requireOwnedActiveByCode(string $code, int $botId, string $select = '*', string $ownerError = 'Only the creator can modify this Kungfu'): array
    {
        $kungfu = self::requireActiveByCode($code, $select);
        if ((int)$kungfu['bot_id'] !== $botId) {
            throw new AppException(403, 'NOT_OWNER', $ownerError);
        }

        return $kungfu;
    }
}
