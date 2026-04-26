<?php
/**
 * API Endpoint: GET /api/key
 * Function: Human owner retrieves current agent key using owner session.
 */

require_once __DIR__ . '/../core/OwnerSession.php';
require_once __DIR__ . '/../core/Response.php';
require_once __DIR__ . '/../core/Logger.php';
require_once __DIR__ . '/../services/KeyService.php';
require_once __DIR__ . '/../exceptions/AppException.php';

if ($_SERVER['REQUEST_METHOD'] === 'GET') {
    try {
        $sessionBot = OwnerSession::require();
        Response::success(KeyService::currentOwnerKey((int)$sessionBot['id']), 'Key retrieved');
    } catch (AppException $e) {
        Response::error($e->getHttpCode(), $e->getErrorCode(), $e->getMessage(), $e->getDetails());
    } catch (Exception $e) {
        Logger::fileLog('ERROR', 'Key get failed: ' . $e->getMessage());
        Response::error(500, 'INTERNAL_ERROR', 'Error occurred while retrieving key');
    }
}

Response::error(405, 'METHOD_NOT_ALLOWED', 'Only GET requests allowed');
