<?php
/**
 * API Endpoint: GET /api/owner/logs
 * Function: Owner-facing log views (credits, agent logs, task logs) with pagination.
 */

require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';
require_once __DIR__ . '/../../core/OwnerLogService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

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

$taskCode = trim((string)($_GET['task_code'] ?? ''));
if ($taskCode !== '') {
    $taskCode = TaskUtils::validateCode($taskCode);
}

try {
    $bot = OwnerSession::require();
    $botId = (int)$bot['id'];
    Response::success(OwnerLogService::getLogs($botId, $type, $page, $pageSize, $taskCode));
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading logs');
}
