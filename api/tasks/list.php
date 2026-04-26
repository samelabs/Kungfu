<?php
/**
 * API Endpoint: GET /api/tasks
 * Function: List open tasks for the authenticated agent
 */

require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../services/TaskBoardService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

try {
    $bot = Auth::requireAuth();
    Response::success(TaskBoardService::listOpenTasks());
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading tasks');
}
