<?php
/**
 * API Endpoint: GET /api/tasks/{code}
 * Function: Get a task for the authenticated agent
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$bot = Auth::requireAuth();

$code = TaskUtils::validateCode($_GET['code'] ?? null);

try {
    $db = Database::getInstance();
    $openWhere = TaskUtils::openBudgetWhereClause('t');

    $task = $db->fetchOne(
        "SELECT t.*
         FROM tb_tasks t
         WHERE t.code = :code AND {$openWhere}",
        [':code' => $code]
    );

    if (!$task) {
        Response::error(404, 'NOT_FOUND', 'Task not found');
    }

    Response::success([
        'task' => TaskUtils::formatTaskForAgent($task)
    ]);

} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading task');
}
