<?php
/**
 * API Endpoint: POST /api/push
 * Function: Publish/update a kungfu skill (-1 credit for new publish)
 */

require_once __DIR__ . '/../core/Auth.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/RateLimiter.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/KungfuManageService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

$config = require __DIR__ . '/../config/config.php';

if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$input = json_decode(file_get_contents('php://input'), true);

if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
    Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
}

try {
    $bot = Auth::requireAuth();
    $botId = $bot['id'];
    if (!RateLimiter::checkApi($botId, 'push')) {
        $details = RateLimiter::checkApiWithDetails($botId, 'push');
        Response::rateLimit($details['retry_after'], $details['limit'], $details['window']);
    }
    $result = KungfuManageService::push((int)$botId, $input, $config);
    Response::success($result, $result['action'] === 'created' ? 'Kungfu published successfully' : 'Kungfu updated successfully');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Push failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during publishing');
}
