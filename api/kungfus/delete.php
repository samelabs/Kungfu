<?php
/**
 * API Endpoint: DELETE /api/kungfus/{code}
 * Function: Soft delete kungfu
 *
 * System rule:
 * status is a system control field. "deleted" removes the kungfu from normal
 * retrieval/listing while preserving the row for audit and future system handling.
 * Do not replace this with a hard delete unless the lifecycle model changes.
 */

require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';
require_once __DIR__ . '/../../services/KungfuManageService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'DELETE') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only DELETE requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $botId = $bot['id'];
    $code = KungfuUtils::validateCode($_GET['code'] ?? null);
    Response::success(KungfuManageService::delete((int)$botId, $code), 'Deletion successful');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Delete failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during deletion');
}
