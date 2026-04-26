<?php

require_once dirname(__DIR__) . '/core/Database.php';

class OwnerLogRepository
{
    public static function tableExists(string $table): bool
    {
        static $cache = [];
        if (array_key_exists($table, $cache)) {
            return $cache[$table];
        }

        $result = Database::getInstance()->fetchOne(
            "SELECT 1
             FROM information_schema.tables
             WHERE table_schema = DATABASE() AND table_name = :table
             LIMIT 1",
            [':table' => $table]
        );
        $cache[$table] = (bool)$result;
        return $cache[$table];
    }

    public static function findBalanceByBotId(int $botId): float
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT balance FROM tb_bots WHERE id = :id",
            [':id' => $botId]
        );

        return (float)($row['balance'] ?? 0);
    }

    public static function countCreditLogs(int $botId): int
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_transactions
             WHERE bot_id = :bot_id",
            [':bot_id' => $botId]
        );

        return (int)($row['total'] ?? 0);
    }

    public static function listCreditLogs(int $botId, int $pageSize, int $offset): array
    {
        return Database::getInstance()->query(
            "SELECT id, type, amount, balance_after, ref_type, ref_id, created_at
             FROM tb_transactions
             WHERE bot_id = :bot_id
             ORDER BY id DESC
             LIMIT {$pageSize} OFFSET {$offset}",
            [':bot_id' => $botId]
        );
    }

    public static function countAgentLogs(int $botId): int
    {
        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_logs
             WHERE bot_id = :bot_id",
            [':bot_id' => $botId]
        );

        return (int)($row['total'] ?? 0);
    }

    public static function listAgentLogs(int $botId, int $pageSize, int $offset): array
    {
        return Database::getInstance()->query(
            "SELECT id, action, target_type, target_id, ip_address, user_agent, request_data,
                    success, error_code, error_msg, created_at
             FROM tb_logs
             WHERE bot_id = :bot_id
             ORDER BY id DESC
             LIMIT {$pageSize} OFFSET {$offset}",
            [':bot_id' => $botId]
        );
    }

    public static function listOwnerTasksForFilter(int $botId): array
    {
        return Database::getInstance()->query(
            "SELECT code, title
             FROM tb_tasks
             WHERE bot_id = :bot_id
             ORDER BY created_at DESC",
            [':bot_id' => $botId]
        );
    }

    public static function countTaskLogs(int $botId, string $taskCode = ''): int
    {
        $params = [':bot_id' => $botId];
        $taskWhere = '';
        if ($taskCode !== '') {
            $taskWhere = ' AND l.task_code = :task_code';
            $params[':task_code'] = $taskCode;
        }

        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_task_logs l
             INNER JOIN tb_tasks t ON t.code = l.task_code
             WHERE t.bot_id = :bot_id{$taskWhere}",
            $params
        );

        return (int)($row['total'] ?? 0);
    }

    public static function listTaskLogs(int $botId, int $pageSize, int $offset, string $taskCode = ''): array
    {
        $params = [':bot_id' => $botId];
        $taskWhere = '';
        if ($taskCode !== '') {
            $taskWhere = ' AND l.task_code = :task_code';
            $params[':task_code'] = $taskCode;
        }

        return Database::getInstance()->query(
            "SELECT l.id, l.task_code, l.bot_id, l.action, l.payload_json, l.response_code,
                    l.response_body, l.success, l.error_code, l.error_message, l.created_at
             FROM tb_task_logs l
             INNER JOIN tb_tasks t ON t.code = l.task_code
             WHERE t.bot_id = :bot_id{$taskWhere}
             ORDER BY l.id DESC
             LIMIT {$pageSize} OFFSET {$offset}",
            $params
        );
    }
}
