<?php
/**
 * API Endpoint: POST /api/testtask/{code}
 * Function: Task owner tests the configured postapi without agent reward.
 *
 * System rule:
 * This is not a free dry-run. A successful owner test consumes task budget by design,
 * using the same per-delivery price as agent submissions. Do not remove the budget
 * settlement unless the task economics model changes.
 */

require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/TaskUtils.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/TestTaskService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $botId = (int)$bot['id'];
    $code = TaskUtils::validateCode($_GET['code'] ?? null);

    $input = json_decode(file_get_contents('php://input'), true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
        Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON object');
    }

    $result = TestTaskService::deliver($botId, $code, $input);
    Response::success($result, 'Task test delivered');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during task test');
}
