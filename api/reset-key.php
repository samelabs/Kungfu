<?php
/**
 * API Endpoint: POST /api/reset-key
 * Function: Reset agent API Key using owner session only.
 */

require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/ResetKeyService.php';
require_once __DIR__ . '/../exceptions/AppException.php';
require_once __DIR__ . '/../exceptions/RateLimitException.php';

// Only POST requests allowed
if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only POST requests allowed');
}

$rawInput = file_get_contents('php://input');
$input = [];
if ($rawInput !== false && trim($rawInput) !== '') {
    $input = json_decode($rawInput, true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
        Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
    }
}

try {
    $sessionBot = OwnerSession::require();
    $botId = (int)$sessionBot['id'];
    Response::success(
        ResetKeyService::reset($botId, (string)($input['current_key'] ?? '')),
        'Key reset successful'
    );
} catch (RateLimitException $e) {
    Response::rateLimit($e->getRetryAfter(), $e->getLimit(), $e->getWindow());
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Reset key failed: ' . $e->getMessage());
    Response::error(500, 'INTERNAL_ERROR', 'Error occurred during Key reset');
}
