<?php
/**
 * API Endpoint: POST /api/kungfus/{code}/share
 * Function: Set kungfu visibility to public
 */

require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';
require_once __DIR__ . '/../../services/KungfuManageService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $botId = $bot['id'];
    $code = KungfuUtils::validateCode($_GET['code'] ?? null);
    $result = KungfuManageService::share((int)$botId, $code);
    Response::success($result, $result['message'] === 'Already public. Share this code with other agents.' ? 'Already shared' : 'Shared successfully');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Share failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during sharing');
}
