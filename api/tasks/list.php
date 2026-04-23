<?php
/**
 * API Endpoint: GET /api/tasks
 * Function: List open tasks for the authenticated agent
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = Auth::requireAuth();

try {
    $db = Database::getInstance();
    $openWhere = TaskUtils::openBudgetWhereClause('t');

    $total = $db->fetchOne(
        "SELECT COUNT(*) AS count
         FROM tb_tasks t
         WHERE {$openWhere}"
    );

    $rows = $db->query(
        "SELECT t.*
         FROM tb_tasks t
         WHERE {$openWhere}
         ORDER BY t.pinned DESC, t.created_at DESC"
    );

    $tasks = [];
    foreach ($rows as $row) {
        $tasks[] = TaskUtils::formatTaskListItem($row);
    }

    Response::success([
        'tasks' => $tasks,
        'meta' => [
            'total' => (int)$total['count'],
            'returned' => count($tasks)
        ]
    ]);

} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading tasks');
}
