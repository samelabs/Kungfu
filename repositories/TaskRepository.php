<?php

require_once dirname(__DIR__) . '/core/Database.php';
require_once dirname(__DIR__) . '/core/TaskUtils.php';

class TaskRepository
{
    public static function countOpenTasks(): int
    {
        $openWhere = TaskUtils::openBudgetWhereClause('t');
        $row = Database::getInstance()->fetchOne(
            "SELECT COUNT(*) AS count
             FROM tb_tasks t
             WHERE {$openWhere}"
        );

        return (int)($row['count'] ?? 0);
    }

    public static function listOpenTasks(): array
    {
        $openWhere = TaskUtils::openBudgetWhereClause('t');
        return Database::getInstance()->query(
            "SELECT t.*
             FROM tb_tasks t
             WHERE {$openWhere}
             ORDER BY t.pinned DESC, t.created_at DESC"
        );
    }

    public static function findOpenTaskByCode(string $code): ?array
    {
        $openWhere = TaskUtils::openBudgetWhereClause('t');
        return Database::getInstance()->fetchOne(
            "SELECT t.*
             FROM tb_tasks t
             WHERE t.code = :code AND {$openWhere}",
            [':code' => $code]
        );
    }

    public static function tableExists(string $table): bool
    {
        static $cache = [];
        if (array_key_exists($table, $cache)) {
            return $cache[$table];
        }

        $db = Database::getInstance();
        $result = $db->fetchOne(
            "SELECT 1
             FROM information_schema.tables
             WHERE table_schema = DATABASE() AND table_name = :table
             LIMIT 1",
            [':table' => $table]
        );
        $cache[$table] = (bool)$result;
        return $cache[$table];
    }

    public static function columnExists(string $table, string $column): bool
    {
        static $cache = [];
        $key = "{$table}.{$column}";
        if (array_key_exists($key, $cache)) {
            return $cache[$key];
        }

        $db = Database::getInstance();
        $result = $db->fetchOne(
            "SELECT 1
             FROM information_schema.columns
             WHERE table_schema = DATABASE()
               AND table_name = :table
               AND column_name = :column
             LIMIT 1",
            [':table' => $table, ':column' => $column]
        );
        $cache[$key] = (bool)$result;
        return $cache[$key];
    }

    public static function listOwnerTasksWithStats(int $botId): array
    {
        $db = Database::getInstance();

        if (self::tableExists('tb_task_logs')) {
            return $db->query(
                "SELECT t.code, t.title, t.requirements, t.budget, t.price, t.pinned, t.status,
                        t.created_at, t.updated_at, t.opened_at, t.closed_at,
                        COALESCE(ls.log_count, 0) AS log_count,
                        COALESCE(ls.success_count, 0) AS success_count,
                        COALESCE(ls.failure_count, 0) AS failure_count
                 FROM tb_tasks t
                 LEFT JOIN (
                     SELECT task_code,
                            COUNT(*) AS log_count,
                            SUM(CASE WHEN action = 'post_succeeded' THEN 1 ELSE 0 END) AS success_count,
                            SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END) AS failure_count
                     FROM tb_task_logs
                     GROUP BY task_code
                 ) ls ON ls.task_code = t.code
                 WHERE t.bot_id = :bot_id
                 ORDER BY t.created_at DESC",
                [':bot_id' => $botId]
            );
        }

        return $db->query(
            "SELECT t.code, t.title, t.requirements, t.budget, t.price, t.pinned, t.status,
                    t.created_at, t.updated_at, t.opened_at, t.closed_at,
                    0 AS log_count, 0 AS success_count, 0 AS failure_count
             FROM tb_tasks t
             WHERE t.bot_id = :bot_id
             ORDER BY t.created_at DESC",
            [':bot_id' => $botId]
        );
    }

    public static function findOwnerTaskByCode(int $botId, string $code): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT code, bot_id, title, requirements, postapi, budget, price, pinned, status,
                    review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
             FROM tb_tasks
             WHERE code = :code AND bot_id = :bot_id",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function findTaskByCode(string $code): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT * FROM tb_tasks WHERE code = :code",
            [':code' => $code]
        );
    }

    public static function findTaskBudgetStatusByCode(string $code): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, budget, status
             FROM tb_tasks
             WHERE code = :code",
            [':code' => $code]
        );
    }

    public static function findTaskBudgetStatusByCodeForUpdate(string $code): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT id, budget, status
             FROM tb_tasks
             WHERE code = :code
             FOR UPDATE",
            [':code' => $code]
        );
    }

    public static function findOwnerTaskByCodeForUpdate(int $botId, string $code): ?array
    {
        return Database::getInstance()->fetchOne(
            "SELECT code, bot_id, title, requirements, postapi, budget, price, pinned, status,
                    review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
             FROM tb_tasks
             WHERE code = :code AND bot_id = :bot_id
             FOR UPDATE",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function findRecentLogsByTaskCode(string $code, int $limit = 50): array
    {
        return Database::getInstance()->query(
            "SELECT id, bot_id, action, response_code, success, error_code, error_message, created_at
             FROM tb_task_logs
             WHERE task_code = :code
             ORDER BY created_at DESC
             LIMIT {$limit}",
            [':code' => $code]
        );
    }

    public static function insertTask(array $taskData): void
    {
        Database::getInstance()->insert('tb_tasks', $taskData);
    }

    public static function openOwnerTask(int $botId, string $code): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET status = 'open', opened_at = COALESCE(opened_at, NOW()), closed_at = NULL
             WHERE code = :code AND bot_id = :bot_id",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function closeOwnerTask(int $botId, string $code): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET status = 'closed', closed_at = NOW()
             WHERE code = :code AND bot_id = :bot_id",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function addOwnerTaskBudget(int $botId, string $code, float $amount): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET budget = budget + :amount
             WHERE code = :code AND bot_id = :bot_id",
            [':amount' => $amount, ':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function clearOwnerTaskBudget(int $botId, string $code): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET budget = 0
             WHERE code = :code AND bot_id = :bot_id",
            [':code' => $code, ':bot_id' => $botId]
        );
    }

    public static function updateOwnerTaskBasics(
        int $botId,
        string $code,
        string $title,
        string $requirements,
        string $postapi,
        float $price
    ): void {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET title = :title,
                 requirements = :requirements,
                 postapi = :postapi,
                 price = :price
             WHERE code = :code AND bot_id = :bot_id",
            [
                ':title' => $title,
                ':requirements' => $requirements,
                ':postapi' => $postapi,
                ':price' => $price,
                ':code' => $code,
                ':bot_id' => $botId
            ]
        );
    }

    public static function updateTaskBudgetAndStatus(int $id, float $budget, string $status, bool $shouldClose): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET budget = :budget,
                 status = :status,
                 closed_at = CASE WHEN :should_close = 1 THEN NOW() ELSE closed_at END
             WHERE id = :id",
            [
                ':budget' => $budget,
                ':status' => $status,
                ':should_close' => $shouldClose ? 1 : 0,
                ':id' => $id
            ]
        );
    }

    public static function decrementTaskBudgetForDelivery(int $id, float $price, float $minOpenBudget): void
    {
        Database::getInstance()->exec(
            "UPDATE tb_tasks
             SET budget = budget - :price_debit,
                 status = CASE WHEN budget - :price_close_check_status < :min_open_budget_status THEN 'closed' ELSE status END,
                 closed_at = CASE WHEN budget - :price_close_check_closed < :min_open_budget_closed THEN NOW() ELSE closed_at END
             WHERE id = :id",
            [
                ':price_debit' => $price,
                ':price_close_check_status' => $price,
                ':price_close_check_closed' => $price,
                ':min_open_budget_status' => $minOpenBudget,
                ':min_open_budget_closed' => $minOpenBudget,
                ':id' => $id
            ]
        );
    }
}
