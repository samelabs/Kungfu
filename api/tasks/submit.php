<?php
/**
 * API Endpoint: POST /api/tasks/{code}/submissions
 * Function: Submit work to a platform task
 */

require_once __DIR__ . '/../../core/Database.php';
require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../core/TaskUtils.php';
require_once __DIR__ . '/../../core/TaskSubmissionService.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../exceptions/AppException.php';
require_once __DIR__ . '/../../repositories/TaskRepository.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $botId = (int)$bot['id'];
    if (!RateLimiter::checkApi($botId, 'task_submit')) {
        $details = RateLimiter::checkApiWithDetails($botId, 'task_submit');
        Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
    }

    $code = TaskUtils::validateCode($_GET['code'] ?? null);

    $input = json_decode(file_get_contents('php://input'), true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
        Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON object');
    }

    $task = TaskRepository::findTaskByCode($code);

    if (!$task) {
        Response::error(404, 'NOT_FOUND', 'Task not found');
    }
    if ($task['status'] !== 'open') {
        Response::error(409, 'TASK_NOT_OPEN', 'Task is not open for submissions');
    }

    $result = TaskSubmissionService::submit($task, $botId, $input);
    Response::success($result, 'Task submission delivered');

} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Task submit failed: ' . $e->getMessage(), [
        'task_code' => $code ?? '',
        'bot_id' => $botId ?? null
    ]);
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during task submission');
}
