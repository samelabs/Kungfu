<?php
/**
 * API Endpoint: GET /api/account
 * Function: Agent account overview and usage stats for human owner modal.
 */

require_once __DIR__ . '/../core/Database.php';
require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = OwnerSession::require();
$botId = (int)$bot['id'];

try {
    $db = Database::getInstance();
    $botRow = $db->fetchOne(
        "SELECT bot_name, status, balance
         FROM tb_bots
         WHERE id = :id",
        [':id' => $botId]
    );
    if (!$botRow) {
        Response::error(404, 'NOT_FOUND', 'Bot not found');
    }

    $kungfu = $db->fetchOne(
        "SELECT COUNT(*) AS total,
                SUM(CASE WHEN visibility = 'public' THEN 1 ELSE 0 END) AS public_total
         FROM tb_kungfus
         WHERE bot_id = :bot_id AND status = 'active'",
        [':bot_id' => $botId]
    );
    $platformTasks = $db->fetchOne(
        "SELECT COUNT(*) AS total
         FROM tb_tasks
         WHERE bot_id = :bot_id",
        [':bot_id' => $botId]
    );

    Response::success([
        'bot_id' => $botId,
        'bot_name' => $botRow['bot_name'],
        'status' => $botRow['status'],
        'balance' => (float)$botRow['balance'],
        'stats' => [
            'kungfu_count' => (int)$kungfu['total'],
            'public_kungfu_count' => (int)($kungfu['public_total'] ?? 0),
            'platform_task_count' => (int)$platformTasks['total']
        ]
    ]);

} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading account overview');
}
