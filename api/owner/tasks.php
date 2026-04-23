<?php
/**
 * API Endpoint: /api/owner/tasks
 * Function: Owner task creation and management.
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';
require_once __DIR__ . '/../../core/PublicCode.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/Transaction.php';

$config = require __DIR__ . '/../../config/config.php';

$bot = OwnerSession::require();
$botId = (int)$bot['id'];
$code = isset($_GET['code']) && $_GET['code'] !== '' ? TaskUtils::validateCode($_GET['code']) : '';
$action = isset($_GET['action']) ? trim((string)$_GET['action']) : '';

try {
    ensureTaskSchema();

    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        if ($code === '') {
            listTasks($botId);
        }
        getTask($botId, $code);
    }

    if ($_SERVER['REQUEST_METHOD'] === 'POST') {
        if ($code === '') {
            createTask($botId, $config);
        }

        if ($action === 'open') {
            setTaskStatus($botId, $code, 'open');
        }
        if ($action === 'close') {
            setTaskStatus($botId, $code, 'closed');
        }
        if ($action === 'add-budget') {
            addTaskBudget($botId, $code);
        }
        if ($action === 'edit') {
            updateTaskBasics($botId, $code, $config);
        }

        Response::error(400, 'INVALID_ACTION', 'Invalid task action');
    }

    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET and POST requests allowed');
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Owner task API failed: ' . $e->getMessage(), [
        'bot_id' => $botId,
        'code' => $code,
        'action' => $action
    ]);
    Response::error(500, 'INTERNAL_ERROR', 'Owner task operation failed');
}

function ensureTaskSchema(): void
{
    if (!tableExists('tb_tasks')) {
        Response::error(503, 'TASKS_NOT_READY', 'Task management is not ready yet.');
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

    $missing = [];
    foreach ($required as $column) {
        if (!columnExists('tb_tasks', $column)) {
            $missing[] = $column;
        }
    }

    if (!empty($missing)) {
        Response::error(503, 'TASKS_NOT_READY', 'Task management is not ready yet.');
    }
}

function listTasks(int $botId): void
{
    $db = Database::getInstance();
    if (tableExists('tb_task_logs')) {
        $rows = $db->query(
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
    } else {
        $rows = $db->query(
            "SELECT t.code, t.title, t.requirements, t.budget, t.price, t.pinned, t.status,
                    t.created_at, t.updated_at, t.opened_at, t.closed_at,
                    0 AS log_count, 0 AS success_count, 0 AS failure_count
             FROM tb_tasks t
             WHERE t.bot_id = :bot_id
             ORDER BY t.created_at DESC",
            [':bot_id' => $botId]
        );
    }

    $tasks = [];
    foreach ($rows as $row) {
        $tasks[] = formatOwnerTask($row, false);
    }

    Response::success([
        'tasks' => $tasks,
        'meta' => [
            'returned' => count($tasks)
        ]
    ]);
}

function getTask(int $botId, string $code): void
{
    $db = Database::getInstance();
    $task = ownerTask($botId, $code);
    $logs = [];
    if (tableExists('tb_task_logs')) {
        $logs = $db->query(
            "SELECT id, bot_id, action, response_code, success, error_code, error_message, created_at
             FROM tb_task_logs
             WHERE task_code = :code
             ORDER BY created_at DESC
             LIMIT 50",
            [':code' => $code]
        );
    }

    Response::success([
        'task' => formatOwnerTask($task, true),
        'logs' => array_map('formatTaskLog', $logs)
    ]);
}

function createTask(int $botId, array $config): void
{
    $input = readJson();
    $title = trim((string)($input['title'] ?? ''));
    $requirements = trim((string)($input['requirements'] ?? ''));
    $postapi = trim((string)($input['postapi'] ?? ''));
    $budget = parseMoney($input['budget'] ?? null, 'budget');
    $price = parseMoney($input['price'] ?? null, 'price');
    $openNow = !empty($input['open_now']);

    validateTaskPayload($title, $requirements, $postapi, $budget, $price, $config);
    if ($openNow) {
        assertOpenable($postapi, $budget, $price);
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

        lockTaskBudget($botId, $budget, $taskCode);

        $db->insert('tb_tasks', [
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
            Response::error(402, 'INSUFFICIENT_CREDITS', 'Not enough credits to fund this task budget. Complete platform tasks to earn credits.');
        }
        throw $e;
    }

    Logger::log($botId, 'owner_task_create', 'task', $taskCode, null, null, true, null, null, [
        'title' => $title,
        'status' => $status
    ]);

    Response::success([
        'task' => formatOwnerTask(ownerTask($botId, $taskCode), true)
    ], 'Task created');
}

function setTaskStatus(int $botId, string $code, string $status): void
{
    $db = Database::getInstance();
    $startedTransaction = !$db->inTransaction();
    try {
        if ($startedTransaction) {
            $db->beginTransaction();
        }

        $task = ownerTaskForUpdate($botId, $code);
        if ($status === 'open') {
            assertOpenable((string)$task['postapi'], (float)$task['budget'], (float)$task['price']);
            $db->exec(
                "UPDATE tb_tasks
                 SET status = 'open', opened_at = COALESCE(opened_at, NOW()), closed_at = NULL
                 WHERE code = :code AND bot_id = :bot_id",
                [':code' => $code, ':bot_id' => $botId]
            );
        } else {
            $refund = (float)$task['budget'];
            if ($refund > 0) {
                Transaction::record($botId, 'refund_task', $refund, 'task', $code);
            }
            $db->exec(
                "UPDATE tb_tasks
                 SET status = 'closed', budget = 0, closed_at = COALESCE(closed_at, NOW())
                 WHERE code = :code AND bot_id = :bot_id",
                [':code' => $code, ':bot_id' => $botId]
            );
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

    Logger::log($botId, 'owner_task_' . $status, 'task', $code);
    Response::success([
        'task' => formatOwnerTask(ownerTask($botId, $code), true)
    ], $status === 'open' ? 'Task opened' : 'Task closed');
}

function addTaskBudget(int $botId, string $code): void
{
    $input = readJson();
    $amount = parseMoney($input['amount'] ?? null, 'amount');
    if ($amount <= 0) {
        Response::error(400, 'INVALID_AMOUNT', 'Budget amount must be greater than zero');
    }
    $db = Database::getInstance();
    $startedTransaction = !$db->inTransaction();
    try {
        if ($startedTransaction) {
            $db->beginTransaction();
        }

        ownerTaskForUpdate($botId, $code);
        lockTaskBudget($botId, $amount, $code);
        $db->exec(
            "UPDATE tb_tasks
             SET budget = budget + :amount
             WHERE code = :code AND bot_id = :bot_id",
            [':amount' => $amount, ':code' => $code, ':bot_id' => $botId]
        );

        if ($startedTransaction) {
            $db->commit();
        }
    } catch (Exception $e) {
        if ($startedTransaction && $db->inTransaction()) {
            $db->rollback();
        }
        if ($e->getCode() == 402) {
            Response::error(402, 'INSUFFICIENT_CREDITS', 'Not enough credits to add this task budget. Complete platform tasks to earn credits.');
        }
        throw $e;
    }

    Logger::log($botId, 'owner_task_add_budget', 'task', $code, null, null, true, null, null, [
        'amount' => $amount
    ]);
    Response::success([
        'task' => formatOwnerTask(ownerTask($botId, $code), true)
    ], 'Budget added');
}

function updateTaskBasics(int $botId, string $code, array $config): void
{
    $input = readJson();
    $db = Database::getInstance();
    $startedTransaction = !$db->inTransaction();

    try {
        if ($startedTransaction) {
            $db->beginTransaction();
        }

        $task = ownerTaskForUpdate($botId, $code);

        $title = array_key_exists('title', $input) ? trim((string)$input['title']) : (string)$task['title'];
        $requirements = array_key_exists('requirements', $input) ? trim((string)$input['requirements']) : (string)$task['requirements'];
        $postapi = array_key_exists('postapi', $input) ? trim((string)$input['postapi']) : (string)$task['postapi'];
        $price = array_key_exists('price', $input) ? parseMoney($input['price'], 'price') : (float)$task['price'];

        validateTaskBasics($title, $requirements, $postapi, $price, $config);

        $status = (string)$task['status'];
        $budget = (float)$task['budget'];
        if ($status === 'open') {
            assertOpenable($postapi, $budget, $price);
        }

        $db->exec(
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

    Response::success([
        'task' => formatOwnerTask(ownerTask($botId, $code), true)
    ], 'Task updated');
}

function ownerTask(int $botId, string $code): array
{
    $db = Database::getInstance();
    $task = $db->fetchOne(
        "SELECT code, bot_id, title, requirements, postapi, budget, price, pinned, status,
                review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
         FROM tb_tasks
         WHERE code = :code AND bot_id = :bot_id",
        [':code' => $code, ':bot_id' => $botId]
    );
    if (!$task) {
        Response::error(404, 'NOT_FOUND', 'Task not found');
    }

    return $task;
}

function ownerTaskForUpdate(int $botId, string $code): array
{
    $db = Database::getInstance();
    $task = $db->fetchOne(
        "SELECT code, bot_id, title, requirements, postapi, budget, price, pinned, status,
                review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
         FROM tb_tasks
         WHERE code = :code AND bot_id = :bot_id
         FOR UPDATE",
        [':code' => $code, ':bot_id' => $botId]
    );
    if (!$task) {
        Response::error(404, 'NOT_FOUND', 'Task not found');
    }

    return $task;
}

function lockTaskBudget(int $botId, float $amount, string $taskCode): void
{
    if ($amount <= 0) {
        return;
    }

    Transaction::record($botId, 'lock_task', -$amount, 'task', $taskCode);
}

function validateTaskPayload(string $title, string $requirements, string $postapi, float $budget, float $price, array $config): void
{
    validateTaskBasics($title, $requirements, $postapi, $price, $config);
    if ($budget <= 0) {
        Response::error(400, 'INVALID_BUDGET', 'Budget must be greater than zero');
    }
}

function validateTaskBasics(string $title, string $requirements, string $postapi, float $price, array $config): void
{
    if ($title === '') {
        Response::error(400, 'MISSING_FIELD', 'Missing required field: title');
    }
    if (mb_strlen($title) > (int)$config['max_title_length']) {
        Response::error(400, 'TITLE_TOO_LONG', 'Title maximum 128 characters');
    }
    if ($requirements === '') {
        Response::error(400, 'MISSING_FIELD', 'Missing required field: requirements');
    }
    if (mb_strlen($requirements) > 20000) {
        Response::error(400, 'REQUIREMENTS_TOO_LONG', 'Requirements maximum 20000 characters');
    }
    validatePostapi($postapi);
    if ($price <= 0) {
        Response::error(400, 'INVALID_PRICE', 'Price must be greater than zero');
    }
}

function validatePostapi(string $postapi): void
{
    if ($postapi === '') {
        Response::error(400, 'MISSING_FIELD', 'Missing required field: postapi');
    }
    if (strlen($postapi) > 2048) {
        Response::error(400, 'POSTAPI_TOO_LONG', 'Postapi maximum 2048 characters');
    }
    if (!filter_var($postapi, FILTER_VALIDATE_URL)) {
        Response::error(400, 'INVALID_POSTAPI', 'Postapi must be a valid URL');
    }
    $scheme = strtolower((string)parse_url($postapi, PHP_URL_SCHEME));
    if (!in_array($scheme, ['http', 'https'], true)) {
        Response::error(400, 'INVALID_POSTAPI', 'Postapi must use http or https');
    }
}

function assertOpenable(string $postapi, float $budget, float $price): void
{
    validatePostapi($postapi);
    if ($price <= 0) {
        Response::error(400, 'INVALID_PRICE', 'Price must be greater than zero');
    }
    if ($budget < TaskUtils::MIN_OPEN_BUDGET || $budget < $price) {
        Response::error(400, 'TASK_BUDGET_TOO_LOW', 'Open tasks require enough budget');
    }
}

function parseMoney($value, string $field): float
{
    if ($value === null || $value === '') {
        Response::error(400, 'MISSING_FIELD', "Missing required field: {$field}");
    }
    if (!is_numeric($value)) {
        Response::error(400, 'INVALID_MONEY', "{$field} must be numeric");
    }
    return round((float)$value, 4);
}

function readJson(): array
{
    $input = json_decode(file_get_contents('php://input'), true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
        Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON object');
    }
    return $input;
}

function formatOwnerTask(array $row, bool $includePrivate): array
{
    $task = [
        'code' => $row['code'],
        'title' => $row['title'],
        'requirements' => $row['requirements'],
        'budget' => (float)$row['budget'],
        'price' => (float)$row['price'],
        'pinned' => (int)($row['pinned'] ?? 0),
        'status' => $row['status'],
        'created_at' => $row['created_at'],
        'updated_at' => $row['updated_at'],
        'opened_at' => $row['opened_at'] ?? null,
        'closed_at' => $row['closed_at'] ?? null,
        'log_count' => (int)($row['log_count'] ?? 0),
        'success_count' => (int)($row['success_count'] ?? 0),
        'failure_count' => (int)($row['failure_count'] ?? 0)
    ];

    if ($includePrivate) {
        $task['postapi'] = $row['postapi'];
        $task['review_note'] = $row['review_note'] ?? null;
        $task['reviewed_at'] = $row['reviewed_at'] ?? null;
    }

    return $task;
}

function formatTaskLog(array $row): array
{
    return [
        'id' => (int)$row['id'],
        'bot_id' => $row['bot_id'] !== null ? (int)$row['bot_id'] : null,
        'action' => formatTaskLogAction((string)$row['action']),
        'response_code' => $row['response_code'] !== null ? (int)$row['response_code'] : null,
        'success' => (bool)$row['success'],
        'error_code' => $row['error_code'],
        'error_message' => $row['error_message'],
        'created_at' => $row['created_at']
    ];
}

function formatTaskLogAction(string $action): string
{
    $labels = [
        'kfcheck' => 'Task check',
        'post_succeeded' => 'Delivery accepted',
        'post_failed' => 'Delivery failed',
    ];

    return $labels[$action] ?? $action;
}

function tableExists(string $table): bool
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

function columnExists(string $table, string $column): bool
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
