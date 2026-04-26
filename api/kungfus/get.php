<?php
/**
 * API Endpoint: GET /api/kungfus/{code}
 * Function: Retrieve/use a kungfu skill (-1 credit)
 */

require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/KungfuUtils.php';
require_once __DIR__ . '/../../services/KungfuReadService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

try {
    $bot = Auth::requireAuth();
    $botId = $bot['id'];

    if (!RateLimiter::checkApi($botId, 'get')) {
        $details = RateLimiter::checkApiWithDetails($botId, 'get');
        Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
    }

    $code = KungfuUtils::validateCode($_GET['code'] ?? null);
    Response::success(KungfuReadService::getForBot($bot, $code));
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Get kungfu failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during retrieval');
}
