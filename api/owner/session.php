<?php
/**
 * API Endpoint: /api/owner/session
 * Function: Owner center session login, status, and logout.
 */

require_once __DIR__ . '/../../core/OwnerSession.php';
require_once __DIR__ . '/../../core/Response.php';
require_once __DIR__ . '/../../core/Logger.php';
require_once __DIR__ . '/../../core/RateLimiter.php';
require_once __DIR__ . '/../../services/OwnerSessionService.php';
require_once __DIR__ . '/../../exceptions/AppException.php';

try {
    if ($_SERVER['REQUEST_METHOD'] === 'GET') {
        Response::success(OwnerSessionService::current(), 'Owner session active');
    }

    if ($_SERVER['REQUEST_METHOD'] === 'POST') {
        $input = json_decode(file_get_contents('php://input'), true);
        if (json_last_error() !== JSON_ERROR_NONE || !is_array($input)) {
            Response::error(400, 'INVALID_JSON', 'Request body must be valid JSON');
        }

        if (empty($input['name']) || empty($input['password'])) {
            Response::error(400, 'MISSING_FIELD', 'Missing required fields: name, password');
        }

        $ip = Logger::getClientIp();
        $rateCheck = RateLimiter::checkOwnerLoginWithRetry($ip);
        if (!$rateCheck['allowed']) {
            Response::rateLimit($rateCheck['retry_after'], $rateCheck['limit'], $rateCheck['window']);
        }

        Response::success(
            OwnerSessionService::login((string)$input['name'], (string)$input['password']),
            'Owner login successful'
        );
    }

    if ($_SERVER['REQUEST_METHOD'] === 'DELETE') {
        Response::success(OwnerSessionService::logout(), 'Owner logout successful');
    }

    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET, POST, and DELETE requests allowed');
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Logger::fileLog('ERROR', 'Owner session API failed: ' . $e->getMessage(), [
        'method' => $_SERVER['REQUEST_METHOD'] ?? '',
        'path' => $_SERVER['REQUEST_URI'] ?? ''
    ]);
    Response::error(500, 'INTERNAL_ERROR', 'Owner session operation failed');
}
