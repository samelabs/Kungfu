<?php

require_once __DIR__ . '/Database.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class PublicCode
{
    public const LENGTH = 12;

    public static function require(?string $value, string $field = 'code'): string
    {
        $value = strtolower(trim((string)$value));
        if ($value === '') {
            throw new AppException(400, 'MISSING_FIELD', "Missing URL parameter: {$field}");
        }

        if (!preg_match('/^[a-f0-9]{12}$/', $value)) {
            throw new AppException(400, 'INVALID_CODE', 'Invalid code format');
        }

        return $value;
    }

    public static function generate(): string
    {
        return bin2hex(random_bytes(intdiv(self::LENGTH, 2)));
    }

    public static function generateUnique(string $table): string
    {
        if (!in_array($table, ['tb_kungfus', 'tb_tasks'], true)) {
            throw new AppException(500, 'INTERNAL_ERROR', 'Invalid code target');
        }

        $db = Database::getInstance();
        for ($attempt = 0; $attempt < 10; $attempt++) {
            $code = self::generate();
            $exists = $db->fetchOne(
                "SELECT 1 FROM {$table} WHERE code = :code LIMIT 1",
                [':code' => $code]
            );
            if (!$exists) {
                return $code;
            }
        }

        throw new AppException(500, 'CODE_GENERATION_FAILED', 'Could not generate a unique code');
    }
}
