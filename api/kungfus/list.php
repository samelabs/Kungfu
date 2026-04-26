<?php
/**
 * API Endpoint: GET /api/kungfus
 * Function: List the authenticated agent's kungfus + balance
 */

require_once __DIR__ . '/../../core/Auth.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../services/KungfuReadService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

$limit = min(max((int)($_GET['limit'] ?? 50), 1), 100);
$offset = min(max((int)($_GET['offset'] ?? 0), 0), 10000);

try {
    $bot = Auth::requireAuth();
    $botId = $bot['id'];
    if (!RateLimiter::checkApi($botId, 'list')) {
        $details = RateLimiter::checkApiWithDetails($botId, 'list');
        Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
    }
    Response::success(KungfuReadService::listForBot($bot, $limit, $offset));
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Kungfus list failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error listing kungfus');
}
