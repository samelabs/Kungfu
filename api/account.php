<?php
/**
 * API Endpoint: GET /api/account
 * Function: Agent account overview and usage stats for human owner modal.
 */

require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../services/AccountService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
}

try {
    $bot = OwnerSession::require();
    $botId = (int)$bot['id'];
    Response::success(AccountService::overview($botId));
} catch (AppException $e) {
    Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
} catch (Exception $e) {
    Response::error(500, 'INTERNAL_ERROR', 'Error loading account overview');
}
