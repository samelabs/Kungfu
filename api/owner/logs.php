<?php
/**
 * API Endpoint: GET /api/owner/logs
 * Function: Owner-facing log views (credits, agent logs, task logs) with pagination.
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = OwnerSession::require();
$botId = (int)$bot['id'];
$type = strtolower(trim((string)($_GET['type'] ?? 'credits')));
$allowedTypes = ['credits', 'agent', 'task'];
if (!in_array($type, $allowedTypes, true)) {
    Response::error(400, 'INVALID_TYPE', 'Type must be one of: credits, agent, task');
}

$page = (int)($_GET['page'] ?? 1);
if ($page < 1) {
    $page = 1;
}
$pageSize = (int)($_GET['page_size'] ?? 20);
if ($pageSize < 1) {
    $pageSize = 20;
}
$pageSize = min($pageSize, 100);
$offset = ($page - 1) * $pageSize;

$taskCode = trim((string)($_GET['task_code'] ?? ''));
if ($taskCode !== '') {
    $taskCode = TaskUtils::validateCode($taskCode);
}

try {
    ensureLogSchema($type);
    $db = Database::getInstance();

    if ($type === 'credits') {
        $balanceRow = $db->fetchOne(
            "SELECT balance FROM tb_bots WHERE id = :id",
            [':id' => $botId]
        );
        $countRow = $db->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_transactions
             WHERE bot_id = :bot_id",
            [':bot_id' => $botId]
        );
        $total = (int)($countRow['total'] ?? 0);
        $rows = $db->query(
            "SELECT id, type, amount, balance_after, ref_type, ref_id, created_at
             FROM tb_transactions
             WHERE bot_id = :bot_id
             ORDER BY id DESC
             LIMIT {$pageSize} OFFSET {$offset}",
            [':bot_id' => $botId]
        );

        Response::success([
            'type' => $type,
            'balance' => (float)($balanceRow['balance'] ?? 0),
            'items' => array_map('formatCreditLogRow', $rows),
            'pagination' => pagination($page, $pageSize, $total)
        ]);
    }

    if ($type === 'agent') {
        $countRow = $db->fetchOne(
            "SELECT COUNT(*) AS total
             FROM tb_logs
             WHERE bot_id = :bot_id",
            [':bot_id' => $botId]
        );
        $total = (int)($countRow['total'] ?? 0);
        $rows = $db->query(
            "SELECT id, action, target_type, target_id, ip_address, user_agent, request_data,
                    success, error_code, error_msg, created_at
             FROM tb_logs
             WHERE bot_id = :bot_id
             ORDER BY id DESC
             LIMIT {$pageSize} OFFSET {$offset}",
            [':bot_id' => $botId]
        );

        Response::success([
            'type' => $type,
            'items' => array_map('formatAgentLogRow', $rows),
            'pagination' => pagination($page, $pageSize, $total)
        ]);
    }

    $tasks = $db->query(
        "SELECT code, title
         FROM tb_tasks
         WHERE bot_id = :bot_id
         ORDER BY created_at DESC",
        [':bot_id' => $botId]
    );

    $params = [':bot_id' => $botId];
    $taskWhere = '';
    if ($taskCode !== '') {
        $taskWhere = ' AND l.task_code = :task_code';
        $params[':task_code'] = $taskCode;
    }

    $countRow = $db->fetchOne(
        "SELECT COUNT(*) AS total
         FROM tb_task_logs l
         INNER JOIN tb_tasks t ON t.code = l.task_code
         WHERE t.bot_id = :bot_id{$taskWhere}",
        $params
    );
    $total = (int)($countRow['total'] ?? 0);

    $rows = $db->query(
        "SELECT l.id, l.task_code, l.bot_id, l.action, l.payload_json, l.response_code,
                l.response_body, l.success, l.error_code, l.error_message, l.created_at
         FROM tb_task_logs l
         INNER JOIN tb_tasks t ON t.code = l.task_code
         WHERE t.bot_id = :bot_id{$taskWhere}
         ORDER BY l.id DESC
         LIMIT {$pageSize} OFFSET {$offset}",
        $params
    );

    Response::success([
        'type' => $type,
        'task_filter' => $taskCode !== '' ? $taskCode : null,
        'tasks' => array_map(static function (array $task): array {
            return [
                'code' => $task['code'],
                'title' => $task['title']
            ];
        }, $tasks),
        'items' => array_map('formatTaskLogRow', $rows),
        'pagination' => pagination($page, $pageSize, $total)
    ]);
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading logs');
}

function ensureLogSchema(string $type): void
{
    $required = ['tb_bots'];
    if ($type === 'credits') {
        $required[] = 'tb_transactions';
    } elseif ($type === 'agent') {
        $required[] = 'tb_logs';
    } else {
        $required[] = 'tb_tasks';
        $required[] = 'tb_task_logs';
    }

    $missing = [];
    foreach ($required as $table) {
        if (!tableExists($table)) {
            $missing[] = $table;
        }
    }
    if (!empty($missing)) {
        Response::error(503, 'LOGS_NOT_READY', 'Logs are not ready yet.');
    }
}

function tableExists(string $table): bool
{
    static $cache = [];
    if (array_key_exists($table, $cache)) {
        return $cache[$table];
    }

    $db = Database::getInstance();
    $row = $db->fetchOne(
        "SELECT 1
         FROM information_schema.tables
         WHERE table_schema = DATABASE() AND table_name = :table
         LIMIT 1",
        [':table' => $table]
    );
    $cache[$table] = (bool)$row;
    return $cache[$table];
}

function pagination(int $page, int $pageSize, int $total): array
{
    return [
        'page' => $page,
        'page_size' => $pageSize,
        'total' => $total,
        'total_pages' => $total > 0 ? (int)ceil($total / $pageSize) : 1
    ];
}

function formatCreditLogRow(array $row): array
{
    return [
        'id' => (int)$row['id'],
        'type' => $row['type'],
        'amount' => (float)$row['amount'],
        'balance_after' => (float)$row['balance_after'],
        'ref_type' => $row['ref_type'],
        'ref_id' => $row['ref_id'],
        'created_at' => $row['created_at']
    ];
}

function formatAgentLogRow(array $row): array
{
    $requestData = null;
    if (!empty($row['request_data'])) {
        $decoded = json_decode((string)$row['request_data'], true);
        if (is_array($decoded)) {
            $requestData = $decoded;
        }
    }

    return [
        'id' => (int)$row['id'],
        'action' => $row['action'],
        'target_type' => $row['target_type'],
        'target_id' => $row['target_id'],
        'ip_address' => $row['ip_address'],
        'user_agent' => $row['user_agent'],
        'request_data' => $requestData,
        'success' => (bool)$row['success'],
        'error_code' => $row['error_code'],
        'error_msg' => $row['error_msg'],
        'created_at' => $row['created_at']
    ];
}

function formatTaskLogRow(array $row): array
{
    $payload = null;
    if (!empty($row['payload_json'])) {
        $decoded = json_decode((string)$row['payload_json'], true);
        if (is_array($decoded)) {
            $payload = $decoded;
        }
    }

    return [
        'id' => (int)$row['id'],
        'task_code' => $row['task_code'],
        'bot_id' => $row['bot_id'] !== null ? (int)$row['bot_id'] : null,
        'action' => formatTaskLogAction((string)$row['action']),
        'payload_json' => $payload,
        'response_code' => $row['response_code'] !== null ? (int)$row['response_code'] : null,
        'response_body' => $row['response_body'],
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
