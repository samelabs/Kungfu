<?php

require_once dirname(__DIR__) . '/core/Database.php';
require_once dirname(__DIR__) . '/core/PublicCode.php';
require_once dirname(__DIR__) . '/core/Logger.php';
require_once dirname(__DIR__) . '/core/Transaction.php';
require_once dirname(__DIR__) . '/repositories/TaskRepository.php';
require_once dirname(__DIR__) . '/presenters/OwnerTaskPresenter.php';
require_once dirname(__DIR__) . '/presenters/TaskLogPresenter.php';
require_once dirname(__DIR__) . '/validators/TaskValidator.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class OwnerTaskService
{
    private const REFUND_COOLDOWN_DAYS = 7;

    public static function ensureTaskSchema(): void
    {
        if (!TaskRepository::tableExists('tb_tasks')) {
            throw new AppException(503, 'TASKS_NOT_READY', 'Task management is not ready yet.');
        }

        $required = [
            'code',
            'bot_id',
            'title',
            'requirements',
            'postapi',
            'budget',
            'price',
            'status',
            'created_at',
            'updated_at',
            'opened_at',
            'closed_at'
        ];

        foreach ($required as $column) {
            if (!TaskRepository::columnExists('tb_tasks', $column)) {
                throw new AppException(503, 'TASKS_NOT_READY', 'Task management is not ready yet.');
            }
        }
    }

    public static function listTasks(int $botId): array
    {
        $rows = TaskRepository::listOwnerTasksWithStats($botId);
        $tasks = array_map([OwnerTaskPresenter::class, 'summary'], $rows);

        return [
            'tasks' => $tasks,
            'meta' => [
                'returned' => count($tasks)
            ]
        ];
    }

    public static function getTask(int $botId, string $code): array
    {
        $task = self::ownerTask($botId, $code);
        $logs = [];
        if (TaskRepository::tableExists('tb_task_logs')) {
            $logs = TaskRepository::findRecentLogsByTaskCode($code);
        }

        return [
            'task' => OwnerTaskPresenter::detail($task),
            'logs' => array_map([TaskLogPresenter::class, 'ownerTaskDetailRow'], $logs)
        ];
    }

    public static function createTask(int $botId, array $config, array $input): array
    {
        $title = trim((string)($input['title'] ?? ''));
        $requirements = trim((string)($input['requirements'] ?? ''));
        $postapi = trim((string)($input['postapi'] ?? ''));
        $budget = TaskValidator::parseMoney($input['budget'] ?? null, 'budget');
        $price = TaskValidator::parseMoney($input['price'] ?? null, 'price');
        $openNow = !empty($input['open_now']);

        TaskValidator::validatePayload($title, $requirements, $postapi, $budget, $price, $config);
        if ($openNow) {
            TaskValidator::assertOpenable($postapi, $budget, $price);
        }

        $db = Database::getInstance();
        $taskCode = PublicCode::generateUnique('tb_tasks');
        $status = $openNow ? 'open' : 'pending';
        $now = date('Y-m-d H:i:s');
        $startedTransaction = !$db->inTransaction();

        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            self::lockTaskBudget($botId, $budget, $taskCode);

            TaskRepository::insertTask([
                'code' => $taskCode,
                'bot_id' => $botId,
                'title' => $title,
                'requirements' => $requirements,
                'postapi' => $postapi,
                'budget' => $budget,
                'price' => $price,
                'pinned' => 0,
                'status' => $status,
                'opened_at' => $openNow ? $now : null
            ]);

            if ($startedTransaction) {
                $db->commit();
            }
        } catch (Exception $e) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            if ($e->getCode() == 402) {
                throw new AppException(402, 'INSUFFICIENT_CREDITS', 'Not enough credits to fund this task budget. Complete platform tasks to earn credits.');
            }
            throw $e;
        }

        Logger::log($botId, 'owner_task_create', 'task', $taskCode, null, null, true, null, null, [
            'title' => $title,
            'status' => $status
        ]);

        return [
            'task' => OwnerTaskPresenter::detail(self::ownerTask($botId, $taskCode))
        ];
    }

    public static function setTaskStatus(int $botId, string $code, string $status): array
    {
        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();
        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            $task = self::ownerTaskForUpdate($botId, $code);
            if ($status === 'open') {
                TaskValidator::assertOpenable((string)$task['postapi'], (float)$task['budget'], (float)$task['price']);
                TaskRepository::openOwnerTask($botId, $code);
            } else {
                TaskRepository::closeOwnerTask($botId, $code);
            }

            if ($startedTransaction) {
                $db->commit();
            }
        } catch (Exception $e) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            throw $e;
        }

        Logger::log($botId, $status === 'open' ? 'owner_task_open' : 'owner_task_close', 'task', $code);

        return [
            'task' => OwnerTaskPresenter::detail(self::ownerTask($botId, $code))
        ];
    }

    public static function addTaskBudget(int $botId, string $code, array $input): array
    {
        $amount = TaskValidator::parseMoney($input['amount'] ?? null, 'amount');
        if ($amount <= 0) {
            throw new AppException(400, 'INVALID_AMOUNT', 'Budget amount must be greater than zero');
        }

        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();
        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            self::ownerTaskForUpdate($botId, $code);
            self::lockTaskBudget($botId, $amount, $code);
            TaskRepository::addOwnerTaskBudget($botId, $code, $amount);

            if ($startedTransaction) {
                $db->commit();
            }
        } catch (Exception $e) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            if ($e->getCode() == 402) {
                throw new AppException(402, 'INSUFFICIENT_CREDITS', 'Not enough credits to add this task budget. Complete platform tasks to earn credits.');
            }
            throw $e;
        }

        Logger::log($botId, 'owner_task_add_budget', 'task', $code, null, null, true, null, null, [
            'amount' => $amount
        ]);

        return [
            'task' => OwnerTaskPresenter::detail(self::ownerTask($botId, $code))
        ];
    }

    public static function updateTaskBasics(int $botId, string $code, array $config, array $input): array
    {
        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();

        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            $task = self::ownerTaskForUpdate($botId, $code);
            if ((string)$task['status'] !== 'closed') {
                throw new AppException(409, 'TASK_MUST_BE_CLOSED', 'Task must be closed before editing.');
            }

            $title = array_key_exists('title', $input) ? trim((string)$input['title']) : (string)$task['title'];
            $requirements = array_key_exists('requirements', $input) ? trim((string)$input['requirements']) : (string)$task['requirements'];
            $postapi = array_key_exists('postapi', $input) ? trim((string)$input['postapi']) : (string)$task['postapi'];
            $price = array_key_exists('price', $input) ? TaskValidator::parseMoney($input['price'], 'price') : (float)$task['price'];

            TaskValidator::validateBasics($title, $requirements, $postapi, $price, $config);

            TaskRepository::updateOwnerTaskBasics($botId, $code, $title, $requirements, $postapi, $price);

            if ($startedTransaction) {
                $db->commit();
            }
        } catch (Exception $e) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            throw $e;
        }

        Logger::log($botId, 'owner_task_edit', 'task', $code, null, null, true, null, null, [
            'title' => $title,
            'price' => $price
        ]);

        return [
            'task' => OwnerTaskPresenter::detail(self::ownerTask($botId, $code))
        ];
    }

    public static function refundTaskBudget(int $botId, string $code): array
    {
        $db = Database::getInstance();
        $startedTransaction = !$db->inTransaction();

        try {
            if ($startedTransaction) {
                $db->beginTransaction();
            }

            $task = self::ownerTaskForUpdate($botId, $code);
            if ((string)$task['status'] !== 'closed') {
                throw new AppException(409, 'TASK_MUST_BE_CLOSED', 'Only closed tasks can refund budget.');
            }

            $budget = (float)$task['budget'];
            if ($budget <= 0) {
                throw new AppException(409, 'TASK_BUDGET_EMPTY', 'Task budget is already zero.');
            }

            $closedAt = (string)($task['closed_at'] ?? '');
            if (!self::canRefundBudget($closedAt)) {
                throw new AppException(409, 'TASK_REFUND_NOT_READY', 'Refund is available 7 days after closing.');
            }

            Transaction::record($botId, 'refund_task', $budget, 'task', $code);
            TaskRepository::clearOwnerTaskBudget($botId, $code);

            if ($startedTransaction) {
                $db->commit();
            }
        } catch (Exception $e) {
            if ($startedTransaction && $db->inTransaction()) {
                $db->rollback();
            }
            throw $e;
        }

        Logger::log($botId, 'owner_task_refund', 'task', $code, null, null, true, null, null, [
            'amount' => $budget
        ]);

        return [
            'task' => OwnerTaskPresenter::detail(self::ownerTask($botId, $code))
        ];
    }

    private static function ownerTask(int $botId, string $code): array
    {
        $task = TaskRepository::findOwnerTaskByCode($botId, $code);
        if (!$task) {
            throw new AppException(404, 'NOT_FOUND', 'Task not found');
        }

        return $task;
    }

    private static function ownerTaskForUpdate(int $botId, string $code): array
    {
        $task = TaskRepository::findOwnerTaskByCodeForUpdate($botId, $code);
        if (!$task) {
            throw new AppException(404, 'NOT_FOUND', 'Task not found');
        }

        return $task;
    }

    private static function lockTaskBudget(int $botId, float $amount, string $taskCode): void
    {
        if ($amount <= 0) {
            return;
        }

        Transaction::record($botId, 'lock_task', -$amount, 'task', $taskCode);
    }

    private static function canRefundBudget(string $closedAt): bool
    {
        if ($closedAt === '') {
            return false;
        }

        $closed = strtotime($closedAt);
        if ($closed === false) {
            return false;
        }

        return $closed <= (time() - (self::REFUND_COOLDOWN_DAYS * 86400));
    }
}
