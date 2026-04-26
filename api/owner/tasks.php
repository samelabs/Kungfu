<?php
/**
 * API Endpoint: /api/owner/tasks
 * Function: Owner task creation and management.
 */

require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/TaskUtils.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/OwnerTaskService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

$config = require __DIR__ . '/../../config/config.php';

try {
    $bot = OwnerSession::require();
    $botId = (int)$bot['id'];
    $code = isset($_GET['code']) && $_GET['code'] !== '' ? TaskUtils::validateCode($_GET['code']) : '';
    $action = isset($_GET['action']) ? trim((string)$_GET['action']) : '';
    OwnerTaskService::ensureTaskSchema();

    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        if ($code === '') {
            Response::success(OwnerTaskService::listTasks($botId));
        }
        Response::success(OwnerTaskService::getTask($botId, $code));
    }

    if ($_SERVER['REQUEST_METHOD'] === 'POST') {
        $input = json_decode(file_get_contents('php://input'), true);
        if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
            Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON object');
        }

        if ($code === '') {
            Response::success(OwnerTaskService::createTask($botId, $config, $input), 'Task created');
        }

        if ($action === 'open') {
            Response::success(OwnerTaskService::setTaskStatus($botId, $code, 'open'), 'Task opened');
        }
        if ($action === 'close') {
            Response::success(OwnerTaskService::setTaskStatus($botId, $code, 'closed'), 'Task closed');
        }
        if ($action === 'add-budget') {
            Response::success(OwnerTaskService::addTaskBudget($botId, $code, $input), 'Budget added');
        }
        if ($action === 'refund') {
            Response::success(OwnerTaskService::refundTaskBudget($botId, $code), 'Budget refunded');
        }
        if ($action === 'edit') {
            Response::success(OwnerTaskService::updateTaskBasics($botId, $code, $config, $input), 'Task updated');
        }

        Response::error(400, 'INVALID_ACTION', 'Invalid task action');
    }

    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET and POST requests allowed');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Owner task API failed: ' . $e->getMessage(), [
        'bot_id' => $botId,
        'code' => $code ?? '',
        'action' => $action ?? ''
    ]);
    Response::error(500, 'INTERNAL_ERROR', 'Owner task operation failed');
}
